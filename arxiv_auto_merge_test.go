package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func autoFixture(t *testing.T) (*indexManager, *arxivHarvester, arxivAutoPolicy, time.Time) {
	t.Helper()
	store := indexFixtureStore(t, map[string][]byte{})
	m, base := indexFixtureManager(t, store, "jsonl", "unused.jsonl")
	h, _ := syncFixture(t)
	t.Setenv("RESEARCH_ARXIV_SYNC_ROOT", h.root)
	t.Setenv("RESEARCH_ARXIV_UPDATES_ENABLED", "true")
	t.Setenv("RESEARCH_ARXIV_AUTO_MERGE", "true")
	m.config.ModelSHA = strings.Repeat("a", 64)
	m.base.ModelSHA = m.config.ModelSHA
	m.base.SHA, _ = indexFileSHA(base)
	m.base.Semantic = filepath.Join(h.root, "base-semantic.json")
	c := SemanticCoverage{CatalogSHA: m.base.SHA, ModelSHA: m.config.ModelSHA, State: "complete", Total: 1, Indexed: 1, VerifiedCount: 1, Algorithm: semanticAlgorithm, Dimensions: semanticDimensions, Collection: semanticCollection(m.base.SHA, m.config.ModelSHA), Engine: semanticEngine()}
	if e := writeIndexJSON(m.base.Semantic, c); e != nil {
		t.Fatal(e)
	}
	var rec arxivRecord
	decodeSyncFixture(oaiFixture("2610.00001", "", "New geology evidence."), &rec)
	if e := h.storeRecord(rec); e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC()
	writeIndexJSON(filepath.Join(m.root, "worker.json"), map[string]string{"updated": now.Format(time.RFC3339Nano)})
	m.restart = func() {}
	return m, h, arxivAutoPolicy{Enabled: true, IntervalHours: 24, MinFreeGiB: 8}, now
}
func readAuto(t *testing.T, m *indexManager) arxivAutoStatus {
	t.Helper()
	var s arxivAutoStatus
	if e := readIndexJSON(filepath.Join(m.root, "auto-merge.json"), &s); e != nil {
		t.Fatal(e)
	}
	return s
}

