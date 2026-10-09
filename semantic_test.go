package main

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func semanticFixture(t *testing.T) (*researchDesk, *inference, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	s, i := deskFixture(t)
	calls, searches := &atomic.Int32{}, &atomic.Int32{}
	docs, err := i.db.getDocuments([]string{"arxiv:1706.03762"})
	if err != nil {
		t.Fatal(err)
	}
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("chat credential forwarded to semantic service")
		}
		switch r.URL.Path {
		case "/health":
			writeJSON(w, 200, semanticEngine())
		case "/tokenize":
			var body struct {
				Input string `json:"input"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			offsets := [][]int{}
			for n := range []rune(body.Input) {
				offsets = append(offsets, []int{n, n + 1})
			}
			writeJSON(w, 200, map[string]any{"input_sha256": textSHA([]byte(body.Input)), "max_input_tokens": 128, "special_tokens": 2, "offsets": offsets})
		case "/embeddings":
			calls.Add(1)
			var body struct {
				Input    []string `json:"input"`
				Truncate bool     `json:"truncate"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Truncate {
				t.Error("query truncation enabled")
			}
			counts, data := []int{}, []any{}
			for n, text := range body.Input {
				v := make([]float64, semanticDimensions)
				v[0] = 1
				counts = append(counts, len([]rune(text))+2)
				data = append(data, map[string]any{"index": n, "embedding": v})
			}
			writeJSON(w, 200, map[string]any{"model": "bge-m3", "input_contract": map[string]any{"truncated": false, "pooling": "cls", "max_input_tokens": 128, "input_tokens": counts}, "data": data})
		case "/v2/vectordb/entities/search":
			searches.Add(1)
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["collectionName"] != i.semantic.collection || body["consistencyLevel"] != "Strong" {
				t.Error("wrong collection or consistency")
			}
			writeJSON(w, 200, map[string]any{"code": 0, "data": []any{map[string]any{"id": docs[0].ID, "fingerprint": docs[0].Fingerprint, "distance": .84}}})
		default:
			t.Error("unexpected semantic service path")
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(engine.Close)
	i.semantic = &semanticClient{embeddingOrigin: engine.URL, milvusOrigin: engine.URL, statusPath: filepath.Join(t.TempDir(), "status.json"), catalogSHA: strings.Repeat("a", 64), modelSHA: strings.Repeat("b", 64), total: 3, client: i.client}
	i.semantic.collection = semanticCollection(i.semantic.catalogSHA, i.semantic.modelSHA)
	writeSemanticStatus(t, i, "indexing", 1)
	i.key = "chat-only-secret"
	return s, i, calls, searches
}

func writeSemanticStatus(t *testing.T, i *inference, state string, indexed int) {
	t.Helper()
	v := SemanticCoverage{Algorithm: semanticAlgorithm, CatalogSHA: i.semantic.catalogSHA, ModelSHA: i.semantic.modelSHA, Collection: i.semantic.collection, Dimensions: semanticDimensions, Engine: semanticEngine(), Indexed: indexed, Total: i.semantic.total, State: state, Updated: time.Now().UTC().Format(time.RFC3339)}
	if state == "complete" {
		v.VerifiedCount = indexed
	}
	b, _ := json.Marshal(v)
	if err := os.WriteFile(i.semantic.statusPath, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSemanticCollectionMatchesRealIndexAndWindows(t *testing.T) {
	if got := semanticCollection("51327619abec6e227e3b772f31c610dc9a4fbfadbabda8a7154adffbecb1fbeb", "9351947e61cdb54e8acdb85a9c16e154915c0320a6a1ade91eaccadc327b4656"); got != "research_arxiv_bgem3_82e8e7d2d18d" {
		t.Fatal(got)
	}
	query := strings.Repeat("岩", 240)
	offsets := [][]int{}
	for n := range []rune(query) {
		offsets = append(offsets, []int{n, n + 1})
	}
	windows, err := exactQueryWindows(query, textSHA([]byte(query)), 128, 2, offsets)
	if err != nil || len(windows) != 3 {
		t.Fatal(err, len(windows))
	}
	total := 0
	for _, w := range windows {
		total += w.weight
		if len([]rune(w.text)) > 112 {
			t.Fatal("oversize window")
		}
	}
	if total != 240 {
		t.Fatal("incomplete query coverage")
	}
	if _, err := exactQueryWindows(query, "changed", 128, 2, offsets); err == nil {
		t.Fatal("wrong input hash accepted")
	}
	offsets[4] = []int{4, 999}
	if _, err := exactQueryWindows(query, textSHA([]byte(query)), 128, 2, offsets); err == nil {
		t.Fatal("invalid offsets accepted")
	}
	v := make([]float64, semanticDimensions)
	v[0] = 1
	if !validSemanticVector(v) {
		t.Fatal("valid vector rejected")
	}
	v[1] = math.NaN()
	if validSemanticVector(v) {
		t.Fatal("nonfinite vector accepted")
	}
}

func TestSemanticJobsPersistAndReuseEmbeddingWhileIndexGrows(t *testing.T) {
	s, i, calls, searches := semanticFixture(t)
	data := SessionData{Title: "An earth-science query", Task: "explain", Search: SearchRequest{Query: "earthquake wave propagation", Meaning: true, Limit: 10}}
	x, err := s.save("owner", "", 0, data)
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.submit("owner", x.ID, x.Revision, "semantic-search")
	if err != nil {
		t.Fatal(err)
	}
	j = awaitDeskJob(t, s, "owner", j.ID)
	if j.Status != "complete" {
		t.Fatal(j.Error)
	}
	var first SearchResponse
	_ = json.Unmarshal(j.Result, &first)
	if first.ModelCalls != 1 || first.EmbeddingCached || first.Coverage.Indexed != 1 || len(first.Documents) != 1 || first.Documents[0].NASRef != "" {
		t.Fatalf("first: %+v", first)
	}
	rows, err := s.db.rows("SELECT count(*) FROM desk_cache")
	if err != nil || rows[0][0] != "0" {
		t.Fatal("partial result entered immutable full-result cache")
	}
	writeSemanticStatus(t, i, "indexing", 2)
	j, err = s.submit("owner", x.ID, x.Revision, "semantic-search")
	if err != nil {
		t.Fatal(err)
	}
	j = awaitDeskJob(t, s, "owner", j.ID)
	var repeat SearchResponse
	_ = json.Unmarshal(j.Result, &repeat)
	if j.Cached || repeat.ModelCalls != 0 || !repeat.EmbeddingCached || repeat.Coverage.Indexed != 2 || calls.Load() != 1 || searches.Load() != 2 {
		t.Fatalf("repeat: %+v calls %d searches %d", repeat, calls.Load(), searches.Load())
	}
	writeSemanticStatus(t, i, "complete", 3)
	j, err = s.submit("owner", x.ID, x.Revision, "semantic-search")
	if err != nil {
		t.Fatal(err)
	}
	j = awaitDeskJob(t, s, "owner", j.ID)
	if j.Status != "complete" {
		t.Fatal(j.Error)
	}
	j, err = s.submit("owner", x.ID, x.Revision, "semantic-search")
	if err != nil || !j.Cached || searches.Load() != 3 || calls.Load() != 1 {
		t.Fatal("complete index did not reuse full result", err)
	}
	if _, err := s.job("other", j.ID); err == nil {
		t.Fatal("cross-account result read")
	}
	copy, err := openDatabase(s.db.path, false)
	_, _ = i.db.run("UPDATE documents SET fingerprint=? WHERE id=?", strings.Repeat("f", 64), first.Documents[0].ID)
	if _, checkErr := s.submit("owner", x.ID, x.Revision, "semantic-search"); checkErr == nil {
		t.Fatal("complete-result cache accepted changed source")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer copy.close()
	rows, err = copy.rows("SELECT count(*) FROM desk_embeddings WHERE owner='owner'")
	if err != nil || rows[0][0] != "1" {
		t.Fatal("query embedding did not persist")
	}
}

func TestSemanticFingerprintDriftAndCorruptCache(t *testing.T) {
	s, i, calls, _ := semanticFixture(t)
	q := SearchRequest{Query: "earthquake", Limit: 10, Meaning: true}
	first, err := s.semanticSearchJob(context.Background(), "owner", q, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if first.ModelCalls != 1 {
		t.Fatal("embedding did not reserve")
	}
	_, _ = i.db.run("UPDATE documents SET fingerprint=? WHERE id=?", strings.Repeat("f", 64), first.Documents[0].ID)
	if _, err := s.semanticSearchJob(context.Background(), "owner", q, func(string) {}); err == nil {
		t.Fatal("wrong source fingerprint accepted")
	}
	if calls.Load() != 1 {
		t.Fatal("cached query made another model call")
	}
	_, _ = s.db.run("UPDATE desk_embeddings SET vector='[1]' WHERE owner='owner'")
	if _, err := s.semanticSearchJob(context.Background(), "owner", q, func(string) {}); err == nil {
		t.Fatal("invalid saved vector accepted")
	}
	writeSemanticStatus(t, i, "complete", 1)
	if _, err := i.semantic.coverage(); err == nil {
		t.Fatal("partial count passed as complete")
	}
}

func TestSemanticModelDriftAndEscapedFilters(t *testing.T) {
	s, i, calls, _ := semanticFixture(t)
	q := SearchRequest{Query: "earthquake", Limit: 10, Meaning: true}
	if _, err := s.semanticSearchJob(context.Background(), "owner", q, func(string) {}); err != nil {
		t.Fatal(err)
	}
	i.semantic.modelSHA = strings.Repeat("c", 64)
	i.semantic.collection = semanticCollection(i.semantic.catalogSHA, i.semantic.modelSHA)
	if _, err := i.semantic.coverage(); err == nil {
		t.Fatal("changed model accepted old index")
	}
	writeSemanticStatus(t, i, "indexing", 1)
	if _, err := s.semanticSearchJob(context.Background(), "owner", q, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("changed embedding identity reused old query vector")
	}
	if _, err := semanticFilter(SearchRequest{Category: `geo-ph%" or id != "`}); err == nil {
		t.Fatal("filter injection accepted")
	}
	if _, err := semanticFilter(SearchRequest{Source: "gutenberg"}); err == nil {
		t.Fatal("book search silently used arXiv vectors")
	}
	if _, err := serviceOrigin("http://user:key@example.com"); err == nil {
		t.Fatal("credential URL accepted")
	}
}

func TestSemanticCatalogFileIsActuallyHashed(t *testing.T) {
	d := testDB(t)
	loadFixture(t, d)
	b, err := os.ReadFile(d.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("RESEARCH_EMBEDDING_ORIGIN", "http://127.0.0.1:12345")
	t.Setenv("RESEARCH_VECTOR_ORIGIN", "http://127.0.0.1:12346")
	t.Setenv("RESEARCH_SEMANTIC_STATUS", filepath.Join(t.TempDir(), "status.json"))
	t.Setenv("RESEARCH_CATALOG_SHA256", textSHA(b))
	t.Setenv("RESEARCH_EMBEDDING_SHA256", strings.Repeat("b", 64))
	i, err := newInference(d)
	if err != nil {
		t.Fatal(err)
	}
	defer i.close()
	if i.semantic.total != 3 {
		t.Fatal("catalog count does not match snapshot")
	}
	writeSemanticStatus(t, i, "indexing", 1)
	if _, err = i.semantic.coverage(); err != nil {
		t.Fatal(err)
	}
	_, _ = d.run("UPDATE documents SET fingerprint=? WHERE id=?", strings.Repeat("f", 64), "arxiv:1706.03762")
	if _, err = i.semantic.coverage(); err == nil {
		t.Fatal("changed NAS snapshot accepted")
	}
	if _, err = newSemantic(i); err == nil {
		t.Fatal("wrong actual catalog checksum accepted")
	}
}

func TestSemanticCancellationDoesNotReplayEmbedding(t *testing.T) {
	s, i, calls, _ := semanticFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.semanticSearchJob(ctx, "owner", SearchRequest{Query: "earthquake", Limit: 10}, func(string) {}); err == nil {
		t.Fatal("canceled search completed")
	}
	if calls.Load() != 0 {
		t.Fatal("canceled health request called model")
	}
	stats, err := i.stats()
	if err != nil || stats.(map[string]any)["model_calls_today"] != 0 {
		t.Fatal("canceled search charged embedding")
	}
}

func TestSemanticRunningJobCancellationRetainsReservation(t *testing.T) {
	s, i, calls, searches := semanticFixture(t)
	origin := i.semantic.embeddingOrigin
	started, release := make(chan struct{}), make(chan struct{})
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/embeddings" {
			close(started)
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		request, err := http.NewRequestWithContext(r.Context(), r.Method, origin+r.URL.Path, r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Error(err)
			return
		}
		defer response.Body.Close()
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	}))
	defer proxy.Close()
	defer close(release)
	i.semantic.embeddingOrigin = proxy.URL
	data := sessionFixture()
	data.Search = SearchRequest{Query: "earthquake", Meaning: true, Limit: 10}
	x, err := s.save("owner", "", 0, data)
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.submit("owner", x.ID, x.Revision, "semantic-search")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("embedding never started")
	}
	j, err = s.cancelJob("owner", j.ID)
	if err != nil || j.Status != "cancelled" {
		t.Fatal("running semantic job did not cancel", err)
	}
	stats, err := i.stats()
	if err != nil || stats.(map[string]any)["model_calls_today"] != 1 {
		t.Fatal("started embedding lost its reservation", err)
	}
	if calls.Load() != 0 || searches.Load() != 0 {
		t.Fatal("cancelled embedding was replayed or searched")
	}
	rows, err := s.db.rows("SELECT count(*) FROM desk_embeddings")
	if err != nil || rows[0][0] != "0" {
		t.Fatal("cancelled embedding entered the cache")
	}
}
