package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

type indexRetentionPolicy struct {
	Enabled       bool `json:"enabled"`
	PruneOriginal bool `json:"prune_original"`
}
type indexRetentionRecord struct {
	Generation indexGeneration   `json:"generation"`
	Coverage   *SemanticCoverage `json:"coverage,omitempty"`
	ConfigSHA  string            `json:"config_sha256"`
	Pruning    bool              `json:"pruning,omitempty"`
	PrunedAt   string            `json:"pruned_at,omitempty"`
}
type indexRetentionStatus struct {
	Enabled             bool   `json:"enabled"`
	PreviousCatalogs    int    `json:"previous_catalogs"`
	State               string `json:"state"`
	Updated             string `json:"updated,omitempty"`
	PrunedGenerations   int    `json:"pruned_generations"`
	ProtectedCandidates int    `json:"protected_candidates"`
	Error               string `json:"error,omitempty"`
}

func retentionPolicy() indexRetentionPolicy {
	return indexRetentionPolicy{Enabled: os.Getenv("RESEARCH_INDEX_RETENTION") == "true", PruneOriginal: os.Getenv("RESEARCH_INDEX_RETENTION_PRUNE_ORIGINAL") == "true"}
}
func (m *indexManager) retentionRecordPath(id string) string {
	return filepath.Join(m.root, "retention", "generations", id+".json")
}
func (m *indexManager) retentionPublicStatus() indexRetentionStatus {
	s := indexRetentionStatus{Enabled: retentionPolicy().Enabled, PreviousCatalogs: 1, State: "disabled"}
	if s.Enabled {
		s.State = "waiting"
		_ = readIndexJSON(filepath.Join(m.root, "retention", "status.json"), &s)
		s.Enabled = true
		s.PreviousCatalogs = 1
	}
	return s
}

