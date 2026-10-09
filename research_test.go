package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if err := enableSQLiteMemoryAccounting(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func testDB(t *testing.T) *database {
	t.Helper()
	d, err := openDatabase(filepath.Join(t.TempDir(), "catalog.sqlite"), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.close)
	if err = d.initialize(); err != nil {
		t.Fatal(err)
	}
	return d
}

const arxivFixture = `{"id":"1706.03762v7","title":"Attention Is All You Need","authors":"Vaswani et al.","abstract":"A transformer replaces recurrence with attention for machine translation.","categories":"cs.CL cs.LG","doi":"10.5555/3295222.3295349","published":"2017-06-12","nas_ref":"s3://training-data/books/arxiv/1706.03762/paper.pdf"}
{"id":"2005.11401","title":"Retrieval-Augmented Generation","authors":"Lewis et al.","abstract":"Retrieval combines a generator with external knowledge from retrieved documents.","categories":"cs.CL","published":"2020-05-22"}
{"id":"hep-th/9901001v2","title":"Black holes","authors":"Example","abstract":"Gravity and quantum theory.","categories":"hep-th","published":"1999-01-01"}
`

func loadFixture(t *testing.T, d *database) {
	t.Helper()
	r, err := importCatalog(d, "arxiv", "jsonl", strings.NewReader(arxivFixture), 0)
	if err != nil || r.Changed != 3 || r.Committed != 3 {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestImportResumeRefreshAndFTS(t *testing.T) {
	d := testDB(t)
	loadFixture(t, d)
	r, err := importCatalog(d, "arxiv", "jsonl", strings.NewReader(arxivFixture), 0)
	if err != nil || r.Changed != 0 || r.Unchanged != 3 {
		t.Fatalf("%+v %v", r, err)
	}
	for _, q := range []SearchRequest{{Query: "attention"}, {Query: "1706.03762v8"}, {Query: "arxiv:1706.03762"}, {Query: "10.5555/3295222.3295349"}, {Query: "attention translation", Category: "cs.CL", From: "2017-01-01", To: "2017-12-31"}} {
		v, err := d.search(q)
		if err != nil || len(v.Documents) != 1 || v.Documents[0].ID != "arxiv:1706.03762" || v.ModelCalls != 0 {
			t.Fatalf("%+v %+v %v", q, v, err)
		}
	}
	update := `{"id":"1706.03762","title":"Changed title","abstract":"Novel zebras","authors":"Vaswani"}`
	if _, err := importCatalog(d, "arxiv", "jsonl", strings.NewReader(update), 0); err != nil {
		t.Fatal(err)
	}
	v, err := d.search(SearchRequest{Query: "attention"})
	if err != nil || len(v.Documents) != 0 {
		t.Fatalf("stale FTS: %+v %v", v, err)
	}
	v, err = d.search(SearchRequest{Query: "zebras"})
	if err != nil || len(v.Documents) != 1 {
		t.Fatalf("missing update: %+v %v", v, err)
	}
	if _, err = d.run("INSERT INTO abstracts(abstracts,rank) VALUES('integrity-check',1)"); err != nil {
		t.Fatal(err)
	}
}

func TestImportRollbackAndCheckpoint(t *testing.T) {
	d := testDB(t)
	var data strings.Builder
	for n := 0; n < 501; n++ {
		fmt.Fprintf(&data, "{\"id\":\"2401.%05d\",\"title\":\"Document %d\",\"abstract\":\"Valid summary\"}\n", n, n)
	}
	data.WriteString("broken json\n")
	r, err := importCatalog(d, "arxiv", "jsonl", strings.NewReader(data.String()), 0)
	if err == nil || r.Committed != 500 {
		t.Fatalf("%+v %v", r, err)
	}
	rows, err := d.rows("SELECT count(*) FROM documents")
	if err != nil || rows[0][0] != "500" {
		t.Fatalf("%v %v", rows, err)
	}
	data.Reset()
	data.WriteString(arxivFixture)
	data.WriteString("not json")
	r, err = importCatalog(d, "arxiv", "jsonl", strings.NewReader(data.String()), 0)
	if err == nil || r.Committed != 0 {
		t.Fatalf("%+v %v", r, err)
	}
	rows, _ = d.rows("SELECT count(*) FROM documents")
	if rows[0][0] != "500" {
		t.Fatal(rows)
	}
}

func TestGutenbergCoverageAndDates(t *testing.T) {
	d := testDB(t)
	csv := "Text#,Type,Issued,Title,Language,Authors,Subjects,Bookshelves,Description\n11,Text,1971-12-01,Alice,en,Carroll,Fantasy,Children,\n12,Text,2008-06-27,Looking Glass,en,Carroll,Fantasy,Children,A journey through a mirror.\n13,Sound,2008-06-27,Sound,en,Carroll,Fantasy,Children,\n"
	r, err := importCatalog(d, "gutenberg", "csv", strings.NewReader(csv), 0)
	if err != nil || r.Changed != 2 || r.MissingSummary != 1 || r.Skipped != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	v, err := d.search(SearchRequest{Query: "Alice", Source: "gutenberg"})
	if err != nil || len(v.Documents) != 1 || v.Documents[0].SummaryKind != "missing" {
		t.Fatalf("%+v %v", v, err)
	}
	if _, err = importCatalog(d, "gutenberg", "csv", strings.NewReader("unknown,title\n1,thing\n"), 0); err == nil {
		t.Fatal("bad catalog accepted")
	}
	date := `{"id":"2301.12345","title":"Dated","abstract":"Test","versions":[{"created":"Sun, 1 Jan 2023 10:00:00 +0000"}]}`
	if _, err = importCatalog(d, "arxiv", "jsonl", strings.NewReader(date), 0); err != nil {
		t.Fatal(err)
	}
	rows, _ := d.rows("SELECT published FROM documents WHERE id='arxiv:2301.12345'")
	if rows[0][0] != "2023-01-01" {
		t.Fatal(rows)
	}
}

func TestSearchInputAndBroadRecall(t *testing.T) {
	d := testDB(t)
	loadFixture(t, d)
	for _, q := range []SearchRequest{{Query: ""}, {Query: "attention", Limit: 100}, {Query: "attention", Source: "other"}, {Query: "attention", From: "2020"}, {Query: "attention", From: "2025-01-01", To: "2020-01-01"}} {
		if _, err := d.search(q); err == nil {
			t.Fatalf("accepted %+v", q)
		}
	}
	v, err := d.search(SearchRequest{Query: `attention " OR zebras`, Broad: true})
	if err != nil || len(v.Documents) != 1 {
		t.Fatalf("literal query: %+v %v", v, err)
	}
	v, err = d.search(SearchRequest{Query: "attention generator", Broad: true})
	if err != nil || len(v.Documents) != 2 {
		t.Fatalf("broad recall: %+v %v", v, err)
	}
	v, err = d.search(SearchRequest{Query: "1706.03762", Source: "gutenberg"})
	if err != nil || len(v.Documents) != 0 {
		t.Fatalf("identifier filter bypass: %+v %v", v, err)
	}
}

func testInference(t *testing.T, d *database) *inference {
	t.Helper()
	t.Setenv("RESEARCH_CHAT_URL", "")
	t.Setenv("RESEARCH_CHAT_MODEL", "")
	t.Setenv("RESEARCH_RERANK_URL", "")
	t.Setenv("RESEARCH_MODEL_KEY", "")
	t.Setenv("RESEARCH_MODEL_IDENTITY_URL", "")
	t.Setenv("RESEARCH_MODEL_ARTIFACTS_SHA256", "")
	t.Setenv("RESEARCH_MODEL_PRODUCER_SHA256", "")
	i, err := newInference(d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(i.close)
	return i
}

func TestAnswerBoundsCitationsAndBudget(t *testing.T) {
	d := testDB(t)
	loadFixture(t, d)
	i := testInference(t, d)
	calls := 0
	content := "The sources use different mechanisms."
	sourceIDs := []string{"arxiv:1706.03762", "arxiv:2005.11401"}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if content, ok := answerReviewFixtureContent(req); ok {
			writeJSON(w, 200, map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": content}}}})
			return
		}
		if req["max_completion_tokens"] != float64(800) {
			t.Error("output cap missing")
		}
		if req["response_format"].(map[string]any)["type"] != "json_schema" {
			t.Error("structured answer schema missing")
		}
		if req["include_reasoning"] != false || req["chat_template_kwargs"].(map[string]any)["enable_thinking"] != false {
			t.Error("thinking was not disabled")
		}
		b, _ := json.Marshal(req)
		if strings.Contains(string(b), "paper.pdf") {
			t.Error("NAS reference sent to model")
		}
		paragraphs := map[string]any{}
		for _, kind := range []string{"reported", "interpretation", "limits"} {
			paragraph := map[string]any{"kind": kind, "text": content, "source_ids": sourceIDs}
			if kind == "limits" {
				paragraph["text"] = "The supplied abstracts omit the quantitative evidence needed to establish a guarantee."
			}
			if kind == "reported" {
				paragraph["evidence_quotes"] = []evidenceQuote{{"arxiv:1706.03762", "A transformer replaces recurrence with attention for machine translation."}, {"arxiv:2005.11401", "Retrieval combines a generator with external knowledge from retrieved documents."}}
			}
			paragraphs[kind] = paragraph
		}
		structured, _ := json.Marshal(paragraphs)
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": string(structured)}}}})
	}))
	defer s.Close()
	i.answerURL = s.URL
	i.model = "fixture"
	req := AnswerRequest{Question: "Explain mechanisms", IDs: []string{"arxiv:1706.03762", "arxiv:2005.11401"}, Task: "explain"}
	v, err := i.answer(context.Background(), req)
	if err != nil || v.ModelCalls != 2 || calls != 2 {
		t.Fatalf("%+v %v", v, err)
	}
	if !strings.Contains(v.Answer, "The supplied abstracts omit the quantitative evidence needed to establish a guarantee.") {
		t.Fatal("the answer discarded its source-specific limits")
	}
	content = "Unknown [arxiv:9999.99999]"
	if _, err = i.answer(context.Background(), req); err == nil {
		t.Fatal("unselected citation accepted")
	}
	content = "No citations"
	sourceIDs = nil
	if _, err = i.answer(context.Background(), req); err == nil {
		t.Fatal("uncited answer accepted")
	}
	i.dailyCalls = 4
	if _, err = i.answer(context.Background(), req); err == nil || calls != 4 {
		t.Fatal("budget did not stop request")
	}
	restarted := testInference(t, d)
	restarted.dailyCalls = 4
	if err = restarted.reserve("answer", 1); err == nil {
		t.Fatal("budget lost on restart")
	}
	bad := req
	bad.IDs = []string{"arxiv:nonexistent"}
	if _, err = i.answer(context.Background(), bad); err == nil {
		t.Fatal("invented source accepted")
	}
}

func TestConcurrentBudgetReservation(t *testing.T) {
	d := testDB(t)
	i := testInference(t, d)
	i.dailyCalls = 5
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for n := 0; n < 30; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i.reserve("answer", 1) == nil {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if accepted != 5 {
		t.Fatalf("reserved %d calls", accepted)
	}
}

func TestBudgetAcrossConnectionsAndInputUnits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.sqlite")
	d1, err := openDatabase(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer d1.close()
	if err = d1.initialize(); err != nil {
		t.Fatal(err)
	}
	d2, err := openDatabase(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.close()
	i1, i2 := testInference(t, d1), testInference(t, d2)
	i1.dailyUnits, i2.dailyUnits = 10, 10
	if err = i1.reserve("answer", 6); err != nil {
		t.Fatal(err)
	}
	if err = i2.reserve("rerank", 5); err == nil {
		t.Fatal("cross-connection input budget exceeded")
	}
	if err = i2.reserve("rerank", 4); err != nil {
		t.Fatal(err)
	}
	if err = i1.reserve("answer", 1); err == nil {
		t.Fatal("shared budget overflow")
	}
}

func TestRerankAndFailureAccounting(t *testing.T) {
	d := testDB(t)
	loadFixture(t, d)
	i := testInference(t, d)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"results":[{"index":1,"relevance_score":0.9},{"index":0,"relevance_score":0.2}]}`)
	}))
	defer s.Close()
	i.rerankURL = s.URL
	before, err := d.search(SearchRequest{Query: "attention generator", Broad: true, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	v, err := i.rerank(context.Background(), SearchRequest{Query: "attention generator", Limit: 2})
	if err != nil || len(v.Documents) != 2 || v.Documents[0].ID != before.Documents[1].ID || v.ModelCalls != 1 {
		t.Fatalf("%+v %v", v, err)
	}
	s.Close()
	if _, err = i.rerank(context.Background(), SearchRequest{Query: "attention", Limit: 1}); err == nil {
		t.Fatal("network failure accepted")
	}
	r, err := i.budget.rows("SELECT calls FROM usage WHERE kind='rerank'")
	if err != nil || len(r) != 1 || r[0][0] != "2" {
		t.Fatal("failed call not accounted", r)
	}
}

func TestHTTPOriginHostAndNoFiles(t *testing.T) {
	d := testDB(t)
	loadFixture(t, d)
	i := testInference(t, d)
	h := handler(d, i, "127.0.0.1:8094")
	for _, tc := range []struct {
		path, host, origin string
		status             int
	}{{"/api/search", "127.0.0.1:8094", "http://127.0.0.1:8094", 200}, {"/api/search", "127.0.0.1:8094", "https://evil.example", 403}, {"/api/search", "127.0.0.1:8094", "", 403}, {"/api/search", "evil.example", "http://evil.example", 403}, {"/api/files", "127.0.0.1:8094", "http://127.0.0.1:8094", 404}} {
		r := httptest.NewRequest("POST", "http://"+tc.host+tc.path, strings.NewReader(`{"query":"attention"}`))
		r.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%+v: %d %s", tc, w.Code, w.Body.String())
		}
	}
	if err := serve(d, i, "0.0.0.0:0"); err == nil {
		t.Fatal("public bind accepted")
	}
	if _, err := openMetadata("library.pdf"); err == nil {
		t.Fatal("PDF input accepted")
	}
}

func BenchmarkKeywordSearch(b *testing.B) {
	d, err := openDatabase(filepath.Join(b.TempDir(), "bench.sqlite"), true)
	if err != nil {
		b.Fatal(err)
	}
	defer d.close()
	if err = d.initialize(); err != nil {
		b.Fatal(err)
	}
	var data strings.Builder
	for n := 0; n < 10000; n++ {
		fmt.Fprintf(&data, "{\"id\":\"2401.%05d\",\"title\":\"Study %d\",\"abstract\":\"Retrieval and reasoning for domain %d\"}\n", n, n, n%100)
	}
	if _, err = importCatalog(d, "arxiv", "jsonl", strings.NewReader(data.String()), 0); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		if _, err = d.search(SearchRequest{Query: "retrieval domain 42"}); err != nil {
			b.Fatal(err)
		}
	}
}

// Stop immediately after a durable checkpoint; the next batch must not commit.
type cancelCheckpoint struct {
	bytes.Buffer
	cancel context.CancelFunc
}

func (w *cancelCheckpoint) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	w.cancel()
	return n, err
}

func TestBatchCancellationPreservesCheckpoint(t *testing.T) {
	d := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	progress := &cancelCheckpoint{cancel: cancel}
	r, err := importCatalogBatched(ctx, d, "arxiv", "jsonl", strings.NewReader(arxivFixture), 0, 2, progress)
	if err != context.Canceled || r.Committed != 2 {
		t.Fatalf("%+v %v", r, err)
	}
	var checkpoint struct {
		Import ImportReport `json:"import_checkpoint"`
	}
	if err = json.Unmarshal(progress.Bytes(), &checkpoint); err != nil || checkpoint.Import.Committed != 2 {
		t.Fatalf("%s %v", progress.Bytes(), err)
	}
	rows, err := d.rows("SELECT documents,missing_summaries FROM catalog_stats WHERE source='arxiv'")
	if err != nil || len(rows) != 1 || rows[0][0] != "2" || rows[0][1] != "0" {
		t.Fatalf("%v %v", rows, err)
	}
	if _, err = d.run("INSERT INTO abstracts(abstracts,rank) VALUES('integrity-check',1)"); err != nil {
		t.Fatal(err)
	}
}

func TestSchemaMigrationAndTransactionalCounters(t *testing.T) {
	d := testDB(t)
	loadFixture(t, d)
	// Mimic version one: document and FTS data exist, counters do not.
	if err := d.exec("DROP TABLE catalog_stats; DROP INDEX documents_doi; PRAGMA user_version=1;"); err != nil {
		t.Fatal(err)
	}
	if err := d.initialize(); err != nil {
		t.Fatal(err)
	}
	rows, err := d.rows("SELECT documents,missing_summaries FROM catalog_stats WHERE source='arxiv'")
	if err != nil || len(rows) != 1 || rows[0][0] != "3" || rows[0][1] != "0" {
		t.Fatalf("%v %v", rows, err)
	}
	update := `{"id":"1706.03762","title":"Missing abstract"}`
	if _, err = importCatalog(d, "arxiv", "jsonl", strings.NewReader(update), 0); err != nil {
		t.Fatal(err)
	}
	rows, _ = d.rows("SELECT documents,missing_summaries FROM catalog_stats WHERE source='arxiv'")
	if rows[0][0] != "3" || rows[0][1] != "1" {
		t.Fatal(rows)
	}
	if _, err = importCatalog(d, "arxiv", "jsonl", strings.NewReader(arxivFixture+"invalid\n"), 0); err == nil {
		t.Fatal("expected rollback")
	}
	rows, _ = d.rows("SELECT documents,missing_summaries FROM catalog_stats WHERE source='arxiv'")
	if rows[0][0] != "3" || rows[0][1] != "1" {
		t.Fatalf("rollback changed counters: %v", rows)
	}
	plan, err := d.rows("EXPLAIN QUERY PLAN SELECT id FROM documents WHERE doi=?", "10.5555/3295222.3295349")
	if err != nil || !strings.Contains(fmt.Sprint(plan), "documents_doi") || strings.Contains(fmt.Sprint(plan), "SCAN documents") {
		t.Fatalf("unindexed DOI lookup: %v %v", plan, err)
	}
	if _, err = d.run("INSERT INTO abstracts(abstracts,rank) VALUES('integrity-check',1)"); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryImportSavesOnlyCommittedRows(t *testing.T) {
	probe, err := openDatabase(":memory:", true)
	if err != nil {
		t.Fatal(err)
	}
	err = probe.sqliteHeapLimit(64 * 1024 * 1024)
	probe.close()
	if err != nil {
		t.Skipf("bounded memory import is unavailable with this SQLite runtime: %v", err)
	}
	path := filepath.Join(t.TempDir(), "saved.sqlite")
	input := filepath.Join(t.TempDir(), "metadata.jsonl")
	if err := os.WriteFile(input, []byte(arxivFixture+"broken\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"import", "--db", path, "--source", "arxiv", "--file", input, "--batch-size", "2", "--memory-mib", "64"}); err == nil {
		t.Fatal("invalid input accepted")
	}
	d, err := openDatabase(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	rows, err := d.rows("SELECT count(*) FROM documents")
	if err != nil || rows[0][0] != "2" {
		t.Fatalf("%v %v", rows, err)
	}
	if _, err = d.run("INSERT INTO abstracts(abstracts,rank) VALUES('integrity-check',1)"); err != nil {
		t.Fatal(err)
	}
	if err = run([]string{"import", "--db", path, "--source", "arxiv", "--file", input, "--memory-mib", "64"}); err == nil {
		t.Fatal("existing snapshot overwritten")
	}
}

func TestSmartSearchFindsSynonymsAndKeepsFilters(t *testing.T) {
	d := testDB(t)
	loadFixture(t, d)
	i := testInference(t, d)
	expandCalls, rerankCalls := 0, 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if r.URL.Path == "/chat" {
			expandCalls++
			json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": `{"queries":["external knowledge generator","attention machine translation","quantum gravity theory"]}`}}}})
		} else {
			rerankCalls++
			docs := req["documents"].([]any)
			if len(docs) != 2 {
				t.Errorf("filters were lost: %v", docs)
			}
			json.NewEncoder(w).Encode(map[string]any{"results": []any{map[string]any{"index": 0, "relevance_score": 0.8}, map[string]any{"index": 1, "relevance_score": 0.7}}})
		}
	}))
	defer s.Close()
	i.answerURL = s.URL + "/chat"
	i.model = "fixture"
	i.rerankURL = s.URL + "/rerank"
	q := SearchRequest{Query: "models supporting fluent synthesis", Source: "arxiv", Category: "cs.CL", Limit: 2}
	baseline, err := d.search(q)
	if err != nil || len(baseline.Documents) != 0 {
		t.Fatalf("%+v %v", baseline, err)
	}
	result, err := i.smartSearch(context.Background(), q)
	if err != nil || len(result.Documents) != 2 || result.ModelCalls != 2 || len(result.Queries) != 4 || expandCalls != 1 || rerankCalls != 1 {
		t.Fatalf("%+v %v", result, err)
	}
	i.dailyCalls = 2
	if _, err = i.smartSearch(context.Background(), q); err == nil || expandCalls != 1 {
		t.Fatal("smart search bypassed shared budget")
	}
}

func TestMemoryServingKeepsModelBudgetDurable(t *testing.T) {
	disk := testDB(t)
	loadFixture(t, disk)
	cache, err := openDatabase(":memory:", true)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.close()
	if err = cache.sqliteHeapLimit(64 * 1024 * 1024); err != nil {
		t.Skipf("bounded cache unavailable: %v", err)
	}
	if err = cache.load(context.Background(), disk); err != nil {
		t.Fatal(err)
	}
	i := testInference(t, disk)
	i.db = cache
	if err = i.reserve("answer", 100); err != nil {
		t.Fatal(err)
	}
	rows, err := i.budget.rows("SELECT calls FROM usage WHERE kind='answer'")
	if err != nil || len(rows) != 1 || rows[0][0] != "1" {
		t.Fatalf("lost persistent budget: %v %v", rows, err)
	}
	rows, err = cache.rows("SELECT calls FROM usage WHERE kind='answer'")
	if err != nil || len(rows) != 0 {
		t.Fatalf("budget leaked into volatile storage: %v %v", rows, err)
	}
	request := httptest.NewRequest("GET", "http://127.0.0.1:8094/api/stats", nil)
	reply := httptest.NewRecorder()
	handler(cache, i, "127.0.0.1:8094").ServeHTTP(reply, request)
	var stats struct {
		Catalog struct {
			Calls int `json:"model_calls_today"`
		} `json:"catalog"`
	}
	if err = json.Unmarshal(reply.Body.Bytes(), &stats); err != nil || stats.Catalog.Calls != 1 {
		t.Fatalf("stale usage: %s %v", reply.Body.String(), err)
	}
}

func TestCLIUsesLoopbackServerAndRejectsRemoteOrigins(t *testing.T) {
	d := testDB(t)
	loadFixture(t, d)
	i := testInference(t, d)
	s := httptest.NewServer(nil)
	defer s.Close()
	s.Config.Handler = handler(d, i, strings.TrimPrefix(s.URL, "http://"))
	result, err := researchServerRequest(s.URL, "search", SearchRequest{Query: "attention"})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(result)
	var found SearchResponse
	if err = json.Unmarshal(data, &found); err != nil || len(found.Documents) != 1 {
		t.Fatalf("%s %v", data, err)
	}
	for _, origin := range []string{"http://example.com", "http://127.0.0.1/private", "http://user:password@127.0.0.1", "https://127.0.0.1", "http://127.0.0.1?path=evil"} {
		if _, err = researchServerRequest(origin, "stats", nil); err == nil {
			t.Fatalf("accepted origin %s", origin)
		}
	}
}

func TestUsageMigrationKeepsCatalogUnchanged(t *testing.T) {
	d := testDB(t)
	loadFixture(t, d)
	day := time.Now().UTC().Format("2006-01-02")
	if _, err := d.run("INSERT INTO usage(day,kind,calls,units) VALUES(?,'answer',2,100)", day); err != nil {
		t.Fatal(err)
	}
	i := testInference(t, d)
	rows, err := i.budget.rows("SELECT calls,units FROM usage WHERE kind='answer'")
	if err != nil || len(rows) != 1 || rows[0][0] != "2" || rows[0][1] != "100" {
		t.Fatalf("legacy usage lost: %v %v", rows, err)
	}
	if err = i.reserve("answer", 20); err != nil {
		t.Fatal(err)
	}
	rows, err = d.rows("SELECT calls,units FROM usage WHERE kind='answer'")
	if err != nil || len(rows) != 1 || rows[0][0] != "2" || rows[0][1] != "100" {
		t.Fatalf("catalog changed: %v %v", rows, err)
	}
	restarted := testInference(t, d)
	rows, err = restarted.budget.rows("SELECT calls,units FROM usage WHERE kind='answer'")
	if err != nil || len(rows) != 1 || rows[0][0] != "3" || rows[0][1] != "120" {
		t.Fatalf("migration replay reset budget: %v %v", rows, err)
	}
	next, err := openDatabase(filepath.Join(filepath.Dir(d.path), "next-snapshot.sqlite"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer next.close()
	if err = next.initialize(); err != nil {
		t.Fatal(err)
	}
	other := testInference(t, next)
	other.dailyCalls = 3
	if err = other.reserve("answer", 1); err == nil {
		t.Fatal("new snapshot file reset the shared ledger")
	}
}

func TestEvidenceQuotesAreExactAndBounded(t *testing.T) {
	summary := "An exact sequence of\nwords supports this claim."
	if _, err := verifyEvidenceQuote(summary, "An exact sequence of words supports this claim."); err != nil {
		t.Fatal(err)
	}
	for _, quote := range []string{"An invented sequence of words supports this claim.", "An exact", strings.Repeat("word ", 26)} {
		if _, err := verifyEvidenceQuote(summary, quote); err == nil {
			t.Fatalf("accepted invalid quote %q", quote)
		}
	}
}

func TestHypothesisPlanRequiresControlsAndAssumptions(t *testing.T) {
	plan := &hypothesisPlan{
		Mechanism: "A speculative mechanism.", Intervention: "Mask retrieval attention.",
		Baseline: "Keep retrieval attention unchanged.", Measurement: "Measure factual answer accuracy.",
		FailureCondition: "No improvement over baseline.", ExistingOverlap: "Both already use attention.",
		Assumptions: "Confirm the implementation in the full papers.",
	}
	out, err := renderHypothesis(plan)
	if err != nil || !strings.Contains(out, "Existing overlap:") || !strings.Contains(out, "Assumptions to verify:") {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := renderHypothesis(nil); err == nil {
		t.Fatal("missing plan accepted")
	}
	plan.Baseline = " "
	if _, err := renderHypothesis(plan); err == nil {
		t.Fatal("missing control accepted")
	}
}

func TestExcerptChoicesAreVerifiedSourceSubstrings(t *testing.T) {
	for _, summary := range []string{
		"A transformer replaces recurrence with attention for machine translation.",
		strings.Repeat("Precision retrieval can improve grounded answers with supporting source context. ", 12),
	} {
		choices := evidenceExcerptChoices(summary)
		if len(choices) < 1 || len(choices) > 3 {
			t.Fatalf("invalid choices: %v", choices)
		}
		for _, quote := range choices {
			if _, err := verifyEvidenceQuote(summary, quote); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(evidenceExcerptChoices("Too short")) != 0 {
		t.Fatal("short excerpt accepted")
	}
}
