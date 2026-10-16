package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

var errArxivUnchanged = errors.New("harvested searchable metadata is unchanged")
var errIndexJobsBusy = errors.New("finish or cancel research jobs before switching the catalog")

type arxivAutoPolicy struct {
	Enabled       bool `json:"enabled"`
	IntervalHours int  `json:"interval_hours"`
	MinFreeGiB    int  `json:"min_free_gib"`
}
type arxivAutoStatus struct {
	arxivAutoPolicy
	State         string `json:"state"`
	Updated       string `json:"updated,omitempty"`
	NextCheck     string `json:"next_check,omitempty"`
	RunID         string `json:"run_id,omitempty"`
	LastActivated string `json:"last_activated,omitempty"`
	Error         string `json:"error,omitempty"`
}

func arxivAutoConfig() (arxivAutoPolicy, error) {
	p := arxivAutoPolicy{Enabled: os.Getenv("RESEARCH_ARXIV_AUTO_MERGE") == "true", IntervalHours: 24, MinFreeGiB: 64}
	for _, v := range []struct {
		name     string
		target   *int
		min, max int
	}{
		{"RESEARCH_ARXIV_AUTO_MERGE_HOURS", &p.IntervalHours, 6, 168},
		{"RESEARCH_ARXIV_AUTO_MERGE_MIN_FREE_GIB", &p.MinFreeGiB, 8, 4096},
	} {
		if s := os.Getenv(v.name); s != "" {
			n, e := strconv.Atoi(s)
			if e != nil || n < v.min || n > v.max {
				return p, errors.New("invalid automatic arXiv merge limits")
			}
			*v.target = n
		}
	}
	if p.Enabled && (syncRoot() == "" || os.Getenv("RESEARCH_ARXIV_UPDATES_ENABLED") != "true") {
		return p, errors.New("automatic arXiv merging requires enabled harvesting and update support")
	}
	return p, nil
}

