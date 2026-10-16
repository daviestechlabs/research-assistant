package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func retentionFixture(t *testing.T) (*indexManager, map[string]indexGeneration, *[]string, *bool) {
	t.Helper()
	store := indexFixtureStore(t, map[string][]byte{})
	m, _ := indexFixtureManager(t, store, "jsonl", "unused.jsonl")
	t.Setenv("RESEARCH_INDEX_RETENTION", "true")
	t.Setenv("RESEARCH_INDEX_RETENTION_PRUNE_ORIGINAL", "true")
	drops := []string{}
	reject := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("chat credential reached retention")
		}
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		switch r.URL.Path {
		case "/v2/vectordb/collections/has":
			writeJSON(w, 200, map[string]any{"code": 0, "data": map[string]bool{"has": true}})
		case "/v2/vectordb/collections/drop":
			if reject {
				writeJSON(w, 200, map[string]any{"code": 1})
				return
			}
			col, ok := b["collectionName"].(string)
			if !ok || col == "" {
				t.Error("wrong drop request")
			}
			drops = append(drops, col)
			writeJSON(w, 200, map[string]any{"code": 0})
		default:
			t.Error("unexpected retention network operation", r.URL.Path)
			writeJSON(w, 500, map[string]any{})
		}
	}))
	t.Cleanup(server.Close)
	m.config.VectorOrigin = server.URL
	m.config.ModelSHA = strings.Repeat("a", 64)
	original := m.base
	original.SHA, _ = indexFileSHA(original.Catalog)
	if e := m.rememberGeneration(original, m.configSHA); e != nil {
		t.Fatal(e)
	}
	gens := map[string]indexGeneration{"original": original}
	for _, name := range []string{"1", "2", "3"} {
		id := strings.Repeat(name, 32)
		dir := m.runPath(id, "")
		os.MkdirAll(dir, 0700)
		path := m.runPath(id, "catalog.sqlite")
		d, e := openDatabase(path, true)
		if e != nil {
			t.Fatal(e)
		}
		d.initialize()
		doc, _ := normalize(Document{ID: "2610.00001", Source: "arxiv", Title: "Synthetic catalog " + name, Summary: "Supplied abstract " + name})
		d.put(doc)
		d.close()
		sha, _ := indexFileSHA(path)
		g := indexGeneration{ID: id, Catalog: path, SHA: sha, Semantic: m.runPath(id, "semantic.json"), ModelSHA: m.config.ModelSHA, Parent: "original", ParentSHA: original.SHA}
		coverage := SemanticCoverage{CatalogSHA: sha, ModelSHA: g.ModelSHA, Collection: semanticCollection(sha, g.ModelSHA), Algorithm: semanticAlgorithm, Dimensions: semanticDimensions, Engine: semanticEngine(), State: "complete", Total: 1, Indexed: 1, VerifiedCount: 1}
		writeIndexJSON(g.Semantic, coverage)
		writeIndexJSON(m.runPath(id, "request.json"), indexRun{ID: id, Kind: "arxiv-update", Base: original, ConfigSHA: m.configSHA})
		writeIndexJSON(m.runPath(id, "control.json"), map[string]string{"desired": "running"})
		m.saveState(&indexState{ID: id, Status: "complete", Phase: "validated", Candidate: &g})
		os.WriteFile(m.runPath(id, "updates.sqlite"), []byte("frozen metadata snapshot"), 0600)
		gens[name] = g
	}
	m.base = gens["3"]
	m.restart = func() {}
	b := gens["2"]
	writeIndexJSON(filepath.Join(m.root, "active.json"), indexActive{Current: m.base, Previous: &b})
	return m, gens, &drops, &reject
}
func requireFile(t *testing.T, path string, exists bool) {
	t.Helper()
	_, e := os.Stat(path)
	if exists && e != nil || !exists && !os.IsNotExist(e) {
		t.Fatalf("file preservation %s: %v", filepath.Base(path), e)
	}
}

