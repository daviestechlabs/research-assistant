package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArxivUpdatePreservesSourcesAndStagesChangesWithoutActivation(t *testing.T) {
	store := indexFixtureStore(t, map[string][]byte{"unused.jsonl": []byte("unused")})
	m, base := indexFixtureManager(t, store, "jsonl", "unused.jsonl")
	h, _ := syncFixture(t)
	t.Setenv("RESEARCH_ARXIV_SYNC_ROOT", h.root)
	t.Setenv("RESEARCH_ARXIV_UPDATES_ENABLED", "true")
	var rec arxivRecord
	decodeSyncFixture(oaiFixture("2610.00001", "", "New mineral experiments."), &rec)
	if e := h.storeRecord(rec); e != nil {
		t.Fatal(e)
	}
	decodeSyncFixture(oaiFixture("2301.00001", "", "Revised geological findings."), &rec)
	if e := h.storeRecord(rec); e != nil {
		t.Fatal(e)
	}
	m.base.SHA, _ = indexFileSHA(base)
	m.base.Semantic = filepath.Join(h.root, "base-semantic.json")
	m.config.ModelSHA = strings.Repeat("a", 64)
	coverage := SemanticCoverage{CatalogSHA: m.base.SHA, ModelSHA: m.config.ModelSHA, State: "complete", Total: 1, Indexed: 1, VerifiedCount: 1, Algorithm: semanticAlgorithm, Dimensions: semanticDimensions, Collection: semanticCollection(m.base.SHA, m.config.ModelSHA), Engine: semanticEngine()}
	if e := writeIndexJSON(m.base.Semantic, coverage); e != nil {
		t.Fatal(e)
	}
	before := m.base.SHA
	s, e := m.submit("arxiv-update", nil, true)
	if e != nil {
		t.Fatal(e)
	}
	// Changes after queueing must not enter this frozen candidate.
	decodeSyncFixture(oaiFixture("2610.00002", "", "Later metadata."), &rec)
	if e = h.storeRecord(rec); e != nil {
		t.Fatal(e)
	}
	var run indexRun
	if e = readIndexJSON(m.runPath(s.ID, "request.json"), &run); e != nil {
		t.Fatal(e)
	}
	e = m.execute(context.Background(), run, &s)
	if e == nil || s.Phase != "meaning" {
		t.Fatalf("expected missing fixture embedding origin after valid keyword merge: %v %s", e, s.Phase)
	}
	d, e := openDatabase(m.runPath(s.ID, "catalog.sqlite"), false)
	if e != nil {
		t.Fatal(e)
	}
	defer d.close()
	rows, e := d.rows("SELECT source,count(*) FROM documents GROUP BY source ORDER BY source")
	if e != nil || len(rows) != 2 || rows[0][1] != "2" || rows[1][1] != "1" {
		t.Fatalf("sources lost: %v %v", rows, e)
	}
	rows, _ = d.rows("SELECT summary FROM documents WHERE id='arxiv:2301.00001'")
	if rows[0][0] != "Revised geological findings." {
		t.Fatal("revision not applied")
	}
	if s.Validation["added_records"] != "1" || s.Validation["changed_records"] != "1" {
		t.Fatalf("bad change accounting: %v", s.Validation)
	}
	after, _ := indexFileSHA(base)
	if before != after {
		t.Fatal("serving catalog changed")
	}
	if _, e = os.Stat(filepath.Join(m.root, "active.json")); !os.IsNotExist(e) {
		t.Fatal("update activated automatically")
	}
	if _, e = m.submit("arxiv-update", nil, true); e == nil {
		t.Fatal("overlapping indexing run accepted")
	}
}
func TestArxivUpdateRequiresCompleteActiveMeaningAndWorkerSupport(t *testing.T) {
	store := indexFixtureStore(t, map[string][]byte{})
	m, _ := indexFixtureManager(t, store, "jsonl", "unused.jsonl")
	t.Setenv("RESEARCH_ARXIV_SYNC_ROOT", t.TempDir())
	if _, e := m.submit("arxiv-update", nil, true); e == nil {
		t.Fatal("old worker accepted update")
	}
	t.Setenv("RESEARCH_ARXIV_UPDATES_ENABLED", "true")
	if _, e := m.submit("arxiv-update", nil, true); e == nil {
		t.Fatal("partial active meaning index accepted")
	}
}
