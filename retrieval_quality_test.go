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
	"time"
)

// All vectors and native responses here are fixtures, not retrieval measurements.
func retrievalFixture(t *testing.T) (*inference, qualitySuite, *atomic.Int32, *atomic.Bool) {
	t.Helper()
	i, suite := qualityFixture(t)
	calls, drift := &atomic.Int32{}, &atomic.Bool{}
	docs, err := i.db.getDocuments([]string{suite.Sources[0].ID, suite.Sources[1].ID, suite.Sources[2].ID, suite.Sources[3].ID})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("chat authorization leaked into retrieval fixture")
		}
		switch r.URL.Path {
		case "/health":
			writeJSON(w, 200, semanticEngine())
		case "/tokenize":
			var b struct {
				Input string `json:"input"`
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			offsets := [][]int{}
			for n := range []rune(b.Input) {
				offsets = append(offsets, []int{n, n + 1})
			}
			writeJSON(w, 200, map[string]any{"input_sha256": textSHA([]byte(b.Input)), "max_input_tokens": 128, "special_tokens": 2, "offsets": offsets})
		case "/embeddings":
			calls.Add(1)
			var b struct {
				Input    []string `json:"input"`
				Truncate bool     `json:"truncate"`
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			if b.Truncate {
				t.Error("silent query truncation")
			}
			if strings.Contains(strings.Join(b.Input, " "), "arxiv:") {
				t.Error("curated target identifiers leaked to embedding request")
			}
			counts, data := []int{}, []any{}
			for n, input := range b.Input {
				index := -1
				for _, q := range suite.Retrieval {
					if input == q.Query {
						for j, doc := range docs {
							if doc.ID == q.TargetID {
								index = j
							}
						}
					}
				}
				if index < 0 {
					t.Error("unexpected embedding query")
					index = 0
				}
				v := make([]float64, semanticDimensions)
				v[index] = 1
				counts = append(counts, len([]rune(input))+2)
				data = append(data, map[string]any{"index": n, "embedding": v})
			}
			writeJSON(w, 200, map[string]any{"model": "bge-m3", "input_contract": map[string]any{"truncated": false, "pooling": "cls", "max_input_tokens": 128, "input_tokens": counts}, "data": data})
		case "/v2/vectordb/entities/search":
			var b struct {
				Filter     string      `json:"filter"`
				Limit      int         `json:"limit"`
				Collection string      `json:"collectionName"`
				Data       [][]float64 `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			if b.Filter != "" || b.Limit != 10 || b.Collection != i.semantic.collection || len(b.Data) != 1 {
				t.Error("benchmark restricted or changed its corpus")
			}
			target := 0
			for n := range docs {
				if b.Data[0][n] > .5 {
					target = n
				}
			}
			hit := docs[target]
			writeJSON(w, 200, map[string]any{"code": 0, "data": []any{map[string]any{"id": hit.ID, "fingerprint": hit.Fingerprint, "distance": 1.0}}})
			if drift.Load() {
				writeSemanticStatus(t, i, "indexing", 1)
			}
		default:
			t.Error("unexpected fixture request")
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(server.Close)
	i.semantic = &semanticClient{embeddingOrigin: server.URL, milvusOrigin: server.URL, statusPath: filepath.Join(t.TempDir(), "status.json"), catalogSHA: strings.Repeat("a", 64), modelSHA: strings.Repeat("b", 64), total: len(docs), client: i.client}
	i.semantic.collection = semanticCollection(i.semantic.catalogSHA, i.semantic.modelSHA)
	writeSemanticStatus(t, i, "complete", len(docs))
	i.limitOwner = "litellm"
	return i, suite, calls, drift
}

func TestRetrievalRejectsPartialCoverageBeforeAnyArtifactOrCall(t *testing.T) {
	i, _, calls, _ := retrievalFixture(t)
	for _, state := range []string{"indexing", "limited", "failed", "interrupted"} {
		writeSemanticStatus(t, i, state, 1)
		root := t.TempDir()
		path, desk := filepath.Join(root, "report.json"), filepath.Join(root, "desk.sqlite")
		if _, err := i.benchmarkRetrieval(context.Background(), path, desk); err == nil {
			t.Fatalf("accepted %s corpus", state)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("incomplete corpus wrote report")
		}
		if _, err := os.Stat(desk); !os.IsNotExist(err) {
			t.Fatal("incomplete corpus created desk")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("incomplete coverage dispatched an embedding")
	}
}

func TestRetrievalPairedMetricsSourcesAndFreeRead(t *testing.T) {
	i, suite, calls, _ := retrievalFixture(t)
	report, err := i.benchmarkRetrieval(context.Background(), retrievalPath(i), filepath.Join(t.TempDir(), "fresh-desk.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "complete" || len(report.Cases) != 8 || len(report.Attempts) != 8 || calls.Load() != 8 || report.EmbeddingCalls != 8 {
		t.Fatalf("incomplete benchmark: %+v %d", report, calls.Load())
	}
	for _, g := range report.Groups {
		if g.Queries != 4 || g.MeaningHits != 4 || g.MeaningMRR != 1 {
			t.Fatalf("group: %+v", g)
		}
	}
	for n, row := range report.Cases {
		if row.qualityQuery != suite.Retrieval[n] || row.Meaning[0].ID != row.TargetID {
			t.Fatal("query pair changed")
		}
	}
	// Persisted aggregate counters and ranks cannot override the recorded results.
	report.Groups[0].KeywordHits = 999
	report.EmbeddingCalls = 999
	report.Cases[0].MeaningRank = 999
	if err = publishQuality(retrievalPath(i), report); err != nil {
		t.Fatal(err)
	}
	got, err := readRetrieval(i)
	if err != nil || got.EmbeddingCalls != 8 || got.Groups[0].KeywordHits > 4 || got.Cases[0].MeaningRank != 1 || calls.Load() != 8 {
		t.Fatalf("unsafe or costly read: %+v %v", got, err)
	}
	if _, err = i.benchmarkRetrieval(context.Background(), retrievalPath(i), filepath.Join(t.TempDir(), "retry.sqlite")); err == nil || calls.Load() != 8 {
		t.Fatal("existing measurement overwritten or retried")
	}
	for _, change := range []func(*retrievalReport){
		func(r *retrievalReport) { r.Cases = r.Cases[:7] },
		func(r *retrievalReport) { r.Cases[0].Query = "changed query" },
		func(r *retrievalReport) { r.Cases[0].Meaning = append(r.Cases[0].Meaning, r.Cases[0].Meaning[0]) },
		func(r *retrievalReport) { r.Cases[0].Meaning[0].Fingerprint = strings.Repeat("9", 64) },
		func(r *retrievalReport) { r.Attempts[0].ReservedUnits = 0 },
	} {
		raw, _ := json.Marshal(report)
		var bad retrievalReport
		_ = json.Unmarshal(raw, &bad)
		change(&bad)
		if err = publishQuality(retrievalPath(i), bad); err != nil {
			t.Fatal(err)
		}
		if _, err = readRetrieval(i); err == nil {
			t.Fatal("malformed paired measurement accepted")
		}
	}
	if err = publishQuality(retrievalPath(i), report); err != nil {
		t.Fatal(err)
	}
	writeSemanticStatus(t, i, "indexing", 1)
	stale, err := readRetrieval(i)
	if err != nil || stale.Status != "stale" || len(stale.Cases) != 0 || calls.Load() != 8 {
		t.Fatalf("stale corpus accepted: %+v %v", stale, err)
	}
}

func TestRetrievalIndexDriftRetainsFailedReservationAndNoCompletedPair(t *testing.T) {
	i, _, calls, drift := retrievalFixture(t)
	drift.Store(true)
	path := retrievalPath(i)
	report, err := i.benchmarkRetrieval(context.Background(), path, filepath.Join(t.TempDir(), "fresh.sqlite"))
	if err == nil || report.Status != "failed" || len(report.Cases) != 0 || len(report.Attempts) != 1 || report.Attempts[0].Outcome != "failed" || report.EmbeddingCalls != 1 || report.ReservedUnits < 1 || calls.Load() != 1 {
		t.Fatalf("lost failed attempt: %+v %v calls=%d", report, err, calls.Load())
	}
	var saved retrievalReport
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &saved) != nil || saved.Status != "failed" || saved.EmbeddingCalls != 1 {
		t.Fatal("failed report did not persist")
	}
}

func TestSemanticFailedEmbeddingReturnsChargedReservation(t *testing.T) {
	s, i, _, _ := semanticFixture(t)
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			writeJSON(w, 200, semanticEngine())
		case "/tokenize":
			writeJSON(w, 200, map[string]any{"input_sha256": textSHA([]byte("rocks")), "max_input_tokens": 128, "special_tokens": 2, "offsets": [][]int{{0, 5}}})
		case "/embeddings":
			w.WriteHeader(503)
		default:
			t.Error("unexpected path")
		}
	}))
	defer engine.Close()
	i.semantic.embeddingOrigin = engine.URL
	result, err := s.semanticSearchJob(context.Background(), "owner", SearchRequest{Query: "rocks", Source: "arxiv", Limit: 10}, func(string) {})
	if err == nil || result.ModelCalls != 1 || result.ReservedUnits < 1 {
		t.Fatalf("failed reservation lost: %+v %v", result, err)
	}
	i.budget.mu.Lock()
	rows, err := i.budget.rows("SELECT calls FROM usage WHERE day=? AND kind='abstract-embedding'", time.Now().UTC().Format("2006-01-02"))
	i.budget.mu.Unlock()
	if err != nil || len(rows) != 1 || rows[0][0] != "1" {
		t.Fatal("failed embedding was not charged")
	}
}

func TestEmbeddingOperatorDoesNotInheritOrWeakenChatPolicy(t *testing.T) {
	d := testDB(t)
	t.Setenv("RESEARCH_LIMIT_OWNER", "litellm")
	t.Setenv("RESEARCH_CHAT_URL", "http://unapproved.invalid/v1/chat/completions")
	t.Setenv("RESEARCH_CHAT_MODEL", "unapproved-model")
	t.Setenv("RESEARCH_MODEL_KEY", "fixture-chat-key-not-real")
	t.Setenv("RESEARCH_MODEL_IDENTITY_URL", "http://unapproved.invalid/identity")
	t.Setenv("RESEARCH_DAILY_UNITS", "invalid-chat-policy")
	i, err := newEmbeddingInference(d)
	if err != nil {
		t.Fatal(err)
	}
	defer i.close()
	if i.answerURL != "" || i.model != "" || i.key != "" || i.rerankURL != "" || i.identityURL != "" || i.dailyCalls != 0 || i.dailyUnits != 0 || cap(i.gate) != 1 {
		t.Fatal("embedding-only operator inherited chat credentials or a daily quota")
	}
	for n := 0; n < 21; n++ {
		if err = i.reserve("abstract-embedding", 100); err != nil {
			t.Fatal("operator inherited a daily allowance", err)
		}
	}
	if _, err = newInference(d); err == nil {
		t.Fatal("chat constructor accepted an unapproved gateway")
	}
	t.Setenv("RESEARCH_LIMIT_OWNER", "operator-bounded-embedding")
	if _, err = newInference(d); err == nil {
		t.Fatal("internal operator mode became selectable in the app")
	}
}