// Hash only searchable metadata and deletion receipts. PDF download progress,
// feed replay timestamps, and SQLite file layout do not create new candidates.
func arxivMetadataDigest(ctx context.Context, path string) (string, error) {
	d, e := openDatabase(path, false)
	if e != nil {
		return "", e
	}
	defer d.close()
	if e = d.exec("BEGIN"); e != nil {
		return "", e
	}
	defer d.exec("ROLLBACK")
	h := sha256.New()
	cursor := ""
	for {
		if e = ctx.Err(); e != nil {
			return "", e
		}
		rows, err := d.rows(`SELECT u.id,u.deleted,coalesce(d.fingerprint,'') FROM arxiv_updates u LEFT JOIN documents d ON u.id=d.id WHERE u.id>? ORDER BY u.id LIMIT 512`, cursor)
		if err != nil {
			return "", err
		}
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			b, _ := json.Marshal(r)
			h.Write(b)
			h.Write([]byte{'\n'})
		}
		cursor = rows[len(rows)-1][0]
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (m *indexManager) autoMergePublicStatus() *arxivAutoStatus {
	p, e := arxivAutoConfig()
	if e != nil {
		return &arxivAutoStatus{State: "configuration-error", Error: "Automatic update configuration needs operator review."}
	}
	s := arxivAutoStatus{arxivAutoPolicy: p, State: "disabled"}
	if p.Enabled {
		s.State = "waiting"
		_ = readIndexJSON(filepath.Join(m.root, "auto-merge.json"), &s)
		s.arxivAutoPolicy = p
		if _, e := os.Stat(filepath.Join(m.root, "auto-merge-error.json")); e == nil {
			s.State = "needs-attention"
			s.Error = "The automatic coordinator needs operator review. The current catalog stays active."
		}
	}
	return &s
}

func (m *indexManager) autoMergeStep(ctx context.Context, p arxivAutoPolicy, now time.Time) error {
	if !p.Enabled || ctx.Err() != nil {
		return ctx.Err()
	}
	lock, e := indexLock(filepath.Join(m.root, "auto-merge.lock"))
	if e != nil {
		return e
	}
	defer indexUnlock(lock)
	s := arxivAutoStatus{arxivAutoPolicy: p, State: "waiting"}
	path := filepath.Join(m.root, "auto-merge.json")
	if e = readIndexJSON(path, &s); e != nil && !os.IsNotExist(e) {
		return errors.New("automatic merge receipt needs operator review")
	}
	s.arxivAutoPolicy = p
	s.Updated = now.UTC().Format(time.RFC3339Nano)
	save := func(state, message string) error { s.State = state; s.Error = message; return writeIndexJSON(path, s) }
	states, e := m.states()
	if e != nil {
		return e
	}
	// Recover ownership after a crash between queue publication and saving this
	// coordinator receipt. Manual candidates are never adopted or activated.
	if s.RunID == "" {
		for _, state := range states {
			if state.Status == "complete" || state.Status == "cancelled" {
				continue
			}
			var run indexRun
			if readIndexJSON(m.runPath(state.ID, "request.json"), &run) != nil {
				return errors.New("update ownership receipt is invalid")
			}
			if run.Automatic {
				s.RunID = state.ID
				break
			}
		}
	}
	if s.RunID != "" {
		if !deskID.MatchString(s.RunID) {
			return errors.New("invalid automatic candidate identity")
		}
		var run indexRun
		var state indexState
		if readIndexJSON(m.runPath(s.RunID, "request.json"), &run) != nil || !run.Automatic || run.Kind != "arxiv-update" || readIndexJSON(m.runPath(s.RunID, "state.json"), &state) != nil {
			return errors.New("automatic candidate ownership needs operator review")
		}
		if m.base.ID == s.RunID {
			if state.Status != "complete" {
				state.Status = "complete"
				if e = m.saveState(&state); e != nil {
					return e
				}
			}
			s.LastActivated = now.UTC().Format(time.RFC3339Nano)
			s.NextCheck = now.Add(time.Duration(p.IntervalHours) * time.Hour).UTC().Format(time.RFC3339Nano)
			s.RunID = ""
			return save("up-to-date", "")
		}
		if state.Status == "failed" || state.Status == "paused" || state.Status == "cancelled" {
			return save("needs-attention", "The automatic candidate is stopped. Review it in Library; the current catalog stays active.")
		}
		if state.Status != "ready" {
			return save("building", "")
		}
		if run.Base.ID != m.base.ID || run.Base.SHA != m.base.SHA {
			return save("needs-attention", "The active parent changed. Review the automatic candidate before continuing.")
		}
		if s.State == "activation-failed" {
			return save(s.State, s.Error)
		}
		// Use the same admission lock as browser activation. Research job creation
		// cannot race the idle check while the catalog identity is validated.
		m.admission.Lock()
		if ctx.Err() != nil {
			m.admission.Unlock()
			return ctx.Err()
		}
		e = m.activate(s.RunID, false)
		m.admission.Unlock()
		if errors.Is(e, errIndexJobsBusy) {
			return save("waiting-for-research", "")
		}
		if e != nil {
			return save("activation-failed", "Automatic activation did not pass its checks. Review the candidate in Library.")
		}
		s.LastActivated = now.UTC().Format(time.RFC3339Nano)
		s.RunID = ""
		s.NextCheck = now.Add(time.Duration(p.IntervalHours) * time.Hour).UTC().Format(time.RFC3339Nano)
		return save("activated", "")
	}
	if next, e := time.Parse(time.RFC3339Nano, s.NextCheck); e == nil && now.Before(next) {
		return nil
	}
	for _, state := range states {
		if state.ID != m.base.ID && state.Status != "complete" && state.Status != "cancelled" {
			return save("waiting-for-indexing", "Another indexing run needs to finish or be reviewed first.")
		}
	}
	if updated, e := time.Parse(time.RFC3339Nano, indexHeartbeat(m.root)); e != nil || now.Sub(updated) > 90*time.Second || updated.After(now.Add(time.Minute)) {
		return save("waiting-for-worker", "")
	}
	var space syscall.Statfs_t
	if e = syscall.Statfs(m.root, &space); e != nil {
		return e
	}
	if uint64(space.Bavail)*uint64(space.Bsize) < uint64(p.MinFreeGiB)<<30 {
		return save("waiting-for-storage", "Free library storage is below the configured reserve.")
	}
	state, e := m.submitRun("arxiv-update", nil, true, true)
	if errors.Is(e, errArxivUnchanged) {
		s.NextCheck = now.Add(time.Duration(p.IntervalHours) * time.Hour).UTC().Format(time.RFC3339Nano)
		return save("up-to-date", "")
	}
	if e != nil {
		s.NextCheck = now.Add(15 * time.Minute).UTC().Format(time.RFC3339Nano)
		return save("queue-blocked", "An update could not be queued. The coordinator will retry without changing the current catalog.")
	}
	s.RunID = state.ID
	return save("building", "")
}

func (m *indexManager) startAutoMerge(ctx context.Context, p arxivAutoPolicy) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			if ctx.Err() != nil {
				return
			}
			path := filepath.Join(m.root, "auto-merge-error.json")
			if e := m.autoMergeStep(ctx, p, time.Now()); e != nil && ctx.Err() == nil {
				_ = writeIndexJSON(path, map[string]string{"updated": indexTime(), "error": "Automatic coordinator requires operator review."})
			} else if e == nil {
				_ = os.Remove(path)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return done
}
