package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHeldoutReviewKeepsLabelsBlindAndRetainsFirstMeasurement(t *testing.T) {
	d := testDB(t)
	suite, err := decodeQualitySuite(qualityHoldout, 4, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range suite.Sources {
		quotes := []string{}
		for _, c := range suite.Cases {
			for _, q := range c.Evidence {
				if q.ID == source.ID {
					quotes = append(quotes, q.Quote)
				}
			}
		}
		if _, err := d.run(`INSERT INTO documents(id,source,title,authors,summary,summary_kind,categories,language,published,doi,nas_ref,fingerprint,imported_at) VALUES(?,'arxiv',?,'',?,'abstract','','','','','',?,'2026-10-07T00:00:00Z')`, source.ID, source.Title, strings.Join(quotes, " "), source.Fingerprint); err != nil {
			t.Fatal(err)
		}
	}
	i, err := newInference(d)
	if err != nil {
		t.Fatal(err)
	}
	i.model = "fixture-model"
	i.budget.path = filepath.Join(t.TempDir(), "research-usage.sqlite")
	path := filepath.Join(filepath.Dir(i.budget.path), "research-quality-holdout.json")
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
		request := body.Messages[1].Content
		if strings.Contains(request, "expected_supported") || strings.Contains(request, "rationale") || strings.Contains(request, "sealed") {
			t.Fatal("held-out labels leaked into model request")
		}
		var inputs []comparisonReviewInput
		if err := json.Unmarshal([]byte(request), &inputs); err != nil {
			t.Fatal(err)
		}
		if len(inputs) != 4 {
			t.Fatal("held-out request omitted cases")
		}
		decisions := []ComparisonReview{}
		for _, input := range inputs {
			verdict, reason := "insufficient", "missing-evidence"
			if input.Category == "methods" {
				verdict, reason = "supported", "direct-evidence"
			}
			decisions = append(decisions, ComparisonReview{input.Category, input.Index, verdict, reason})
		}
		content, _ := json.Marshal(map[string]any{"decisions": decisions})
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"content": string(content)}}}})
	}))
	defer server.Close()
	i.answerURL = server.URL
	if _, err = i.benchmarkQualityHoldout(context.Background(), filepath.Join(filepath.Dir(path), "other.json")); err == nil || calls.Load() != 0 {
		t.Fatal("alternate held-out path accepted")
	}
	r, err := i.benchmarkQualityHoldout(context.Background(), path)
	if err != nil || r.Status != "complete" || r.ModelCalls != 1 || r.Counts.TrueAccepts != 2 || r.Counts.TrueAbstentions != 2 {
		t.Fatalf("held-out measurement: %+v %v", r, err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = i.benchmarkQualityHoldout(context.Background(), path); err == nil || calls.Load() != 1 {
		t.Fatal("observed holdout was rerun")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("first measurement changed")
	}
	response := httptest.NewRecorder()
	qualityHandler(i)(response, httptest.NewRequest("GET", "/api/quality", nil))
	var out struct {
		Status  string        `json:"status"`
		Holdout qualityReport `json:"holdout"`
	}
	if json.Unmarshal(response.Body.Bytes(), &out) != nil || out.Status != "missing" || out.Holdout.Status != "complete" || calls.Load() != 1 {
		t.Fatal("held-out report unavailable without development report")
	}
	r.PolicySHA = "changed"
	if err := publishQuality(path, r); err != nil {
		t.Fatal(err)
	}
	stale, err := readQualitySuite(i, path, qualityHoldout, 4, 2, 0)
	if err != nil || stale.Status != "stale" || calls.Load() != 1 {
		t.Fatal("changed policy left a fresh held-out claim")
	}
}
