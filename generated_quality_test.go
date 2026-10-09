package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGeneratedAuditRejectsTamperingAndMarksSourceDrift(t *testing.T) {
	d := testDB(t)
	if _, err := d.run(`INSERT INTO documents(id,source,title,authors,summary,summary_kind,categories,language,published,doi,nas_ref,fingerprint,imported_at) VALUES('arxiv:0704.0001','arxiv','Fixture','','Fixture words are only a boundary test.','abstract','','','','','',?,'2026-10-07T00:00:00Z')`, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	i, err := newInference(d)
	if err != nil {
		t.Fatal(err)
	}
	i.budget.path = filepath.Join(t.TempDir(), "research-usage.sqlite")
	missing, err := readGeneratedQuality(i)
	if err != nil || missing.Status != "missing" {
		t.Fatal("missing audit did not remain explicit")
	}
	r := generatedQualityReport{Contract: "generated-output-audit-v1", Status: "complete", ModelCalls: 0, Counts: json.RawMessage(`{"outputs":8}`), Cases: []json.RawMessage{}}
	for n := 0; n < 8; n++ {
		r.Cases = append(r.Cases, json.RawMessage(`{}`))
	}
	data, _ := json.Marshal(r)
	approval, _ := json.Marshal(map[string]any{"contract": r.Contract, "receipt_sha256": textSHA(data), "sources": []qualitySource{{ID: "arxiv:0704.0001", Fingerprint: strings.Repeat("a", 64)}}})
	previous := generatedQualityApproval
	generatedQualityApproval = approval
	defer func() { generatedQualityApproval = previous }()
	path := filepath.Join(filepath.Dir(i.budget.path), "research-generated-quality.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := readGeneratedQuality(i)
	if err != nil || got.Status != "complete" {
		t.Fatalf("approved audit: %+v %v", got, err)
	}
	if err := os.WriteFile(path, append(data, ' '), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readGeneratedQuality(i); err == nil {
		t.Fatal("unapproved receipt mutation accepted")
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.run("UPDATE documents SET fingerprint=? WHERE id='arxiv:0704.0001'", strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	got, err = readGeneratedQuality(i)
	if err != nil || got.Status != "stale" {
		t.Fatal("source drift left an apparently current audit")
	}
	stats, err := i.stats()
	if err != nil || stats.(map[string]any)["model_calls_today"].(int) != 0 {
		t.Fatal("reading an audit charged model usage")
	}
}