func TestOneBackupRetentionPrunesOnlyRetiredArtifactsAndSupportsRollback(t *testing.T) {
	m, g, drops, _ := retentionFixture(t)
	for _, name := range []string{"saved-paper.pdf", "private-book.epub", "research-desk.sqlite", "model-weights.bin"} {
		os.WriteFile(filepath.Join(filepath.Dir(g["original"].Catalog), name), []byte("preserve"), 0600)
	}
	os.WriteFile(m.runPath(g["1"].ID, "unexpected.pdf"), []byte("preserve"), 0600)
	if e := m.retentionStep(context.Background()); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"original", "1"} {
		requireFile(t, g[name].Catalog, false)
	}
	for _, name := range []string{"2", "3"} {
		requireFile(t, g[name].Catalog, true)
		sha, _ := indexFileSHA(g[name].Catalog)
		if sha != g[name].SHA {
			t.Fatal("protected catalog changed")
		}
	}
	requireFile(t, m.runPath(g["1"].ID, "updates.sqlite"), false)
	requireFile(t, g["1"].Semantic, false)
	requireFile(t, m.runPath(g["1"].ID, "state.json"), true)
	requireFile(t, m.runPath(g["1"].ID, "unexpected.pdf"), true)
	for _, name := range []string{"saved-paper.pdf", "private-book.epub", "research-desk.sqlite", "model-weights.bin"} {
		requireFile(t, filepath.Join(filepath.Dir(g["original"].Catalog), name), true)
	}
	if len(*drops) != 1 || (*drops)[0] != semanticCollection(g["1"].SHA, g["1"].ModelSHA) {
		t.Fatal("wrong vector collection retired", *drops)
	}
	var state indexState
	readIndexJSON(m.runPath(g["1"].ID, "state.json"), &state)
	if state.Status != "complete" || state.PrunedAt == "" {
		t.Fatal("old worker could resume pruned work")
	}
	if e := m.activate("", true); e != nil {
		t.Fatal("remaining backup cannot restore", e)
	}
	var active indexActive
	readIndexJSON(filepath.Join(m.root, "active.json"), &active)
	if active.Current.ID != g["2"].ID || active.Previous.ID != g["3"].ID {
		t.Fatal("rollback identities changed")
	}
	m.base = active.Current
	m.reloading.Store(false)
	if e := m.retentionStep(context.Background()); e != nil {
		t.Fatal(e)
	}
	if len(*drops) != 1 {
		t.Fatal("prune retry repeated destructive work")
	}
}
func TestRetentionProtectsUnfinishedParentsAndSharedVectorCollections(t *testing.T) {
	for _, kind := range []string{"unfinished-parent", "shared-vectors", "control-lock", "original-opt-out"} {
		t.Run(kind, func(t *testing.T) {
			m, g, drops, _ := retentionFixture(t)
			switch kind {
			case "unfinished-parent":
				id := strings.Repeat("4", 32)
				os.MkdirAll(m.runPath(id, ""), 0700)
				writeIndexJSON(m.runPath(id, "request.json"), indexRun{ID: id, Kind: "arxiv-update", Base: g["1"], ConfigSHA: m.configSHA})
				m.saveState(&indexState{ID: id, Status: "paused", Phase: "meaning"})
			case "shared-vectors":
				old := g["1"]
				data, _ := os.ReadFile(g["3"].Catalog)
				os.WriteFile(old.Catalog, data, 0600)
				old.SHA = g["3"].SHA
				var coverage SemanticCoverage
				readIndexJSON(g["3"].Semantic, &coverage)
				writeIndexJSON(old.Semantic, coverage)
				m.saveState(&indexState{ID: old.ID, Status: "complete", Candidate: &old})
				g["1"] = old
			case "control-lock":
				lock, e := indexLock(filepath.Join(m.root, "control.lock"))
				if e != nil {
					t.Fatal(e)
				}
				defer indexUnlock(lock)
			case "original-opt-out":
				t.Setenv("RESEARCH_INDEX_RETENTION_PRUNE_ORIGINAL", "false")
			}
			if e := m.retentionStep(context.Background()); e != nil {
				t.Fatal(e)
			}
			if kind == "unfinished-parent" || kind == "control-lock" {
				requireFile(t, g["1"].Catalog, true)
				if len(*drops) != 0 {
					t.Fatal("protected vectors dropped")
				}
			}
			if kind == "shared-vectors" {
				requireFile(t, g["1"].Catalog, false)
				if len(*drops) != 0 {
					t.Fatal("shared live vector collection dropped")
				}
			}
			if kind == "original-opt-out" {
				requireFile(t, g["original"].Catalog, true)
			}
			requireFile(t, g["2"].Catalog, true)
			requireFile(t, g["3"].Catalog, true)
		})
	}
}
func TestRetentionRejectsChangedFilesLinksAndFailedVectorDrops(t *testing.T) {
	for _, kind := range []string{"changed-file", "symlink", "vector-failure", "disabled"} {
		t.Run(kind, func(t *testing.T) {
			m, g, drops, reject := retentionFixture(t)
			switch kind {
			case "changed-file":
				f, _ := os.OpenFile(g["1"].Catalog, os.O_APPEND|os.O_WRONLY, 0600)
				f.Write([]byte("changed"))
				f.Close()
			case "symlink":
				target := filepath.Join(t.TempDir(), "unrelated.sqlite")
				b, _ := os.ReadFile(g["1"].Catalog)
				os.WriteFile(target, b, 0600)
				os.Remove(g["1"].Catalog)
				os.Symlink(target, g["1"].Catalog)
			case "vector-failure":
				*reject = true
			case "disabled":
				t.Setenv("RESEARCH_INDEX_RETENTION", "false")
			}
			if e := m.retentionStep(context.Background()); e != nil {
				t.Fatal(e)
			}
			requireFile(t, g["1"].Catalog, true)
			requireFile(t, g["2"].Catalog, true)
			requireFile(t, g["3"].Catalog, true)
			if len(*drops) != 0 {
				t.Fatal("unsafe cleanup proceeded")
			}
			if kind != "disabled" && m.retentionPublicStatus().State != "needs-attention" {
				t.Fatal("cleanup failure hidden")
			}
			if kind == "vector-failure" {
				*reject = false
				if e := m.retentionStep(context.Background()); e != nil {
					t.Fatal(e)
				}
				requireFile(t, g["1"].Catalog, false)
			}
		})
	}
}