// Register exact application-owned generation identities, never a collection
// prefix or a caller-supplied filesystem deletion target.
func (m *indexManager) rememberGeneration(g indexGeneration, configSHA string) error {
	if g.ID != "original" && !deskID.MatchString(g.ID) {
		return errors.New("invalid retention generation")
	}
	if !shaPattern.MatchString(g.SHA) {
		return errors.New("retention generation lacks a checksum")
	}
	original := os.Getenv("RESEARCH_INDEX_ORIGINAL_CATALOG")
	if g.ID == "original" {
		if g.Catalog != original || !filepath.IsAbs(original) || g.Semantic != "" && filepath.Dir(g.Semantic) != filepath.Dir(original) {
			return errors.New("original retention paths changed")
		}
	} else if g.Catalog != m.runPath(g.ID, "catalog.sqlite") || g.Semantic != "" && g.Semantic != m.runPath(g.ID, "semantic.json") {
		return errors.New("managed retention paths changed")
	}
	path := m.retentionRecordPath(g.ID)
	var old indexRetentionRecord
	if e := readIndexJSON(path, &old); e == nil {
		if old.Generation != g || old.ConfigSHA != configSHA {
			return errors.New("retention identity changed")
		}
		return nil
	} else if !os.IsNotExist(e) {
		return e
	}
	r := indexRetentionRecord{Generation: g, ConfigSHA: configSHA}
	if g.Semantic != "" {
		var c SemanticCoverage
		if readIndexJSON(g.Semantic, &c) != nil || c.CatalogSHA != g.SHA || c.ModelSHA != g.ModelSHA || c.Collection != semanticCollection(g.SHA, g.ModelSHA) || c.Algorithm != semanticAlgorithm || c.Dimensions != semanticDimensions {
			return errors.New("retention vector identity changed")
		}
		expected, _ := json.Marshal(semanticEngine())
		actual, _ := json.Marshal(c.Engine)
		if string(expected) != string(actual) {
			return errors.New("retention engine identity changed")
		}
		r.Coverage = &c
	}
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	return writeIndexJSON(path, r)
}
func (m *indexManager) rememberSwitch(a indexActive) error {
	if !retentionPolicy().Enabled {
		return nil
	}
	if e := m.rememberGeneration(a.Current, m.configSHA); e != nil {
		return e
	}
	if a.Previous != nil {
		return m.rememberGeneration(*a.Previous, m.configSHA)
	}
	return nil
}
func retentionRegular(path string) (int64, error) {
	s, e := os.Lstat(path)
	if os.IsNotExist(e) {
		return 0, nil
	}
	if e != nil {
		return 0, e
	}
	if !s.Mode().IsRegular() {
		return 0, errors.New("retention refuses non-regular files")
	}
	return s.Size(), nil
}
func retentionDirectory(path, base string) error {
	rel, e := filepath.Rel(base, path)
	if e != nil || rel == ".." || len(rel) > 3 && rel[:3] == ".."+string(os.PathSeparator) {
		return errors.New("retention path escaped its owner")
	}
	for p := path; ; p = filepath.Dir(p) {
		s, e := os.Lstat(p)
		if e != nil || !s.IsDir() || s.Mode()&os.ModeSymlink != 0 {
			return errors.New("retention refuses linked directories")
		}
		if p == base {
			return nil
		}
	}
}
func (m *indexManager) retentionStep(ctx context.Context) error {
	policy := retentionPolicy()
	if !policy.Enabled {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	lock, e := indexLock(filepath.Join(m.root, "control.lock"))
	if e != nil {
		return nil
	}
	defer indexUnlock(lock)
	if m.reloading.Load() {
		return nil
	}
	s := indexRetentionStatus{Enabled: true, PreviousCatalogs: 1, State: "current-plus-one-backup", Updated: indexTime()}
	statusPath := filepath.Join(m.root, "retention", "status.json")
	if e = os.MkdirAll(filepath.Dir(statusPath), 0700); e != nil {
		return e
	}
	fail := func() error {
		s.State = "needs-attention"
		s.Error = "Catalog cleanup requires operator review. Protected catalogs and sources stay saved."
		return writeIndexJSON(statusPath, s)
	}
	var active indexActive
	if e = readIndexJSON(filepath.Join(m.root, "active.json"), &active); e != nil {
		return fail()
	}
	// A stale process cannot prune around a generation it has not opened.
	if active.Current.ID != m.base.ID || active.Current.SHA != m.base.SHA {
		return nil
	}
	if e = m.rememberSwitch(active); e != nil {
		return fail()
	}
	protectedIDs := map[string]bool{}
	protectedCollections := map[string]bool{}
	protect := func(g indexGeneration) {
		protectedIDs[g.ID] = true
		if shaPattern.MatchString(g.SHA) {
			model := g.ModelSHA
			if model == "" {
				model = m.config.ModelSHA
			}
			if shaPattern.MatchString(model) {
				protectedCollections[semanticCollection(g.SHA, model)] = true
			}
		}
	}
	protect(active.Current)
	if active.Previous != nil {
		protect(*active.Previous)
	}
	states, e := m.states()
	if e != nil {
		return fail()
	}
	for _, state := range states {
		if state.PrunedAt != "" {
			continue
		}
		var run indexRun
		if readIndexJSON(m.runPath(state.ID, "request.json"), &run) != nil {
			return fail()
		}
		if state.Status != "complete" && state.Status != "cancelled" {
			// The existing active run may still have its legacy ready status.
			if state.ID != active.Current.ID && (active.Previous == nil || state.ID != active.Previous.ID) {
				s.ProtectedCandidates++
				protect(run.Base)
			}
			protectedIDs[state.ID] = true
			if state.Candidate != nil {
				protect(*state.Candidate)
			}
		}
		if state.Status == "complete" && state.Candidate != nil {
			if e = m.rememberGeneration(*state.Candidate, run.ConfigSHA); e != nil {
				return fail()
			}
		}
	}
	paths, e := filepath.Glob(filepath.Join(m.root, "retention", "generations", "*.json"))
	if e != nil {
		return fail()
	}
	for _, path := range paths {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var r indexRetentionRecord
		if readIndexJSON(path, &r) != nil {
			return fail()
		}
		id := r.Generation.ID
		if id != "original" && !deskID.MatchString(id) || path != m.retentionRecordPath(id) {
			return fail()
		}
		if r.PrunedAt != "" {
			// Complete the small state receipt after a crash during final bookkeeping.
			if id != "original" {
				var state indexState
				if readIndexJSON(m.runPath(id, "state.json"), &state) != nil {
					return fail()
				}
				if state.PrunedAt == "" {
					state.PrunedAt = r.PrunedAt
					if state.Status != "cancelled" {
						state.Status = "complete"
					}
					if e = m.saveState(&state); e != nil {
						return fail()
					}
				}
			}
			s.PrunedGenerations++
			continue
		}
		if protectedIDs[id] || id == "original" && !policy.PruneOriginal {
			continue
		}
		if r.ConfigSHA != m.configSHA {
			return fail()
		}
		g := r.Generation
		// Exact paths were checked on registration. Check again before unlinking.
		if id == "original" {
			if g.Catalog != os.Getenv("RESEARCH_INDEX_ORIGINAL_CATALOG") || g.Semantic != "" && filepath.Dir(g.Semantic) != filepath.Dir(g.Catalog) {
				return fail()
			}
		} else if g.Catalog != m.runPath(id, "catalog.sqlite") || g.Semantic != "" && g.Semantic != m.runPath(id, "semantic.json") {
			return fail()
		}
		files := []string{g.Catalog}
		if g.Semantic != "" {
			files = append(files, g.Semantic)
		}
		if id != "original" {
			files = append(files, m.runPath(id, "updates.sqlite"))
		}
		for _, file := range files {
			base := m.root
			if id == "original" {
				base = filepath.Dir(g.Catalog)
			}
			if e = retentionDirectory(filepath.Dir(file), base); e != nil {
				return fail()
			}
			if _, e = retentionRegular(file); e != nil {
				return fail()
			}
		}
		if _, e = os.Stat(g.Catalog); e == nil {
			sha, err := indexFileSHA(g.Catalog)
			if err != nil || sha != g.SHA {
				return fail()
			}
		} else if !r.Pruning || !os.IsNotExist(e) {
			return fail()
		}
		r.Pruning = true
		if e = writeIndexJSON(path, r); e != nil {
			return fail()
		}
		if r.Coverage != nil {
			c := r.Coverage
			if c.CatalogSHA != g.SHA || c.ModelSHA != g.ModelSHA || c.Collection != semanticCollection(g.SHA, g.ModelSHA) || c.Algorithm != semanticAlgorithm || c.Dimensions != semanticDimensions {
				return fail()
			}
			if !protectedCollections[c.Collection] {
				client := &semanticClient{milvusOrigin: m.config.VectorOrigin, client: &http.Client{Timeout: 60 * time.Second}}
				var has struct {
					Code int `json:"code"`
					Data struct {
						Has bool `json:"has"`
					} `json:"data"`
				}
				if e = client.call(ctx, client.milvusOrigin, "/v2/vectordb/collections/has", map[string]any{"collectionName": c.Collection}, &has); e != nil || has.Code != 0 {
					return fail()
				}
				if has.Data.Has {
					if e = indexMilvus(ctx, client, "/v2/vectordb/collections/drop", map[string]any{"collectionName": c.Collection}); e != nil {
						return fail()
					}
				}
			}
		}
		for _, file := range files {
			if e = os.Remove(file); e != nil && !os.IsNotExist(e) {
				return fail()
			}
		}
		for _, file := range files {
			dir, err := os.Open(filepath.Dir(file))
			if err != nil {
				return fail()
			}
			err = dir.Sync()
			dir.Close()
			if err != nil {
				return fail()
			}
		}
		r.PrunedAt = indexTime()
		r.Pruning = false
		if e = writeIndexJSON(path, r); e != nil {
			return fail()
		}
		if id != "original" {
			var state indexState
			if readIndexJSON(m.runPath(id, "state.json"), &state) != nil {
				return fail()
			}
			state.PrunedAt = r.PrunedAt
			if state.Status != "cancelled" {
				state.Status = "complete"
			}
			if e = m.saveState(&state); e != nil {
				return fail()
			}
		}
		s.PrunedGenerations++
	}
	return writeIndexJSON(statusPath, s)
}
func (m *indexManager) startRetention(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			if ctx.Err() != nil {
				return
			}
			_ = m.retentionStep(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return done
}
