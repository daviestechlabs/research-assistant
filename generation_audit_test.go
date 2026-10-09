package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestGenerationAuditPublicationBoundaries(t *testing.T) {
	d := testDB(t)
	suite, err := decodeGenerationSuite(generationQualityCases)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range suite.Sources {
		doc, err := normalize(Document{ID: s.ID, Source: "arxiv", Title: s.Title, Summary: "Fixture abstract for source binding boundaries."})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = d.put(doc); err != nil {
			t.Fatal(err)
		}
		if _, err = d.run("UPDATE documents SET fingerprint=? WHERE id=?", s.Fingerprint, s.ID); err != nil {
			t.Fatal(err)
		}
	}
	i, err := newInference(d)
	if err != nil {
		t.Fatal(err)
	}
	i.model = "fast-local"
	i.budget.path = filepath.Join(t.TempDir(), "research-usage.sqlite")
	r, err := readGenerationAudit(i)
	if err != nil || r.Status != "missing" {
		t.Fatal("missing generation audit not explicit", err)
	}
	fixture := generationAuditReport{Contract: "research-generation-audit-v1", Status: "measured-development-audit", SuiteSHA: textSHA(generationQualityCases), PolicySHA: generationEvidencePolicySHA(), Model: "fast-local", GenerationCalls: 7, AuditCalls: 0}
	for n := 0; n < 6; n++ {
		fixture.Cases = append(fixture.Cases, json.RawMessage(`{}`))
	}
	b, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	approval, err := json.Marshal(map[string]any{"contract": fixture.Contract, "receipt_sha256": textSHA(b), "sources": suite.Sources})
	if err != nil {
		t.Fatal(err)
	}
	previous := generationAuditApproval
	generationAuditApproval = approval
	defer func() { generationAuditApproval = previous }()
	path := filepath.Join(filepath.Dir(i.budget.path), "research-generation-audit.json")
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	r, err = readGenerationAudit(i)
	if err != nil || r.Status != "measured-development-audit" || r.GenerationCalls != 7 || len(r.Cases) != 6 {
		t.Fatal("measured receipt unavailable", r.Status, err)
	}
	legacy := fixture
	legacy.PolicySHA = ""
	legacyBytes, _ := json.Marshal(legacy)
	legacyApproval, _ := json.Marshal(map[string]any{"contract": fixture.Contract, "receipt_sha256": textSHA(legacyBytes), "sources": suite.Sources})
	generationAuditApproval = legacyApproval
	if err = os.WriteFile(path, legacyBytes, 0600); err != nil {
		t.Fatal(err)
	}
	r, err = readGenerationAudit(i)
	if err != nil || r.Status != "stale" {
		t.Fatal("unbound policy audit appears current")
	}
	generationAuditApproval = approval
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	invalidFilename, _ := json.Marshal(map[string]any{"contract": fixture.Contract, "receipt_sha256": textSHA(b), "sources": suite.Sources, "filename": "../research-generation-audit.json"})
	generationAuditApproval = invalidFilename
	if _, err = readGenerationAudit(i); err == nil {
		t.Fatal("audit path escaped the NAS receipt directory")
	}
	generationAuditApproval = approval
	if err = os.WriteFile(path, append(b, ' '), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = readGenerationAudit(i); err == nil {
		t.Fatal("unapproved receipt accepted")
	}
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	i.model = "different-model"
	r, err = readGenerationAudit(i)
	if err != nil || r.Status != "stale" {
		t.Fatal("different model appears measured")
	}
	i.model = "fast-local"
	i.identityURL = "http://native.invalid/identity"
	r, err = readGenerationAudit(i)
	if err != nil || r.Status != "stale" {
		t.Fatal("alias-only audit passed native binding")
	}
	i.identityURL = ""
	if _, err = d.run("UPDATE documents SET fingerprint='changed' WHERE id=?", suite.Sources[0].ID); err != nil {
		t.Fatal(err)
	}
	r, err = readGenerationAudit(i)
	if err != nil || r.Status != "stale" {
		t.Fatal("source drift accepted")
	}
	stats, err := i.stats()
	if err != nil || stats.(map[string]any)["model_calls_today"].(int) != 0 {
		t.Fatal("audit reading charged model usage")
	}
}

func TestGenerationAuditCollectionKeepsRunScope(t *testing.T) {
	d := testDB(t)
	suite, err := decodeGenerationSuite(generationQualityCases)
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range suite.Sources {
		doc, err := normalize(Document{ID: source.ID, Source: "arxiv", Title: source.Title, Summary: "Fixture abstract for audit collection boundaries."})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.put(doc); err != nil {
			t.Fatal(err)
		}
		if _, err := d.run("UPDATE documents SET fingerprint=? WHERE id=?", source.Fingerprint, source.ID); err != nil {
			t.Fatal(err)
		}
	}
	i, err := newInference(d)
	if err != nil {
		t.Fatal(err)
	}
	i.model = "smart-local"
	i.budget.path = filepath.Join(t.TempDir(), "research-usage.sqlite")
	partial := generationAuditReport{Contract: "research-generation-audit-v1", Status: "partial-development-audit", SuiteSHA: textSHA(generationQualityCases), PolicySHA: generationEvidencePolicySHA(), Model: "smart-local", GenerationCalls: 11, AuditCalls: 0, Unmeasured: []string{suite.Cases[5].ID}}
	for _, c := range suite.Cases[:5] {
		b, _ := json.Marshal(map[string]string{"case_id": c.ID})
		partial.Cases = append(partial.Cases, b)
	}
	selected := partial
	selected.Status = "measured-development-audit"
	selected.SelectedCase = suite.Cases[4].ID
	selected.Cases = partial.Cases[4:5]
	selected.Unmeasured = nil
	selected.GenerationCalls = 2
	previous := generationAuditApproval
	defer func() { generationAuditApproval = previous }()
	publish := func(reports []generationAuditReport) {
		t.Helper()
		entries := []generationAuditEntry{}
		for n, r := range reports {
			name := []string{"research-generation-audit-partial-fixture.json", "research-generation-audit-selected-fixture.json"}[n]
			b, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(filepath.Dir(i.budget.path), name), b, 0600); err != nil {
				t.Fatal(err)
			}
			entries = append(entries, generationAuditEntry{Contract: r.Contract, SHA: textSHA(b), Filename: name})
		}
		b, err := json.Marshal(generationAuditSelection{generationAuditEntry: generationAuditEntry{Contract: "research-generation-audit-collection-v1"}, Sources: suite.Sources, Audits: entries})
		if err != nil {
			t.Fatal(err)
		}
		generationAuditApproval = b
	}
	publish([]generationAuditReport{partial, selected})
	r, err := readGenerationAudit(i)
	if err != nil || r.Status != "measured-development-audits" || len(r.Audits) != 2 {
		t.Fatal("separate measured runs unavailable", err, r)
	}
	if r.Audits[0].Status != "partial-development-audit" || len(r.Audits[0].Unmeasured) != 1 || r.Audits[0].GenerationCalls != 11 || r.Audits[1].SelectedCase != suite.Cases[4].ID || r.Audits[1].GenerationCalls != 2 || len(r.Cases) != 0 {
		t.Fatal("partial and selected attempts were pooled or lost", r)
	}
	for _, tc := range []struct {
		name   string
		change func(*generationAuditReport)
	}{
		{"partial called complete", func(r *generationAuditReport) { r.Status = "measured-development-audit" }},
		{"unknown selected case", func(r *generationAuditReport) { r.SelectedCase = "not-a-case" }},
		{"case order changed", func(r *generationAuditReport) { r.Cases = append([]json.RawMessage{r.Cases[1]}, r.Cases[1:]...) }},
		{"unrun case changed", func(r *generationAuditReport) { r.Unmeasured = []string{suite.Cases[0].ID} }},
		{"audit made model calls", func(r *generationAuditReport) { r.AuditCalls = 1 }},
		{"nested run supplied", func(r *generationAuditReport) { r.Audits = []generationAuditReport{selected} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := partial
			tc.change(&bad)
			publish([]generationAuditReport{bad, selected})
			if _, err := readGenerationAudit(i); err == nil {
				t.Fatal("invalid run scope accepted")
			}
		})
	}
	publish([]generationAuditReport{partial, selected})
	var approval generationAuditSelection
	if json.Unmarshal(generationAuditApproval, &approval) != nil {
		t.Fatal("fixture approval decode")
	}
	approval.Audits[1] = approval.Audits[0]
	generationAuditApproval, _ = json.Marshal(approval)
	if _, err := readGenerationAudit(i); err == nil {
		t.Fatal("duplicate receipt counted twice")
	}
	publish([]generationAuditReport{partial, selected})
	i.model = "fast-local"
	r, err = readGenerationAudit(i)
	if err != nil || r.Audits[0].Status != "stale" || r.Audits[1].Status != "stale" {
		t.Fatal("different model published as current", err)
	}
	stats, err := i.stats()
	if err != nil || stats.(map[string]any)["model_calls_today"].(int) != 0 {
		t.Fatal("reading collection charged model usage")
	}
}