// Stand in for a worker that passed keyword validation and supplied a complete
// synthetic vector receipt. Separate worker tests exercise actual vector calls.
func readyAutoFixture(t *testing.T, m *indexManager, id string) indexState {
	t.Helper()
	var run indexRun
	var s indexState
	readIndexJSON(m.runPath(id, "request.json"), &run)
	readIndexJSON(m.runPath(id, "state.json"), &s)
	if e := m.execute(context.Background(), run, &s); e == nil || s.Phase != "meaning" {
		t.Fatal("fixture did not reach the embedding boundary", e)
	}
	c := SemanticCoverage{CatalogSHA: s.Candidate.SHA, ModelSHA: m.config.ModelSHA, State: "complete", Total: 2, Indexed: 2, VerifiedCount: 2, Algorithm: semanticAlgorithm, Dimensions: semanticDimensions, Collection: semanticCollection(s.Candidate.SHA, m.config.ModelSHA), Engine: semanticEngine()}
	s.Candidate.Semantic = m.runPath(id, "semantic.json")
	s.Candidate.ModelSHA = m.config.ModelSHA
	writeIndexJSON(s.Candidate.Semantic, c)
	s.Status = "ready"
	s.Phase = "validated"
	s.Vectors = 2
	s.Validation["verified_vectors"] = 2
	if e := m.saveState(&s); e != nil {
		t.Fatal(e)
	}
	return s
}
func TestAutomaticArxivQueueRecoveryIdleActivationAndNoOp(t *testing.T) {
	m, h, p, now := autoFixture(t)
	if e := m.autoMergeStep(context.Background(), p, now); e != nil {
		t.Fatal(e)
	}
	a := readAuto(t, m)
	if a.State != "building" || a.RunID == "" {
		t.Fatal(a)
	}
	var run indexRun
	readIndexJSON(m.runPath(a.RunID, "request.json"), &run)
	if !run.Automatic || run.UpdateSHA == "" || run.Kind != "arxiv-update" {
		t.Fatal("queue lacks persistent ownership")
	}
	if _, e := os.Stat(filepath.Join(m.root, "active.json")); !os.IsNotExist(e) {
		t.Fatal("unvalidated update activated")
	}
	// A restart recovers the already published queue without duplicating it.
	os.Remove(filepath.Join(m.root, "auto-merge.json"))
	if e := m.autoMergeStep(context.Background(), p, now); e != nil {
		t.Fatal(e)
	}
	if readAuto(t, m).RunID != a.RunID {
		t.Fatal("queued candidate lost during recovery")
	}
	ready := readyAutoFixture(t, m, a.RunID)
	d, e := openDatabase(filepath.Join(t.TempDir(), "jobs.sqlite"), true)
	if e != nil {
		t.Fatal(e)
	}
	defer d.close()
	d.exec("CREATE TABLE desk_jobs(status TEXT);INSERT INTO desk_jobs VALUES('running')")
	m.desk = &researchDesk{db: d}
	restarts := 0
	m.restart = func() { restarts++ }
	if e = m.autoMergeStep(context.Background(), p, now); e != nil {
		t.Fatal(e)
	}
	if readAuto(t, m).State != "waiting-for-research" || restarts != 0 {
		t.Fatal("active research interrupted")
	}
	d.exec("UPDATE desk_jobs SET status='complete'")
	if e = m.autoMergeStep(context.Background(), p, now); e != nil {
		t.Fatal(e)
	}
	var active indexActive
	readIndexJSON(filepath.Join(m.root, "active.json"), &active)
	if active.Current.ID != a.RunID || active.Previous == nil || active.Previous.ID != "original" || active.Current.UpdateSHA != run.UpdateSHA || restarts != 1 {
		t.Fatal("atomic activation or rollback identity lost")
	}
	var saved indexState
	readIndexJSON(m.runPath(a.RunID, "state.json"), &saved)
	if saved.Status != "complete" {
		t.Fatal("activated run remains pending")
	}
	// Simulate the reloaded app. A PDF-only change must not build another index.
	m.base = active.Current
	m.reloading.Store(false)
	if _, e = h.db.run("UPDATE arxiv_updates SET pdf_state='saved',sha256=?,bytes=100", strings.Repeat("b", 64)); e != nil {
		t.Fatal(e)
	}
	writeIndexJSON(filepath.Join(m.root, "worker.json"), map[string]string{"updated": now.Add(25 * time.Hour).Format(time.RFC3339Nano)})
	if e = m.autoMergeStep(context.Background(), p, now.Add(25*time.Hour)); e != nil {
		t.Fatal(e)
	}
	after := readAuto(t, m)
	states, _ := m.states()
	if after.State != "up-to-date" || len(states) != 1 || restarts != 1 || after.RunID != "" {
		t.Fatal("unchanged metadata rebuilt", after)
	}
	if ready.Candidate.SHA != active.Current.SHA {
		t.Fatal("candidate replaced during activation")
	}
}
func TestAutomaticArxivGuardsAndManualOwnership(t *testing.T) {
	for _, kind := range []string{"disabled", "stale-worker", "low-storage", "manual-candidate", "failed", "parent-changed", "bad-checksum"} {
		t.Run(kind, func(t *testing.T) {
			m, _, p, now := autoFixture(t)
			restarts := 0
			m.restart = func() { restarts++ }
			if kind == "disabled" {
				p.Enabled = false
			}
			if kind == "stale-worker" {
				os.Remove(filepath.Join(m.root, "worker.json"))
			}
			if kind == "low-storage" {
				p.MinFreeGiB = 4096
			}
			if kind == "manual-candidate" {
				if _, e := m.submit("arxiv-update", nil, true); e != nil {
					t.Fatal(e)
				}
			}
			if e := m.autoMergeStep(context.Background(), p, now); e != nil {
				t.Fatal(e)
			}
			states, _ := m.states()
			if kind == "failed" || kind == "parent-changed" || kind == "bad-checksum" {
				a := readAuto(t, m)
				if kind == "failed" {
					var s indexState
					readIndexJSON(m.runPath(a.RunID, "state.json"), &s)
					s.Status = "failed"
					m.saveState(&s)
				} else {
					s := readyAutoFixture(t, m, a.RunID)
					if kind == "parent-changed" {
						m.base.SHA = strings.Repeat("b", 64)
					} else {
						f, _ := os.OpenFile(s.Candidate.Catalog, os.O_APPEND|os.O_WRONLY, 0600)
						f.Write([]byte("changed"))
						f.Close()
					}
				}
				if e := m.autoMergeStep(context.Background(), p, now.Add(time.Minute)); e != nil {
					t.Fatal(e)
				}
				if readAuto(t, m).RunID != a.RunID {
					t.Fatal("blocked candidate replaced")
				}
			} else if kind != "manual-candidate" && len(states) != 0 {
				t.Fatal("guard still queued an index")
			}
			if _, e := os.Stat(filepath.Join(m.root, "active.json")); !os.IsNotExist(e) || restarts != 0 {
				t.Fatal("guard allowed activation")
			}
		})
	}
}
func TestArxivMetadataDigestAndAutomaticConfiguration(t *testing.T) {
	m, h, _, _ := autoFixture(t)
	ctx := context.Background()
	path := filepath.Join(h.root, "updates.sqlite")
	a, e := arxivMetadataDigest(ctx, path)
	if e != nil {
		t.Fatal(e)
	}
	h.db.exec("UPDATE arxiv_updates SET datestamp='2026-10-15',pdf_state='saved',attempts=3")
	b, e := arxivMetadataDigest(ctx, path)
	if e != nil || a != b {
		t.Fatal("download progress changed searchable metadata identity")
	}
	h.db.exec("UPDATE arxiv_updates SET deleted=1")
	c, e := arxivMetadataDigest(ctx, path)
	if e != nil || a == c {
		t.Fatal("deletion omitted from identity")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, e = arxivMetadataDigest(cancelled, path); !errors.Is(e, context.Canceled) {
		t.Fatal("cancellation ignored")
	}
	for _, value := range []string{"0", "5", "169", "invalid"} {
		t.Setenv("RESEARCH_ARXIV_AUTO_MERGE_HOURS", value)
		if _, e = arxivAutoConfig(); e == nil {
			t.Fatal("invalid interval accepted")
		}
	}
	t.Setenv("RESEARCH_ARXIV_AUTO_MERGE_HOURS", "24")
	t.Setenv("RESEARCH_ARXIV_UPDATES_ENABLED", "false")
	if _, e = arxivAutoConfig(); e == nil {
		t.Fatal("unsupported worker accepted")
	}
	if m.autoMergePublicStatus().State != "configuration-error" {
		t.Fatal("invalid configuration hidden")
	}
}

func TestAutomaticActivationSharesResearchAdmissionLock(t *testing.T) {
	m, _, p, now := autoFixture(t)
	if e := m.autoMergeStep(context.Background(), p, now); e != nil {
		t.Fatal(e)
	}
	id := readAuto(t, m).RunID
	readyAutoFixture(t, m, id)
	d, e := openDatabase(filepath.Join(t.TempDir(), "jobs.sqlite"), true)
	if e != nil {
		t.Fatal(e)
	}
	defer d.close()
	d.exec("CREATE TABLE desk_jobs(status TEXT)")
	m.desk = &researchDesk{db: d}
	m.admission.RLock()
	done := make(chan error, 1)
	go func() { done <- m.autoMergeStep(context.Background(), p, now) }()
	// A new job admitted before the switch must be visible to activation.
	d.mu.Lock()
	d.exec("INSERT INTO desk_jobs VALUES('queued')")
	d.mu.Unlock()
	select {
	case <-done:
		m.admission.RUnlock()
		t.Fatal("activation bypassed the research admission lock")
	case <-time.After(20 * time.Millisecond):
	}
	m.admission.RUnlock()
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	if readAuto(t, m).State != "waiting-for-research" {
		t.Fatal("job creation raced the idle check")
	}
	if _, e = os.Stat(filepath.Join(m.root, "active.json")); !os.IsNotExist(e) {
		t.Fatal("queued research job interrupted")
	}
}
