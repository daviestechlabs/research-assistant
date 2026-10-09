package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// Reconstruct only exact curated excerpts for orchestration tests. The actual
// operator run independently checks these against full real NAS abstracts.
func qualityFixture(t *testing.T) (*inference, qualitySuite) {
	t.Helper()
	d := testDB(t)
	suite, err := loadQualitySuite()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range suite.Sources {
		quotes := []string{}
		for _, c := range suite.Cases {
			for _, q := range c.Evidence {
				if q.ID == s.ID {
					quotes = append(quotes, q.Quote)
				}
			}
		}
		_, err := d.run(`INSERT INTO documents(id,source,title,authors,summary,summary_kind,categories,language,published,doi,nas_ref,fingerprint,imported_at) VALUES(?,'arxiv',?,'',?,'abstract','','','','','',?,'2026-10-07T00:00:00Z')`, s.ID, s.Title, strings.Join(quotes, " "), s.Fingerprint)
		if err != nil {
			t.Fatal(err)
		}
	}
	i, err := newInference(d)
	if err != nil {
		t.Fatal(err)
	}
	i.model = "fixture-model"
	i.budget.path = filepath.Join(t.TempDir(), "research-usage.sqlite")
	return i, suite
}

func TestQualitySourceDriftStopsBeforeModel(t *testing.T) {
	i, suite := qualityFixture(t)
	findings, checks, err := prepareQuality(i.db, suite)
	if err != nil || len(findings) != 16 || checks.VerifiedExcerpts < 16 || checks.FabricatedRejected != 4 || checks.MisboundRejected != 16 {
		t.Fatalf("source checks: %+v %v", checks, err)
	}
	if len(findings[15].Citations) != 2 || findings[15].Citations[1].Scope != "synthetic-adversarial-excerpt" {
		t.Fatal("adversarial source must remain explicitly synthetic")
	}
	suite.Sources[0].Fingerprint = "changed"
	if _, _, err = prepareQuality(i.db, suite); err == nil {
		t.Fatal("source drift accepted")
	}
}

func TestQualityBenchmarkBlindLabelsMetricsAndReadWithoutCalls(t *testing.T) {
	i, suite := qualityFixture(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		payload := body.Messages[1].Content
		if strings.Contains(payload, "expected_supported") || strings.Contains(payload, "rationale") || strings.Contains(payload, "suite_sha256") {
			t.Fatal("annotations leaked to reviewer")
		}
		var inputs []comparisonReviewInput
		if err := json.Unmarshal([]byte(payload), &inputs); err != nil {
			t.Fatal(err)
		}
		decisions := []ComparisonReview{}
		for _, input := range inputs {
			accepted := false
			for _, c := range suite.Cases {
				if c.Text == input.Finding.Text {
					accepted = c.ExpectedSupported
					if c.ID == "penalty-guarantees" {
						accepted = true
					}
					if c.ID == "rag-memory-combination" {
						accepted = false
					}
				}
			}
			verdict, reason := "insufficient", "unsupported-relationship"
			if accepted {
				verdict, reason = "supported", "direct-evidence"
			}
			decisions = append(decisions, ComparisonReview{input.Category, input.Index, verdict, reason})
		}
		content, _ := json.Marshal(map[string]any{"decisions": decisions})
		writeJSON(w, 200, map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"content": string(content)}}}})
	}))
	defer server.Close()
	i.answerURL = server.URL
	r, err := i.benchmarkQuality(context.Background(), qualityPath(i))
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "complete" || r.ModelCalls != 4 || calls.Load() != 4 || r.Counts.FalseAccepts != 1 || r.Counts.FalseAbstentions != 1 {
		t.Fatalf("unreported quality failures: %+v", r.Counts)
	}
	// Counters are derived again on read. Fetching saved measurements is free.
	r.Counts.FalseAccepts = 0
	if err = publishQuality(qualityPath(i), r); err != nil {
		t.Fatal(err)
	}
	got, err := readQuality(i)
	if err != nil || got.Counts.FalseAccepts != 1 || calls.Load() != 4 {
		t.Fatalf("receipt: %+v %v", got.Counts, err)
	}
	i.model = "replacement"
	got, err = readQuality(i)
	if err != nil || got.Status != "stale" {
		t.Fatal("changed model configuration reused old result")
	}
	i.model = "fixture-model"
	r.PolicySHA = "changed"
	if err = publishQuality(qualityPath(i), r); err != nil {
		t.Fatal(err)
	}
	got, err = readQuality(i)
	if err != nil || got.Status != "stale" {
		t.Fatal("changed policy reused old result")
	}
}

func TestQualityMetricBindingAndMissingReceipt(t *testing.T) {
	i, _ := qualityFixture(t)
	r, err := readQuality(i)
	if err != nil || r.Status != "missing" {
		t.Fatal("missing report is not a success")
	}
	r.Cases = []qualityCase{{ID: "correct", ExpectedSupported: true}}
	r.Outcomes = []qualityOutcome{{CaseID: "wrong", Expected: true, Accepted: true}}
	if summarizeQuality(&r) == nil {
		t.Fatal("misbound label accepted")
	}
	if publishQuality("relative.json", r) == nil {
		t.Fatal("relative output accepted")
	}
}
