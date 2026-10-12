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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/parquet-go/parquet-go"
)

func indexFixtureStore(t *testing.T, objects map[string][]byte) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/synthetic/")
		data, ok := objects[key]
		if !ok {
			w.WriteHeader(404)
			return
		}
		if !strings.Contains(r.Header.Get("Authorization"), "Credential=fixture-access/") {
			t.Error("missing worker signature")
		}
		etag := `"` + textSHA(data) + `"`
		if match := r.Header.Get("If-Match"); match != "" && match != etag {
			w.WriteHeader(412)
			return
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		if r.Method == "HEAD" {
			return
		}
		if value := r.Header.Get("Range"); value != "" {
			var start, end int
			fmt.Sscanf(value, "bytes=%d-%d", &start, &end)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
			w.Header().Set("Content-Length", strconv.Itoa(end-start+1))
			w.WriteHeader(206)
			w.Write(data[start : end+1])
			return
		}
		w.Write(data)
	}))
	t.Cleanup(s.Close)
	t.Setenv("RESEARCH_INDEX_TEST_ACCESS", "fixture-access")
	t.Setenv("RESEARCH_INDEX_TEST_SECRET", "fixture-secret")
	return s
}
func indexFixtureManager(t *testing.T, server *httptest.Server, adapter, key string) (*indexManager, string) {
	t.Helper()
	root := t.TempDir()
	base := filepath.Join(root, "original.sqlite")
	d, e := openDatabase(base, true)
	if e != nil {
		t.Fatal(e)
	}
	if e = d.initialize(); e != nil {
		t.Fatal(e)
	}
	for _, doc := range []Document{{ID: "2301.00001", Source: "arxiv", Title: "Old metadata", Summary: "old abstract"}, {ID: "1", Source: "gutenberg", Title: "Unselected book", NASRef: "s3://synthetic/retained.epub"}} {
		v, e := normalize(doc)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = d.put(v); e != nil {
			t.Fatal(e)
		}
	}
	d.close()
	c := indexConfig{Collections: []indexCollection{{ID: "papers", Label: "Papers", Source: "arxiv", Adapter: adapter, Endpoint: server.URL, Bucket: "synthetic", Key: key, AccessEnv: "RESEARCH_INDEX_TEST_ACCESS", SecretEnv: "RESEARCH_INDEX_TEST_SECRET"}}, Admins: []string{"loopback-operator"}, MaxObjects: 10, MaxCatalogMiB: 64, DocumentsPerSecond: 1}
	config := filepath.Join(root, "config.json")
	if e = writeIndexJSON(config, c); e != nil {
		t.Fatal(e)
	}
	t.Setenv("RESEARCH_INDEX_CONFIG", config)
	t.Setenv("RESEARCH_INDEX_ROOT", filepath.Join(root, "indexing"))
	t.Setenv("RESEARCH_INDEX_ORIGINAL_CATALOG", base)
	t.Setenv("RESEARCH_CATALOG_SHA256", "")
	t.Setenv("RESEARCH_SEMANTIC_STATUS", "")
	t.Setenv("RESEARCH_EMBEDDING_SHA256", "")
	m, e := newIndexManager(base, nil)
	if e != nil {
		t.Fatal(e)
	}
	return m, base
}
func TestIndexingBuildValidateActivateRollback(t *testing.T) {
	rows := `{"id":"2401.00001","title":"Geology rocks","abstract":"Geology explains rock formation."}` + "\n" + `{"id":"2401.00002","title":"Plate tectonics","abstract":"Geology studies moving plates."}` + "\n" + `{"id":"2401.00001","title":"Geology rocks","abstract":"Geology explains rock formation."}` + "\n"
	server := indexFixtureStore(t, map[string][]byte{"papers.jsonl": []byte(rows)})
	m, base := indexFixtureManager(t, server, "jsonl", "papers.jsonl")
	original, _ := indexFileSHA(base)
	sessionPath := filepath.Join(filepath.Dir(base), "research-desk.sqlite")
	os.WriteFile(sessionPath, []byte("sessions-must-survive"), 0600)
	usagePath := filepath.Join(filepath.Dir(base), "research-usage.sqlite")
	os.WriteFile(usagePath, []byte("ledger-must-survive"), 0600)
	s, e := m.submit("rebuild", []string{"papers"}, false)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = m.submit("inventory", []string{"papers"}, false); e == nil {
		t.Fatal("competing run accepted")
	}
	var run indexRun
	if e = readIndexJSON(m.runPath(s.ID, "request.json"), &run); e != nil {
		t.Fatal(e)
	}
	if e = m.execute(context.Background(), run, &s); e != nil {
		t.Fatal(e)
	}
	if s.Status != "ready" || s.Read != 3 || s.Duplicates != 1 || s.Candidate == nil {
		t.Fatalf("invalid state %+v", s)
	}
	d, e := openDatabase(s.Candidate.Catalog, false)
	if e != nil {
		t.Fatal(e)
	}
	search, e := d.search(SearchRequest{Query: "geology", Limit: 5})
	if e != nil || len(search.Documents) != 2 {
		t.Fatalf("abstract search: %+v %v", search, e)
	}
	r, _ := d.rows("SELECT count(*) FROM documents WHERE id='arxiv:2301.00001'")
	if r[0][0] != "0" {
		t.Fatal("removed record remains")
	}
	r, _ = d.rows("SELECT nas_ref FROM documents WHERE source='gutenberg'")
	if len(r) != 1 || r[0][0] != "s3://synthetic/retained.epub" {
		t.Fatal("unselected library link changed")
	}
	d.close()
	switched := false
	m.restart = func() { switched = true }
	if e = m.activate(s.ID, false); e != nil {
		t.Fatal(e)
	}
	if !switched {
		t.Fatal("reload was not requested")
	}
	selected, e := resolveIndexCatalog(base)
	if e != nil || selected != s.Candidate.Catalog {
		t.Fatal(selected, e)
	}
	m.reloading.Store(false) // Simulate the new server after its graceful reload.
	if e = m.activate("", true); e != nil {
		t.Fatal(e)
	}
	selected, e = resolveIndexCatalog(base)
	if e != nil || selected != base {
		t.Fatal("rollback failed", e)
	}
	after, _ := indexFileSHA(base)
	if after != original {
		t.Fatal("original catalog changed")
	}
	for path, want := range map[string]string{sessionPath: "sessions-must-survive", usagePath: "ledger-must-survive"} {
		b, _ := os.ReadFile(path)
		if string(b) != want {
			t.Fatal("durable research data changed")
		}
	}
}
func TestIndexingPauseResumeAndSnapshotChange(t *testing.T) {
	objects := map[string][]byte{"papers.jsonl": []byte(`{"id":"2401.00001","title":"Geology","abstract":"Complete abstract."}` + "\n")}
	server := indexFixtureStore(t, objects)
	m, _ := indexFixtureManager(t, server, "jsonl", "papers.jsonl")
	s, e := m.submit("rebuild", []string{"papers"}, false)
	if e != nil {
		t.Fatal(e)
	}
	m.control(s.ID, "pause")
	var run indexRun
	readIndexJSON(m.runPath(s.ID, "request.json"), &run)
	if e = m.execute(context.Background(), run, &s); e != errIndexPause {
		t.Fatal("pause ignored", e)
	}
	m.control(s.ID, "resume")
	if e = m.execute(context.Background(), run, &s); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(s.Candidate.Catalog)
	b[100] ^= 1
	os.WriteFile(s.Candidate.Catalog, b, 0600)
	m.restart = func() {}
	if e = m.activate(s.ID, false); e == nil {
		t.Fatal("changed candidate activated")
	}
}
func TestIndexingConflictingDuplicatesFailClosed(t *testing.T) {
	rows := `{"id":"2401.00001","title":"First","abstract":"a"}` + "\n" + `{"id":"2401.00001","title":"Conflicting","abstract":"b"}` + "\n"
	server := indexFixtureStore(t, map[string][]byte{"papers.jsonl": []byte(rows)})
	m, base := indexFixtureManager(t, server, "jsonl", "papers.jsonl")
	sha, _ := indexFileSHA(base)
	s, _ := m.submit("rebuild", []string{"papers"}, false)
	var run indexRun
	readIndexJSON(m.runPath(s.ID, "request.json"), &run)
	if e := m.execute(context.Background(), run, &s); e == nil {
		t.Fatal("conflicting snapshot accepted")
	}
	after, _ := indexFileSHA(base)
	if sha != after {
		t.Fatal("failure changed active catalog")
	}
}
func TestIndexingParquetProjectionAndManifest(t *testing.T) {
	var b bytes.Buffer
	w := parquet.NewGenericWriter[indexPaper](&b)
	title, authors, abstract, category, doi := "Geology study", "Synthetic author", "A complete supplied geology abstract.", "physics.geo-ph", ""
	date := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	_, e := w.Write([]indexPaper{{ID: "2401.00001", Title: &title, Authors: &authors, Abstract: &abstract, Categories: &category, DOI: &doi, Date: &date}})
	if e != nil {
		t.Fatal(e)
	}
	if e = w.Close(); e != nil {
		t.Fatal(e)
	}
	manifest := []byte(`{"counts":{"targets":1},"entries":[{"gutenberg_id":2,"title":"Synthetic book","authors":"Author","s3_uri":"s3://synthetic/book.epub"}]}`)
	server := indexFixtureStore(t, map[string][]byte{"metadata.parquet": b.Bytes(), "books.json": manifest})
	m, _ := indexFixtureManager(t, server, "arxiv-parquet", "metadata.parquet")
	s, _ := m.submit("rebuild", []string{"papers"}, false)
	var run indexRun
	readIndexJSON(m.runPath(s.ID, "request.json"), &run)
	if e = m.execute(context.Background(), run, &s); e != nil {
		t.Fatal(e)
	}
	if s.Read != 1 {
		t.Fatal(s.Read)
	}
	a, _ := collectionStore(m.config.Collections[0])
	o, _ := indexHead(context.Background(), a, "books.json")
	v, e := gutenbergEntries(context.Background(), a, o)
	if e != nil || len(v) != 1 || v[0].Summary != "" || v[0].NASRef != "s3://synthetic/book.epub" {
		t.Fatal("manifest should preserve missing description and saved file link", e)
	}
}
func TestIndexingAdminAndBrowserRedaction(t *testing.T) {
	server := indexFixtureStore(t, map[string][]byte{"private-key.jsonl": []byte(`{"id":"2401.00001","title":"Synthetic","abstract":"supplied"}` + "\n")})
	m, _ := indexFixtureManager(t, server, "jsonl", "private-key.jsonl")
	s, _ := m.submit("inventory", []string{"papers"}, false)
	var run indexRun
	readIndexJSON(m.runPath(s.ID, "request.json"), &run)
	if e := m.execute(context.Background(), run, &s); e != nil {
		t.Fatal(e)
	}
	mux := http.NewServeMux()
	m.routes(mux)
	req := httptest.NewRequest("GET", "/api/library/indexing", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatal("anonymous indexing allowed")
	}
	req = req.WithContext(context.WithValue(req.Context(), ownerKey{}, "loopback-operator"))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	for _, secret := range []string{"private-key.jsonl", "fixture-access", "fixture-secret", server.URL, m.root} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("private configuration reached browser")
		}
	}
	var response map[string]any
	if e := json.Unmarshal(w.Body.Bytes(), &response); e != nil {
		t.Fatal(e)
	}
	req = httptest.NewRequest("POST", "/api/library/indexing", strings.NewReader(`{"kind":"rebuild","collections":["../../escape"]}`)).WithContext(context.WithValue(context.Background(), ownerKey{}, "loopback-operator"))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code < 400 {
		t.Fatal("unconfigured collection accepted")
	}
}
func TestIndexingBatchCheckpointSurvivesJSONLag(t *testing.T) {
	var rows strings.Builder
	for n := 0; n < 501; n++ {
		fmt.Fprintf(&rows, "{\"id\":\"2401.%05d\",\"title\":\"Geology %d\",\"abstract\":\"Supplied abstract\"}\n", n, n)
	}
	server := indexFixtureStore(t, map[string][]byte{"papers.jsonl": []byte(rows.String())})
	m, _ := indexFixtureManager(t, server, "jsonl", "papers.jsonl")
	s, _ := m.submit("rebuild", []string{"papers"}, false)
	var run indexRun
	readIndexJSON(m.runPath(s.ID, "request.json"), &run)
	// Stop at the first durable batch using cancellation from a status reader.
	a, _ := collectionStore(m.config.Collections[0])
	o, _ := indexHead(context.Background(), a, "papers.jsonl")
	o.Rows = 501
	accepted := 0
	e := streamIndexObject(context.Background(), a, m.config.Collections[0], o, 500, func(batch []Document, cursor int64) error {
		accepted += len(batch)
		if cursor != 501 {
			t.Fatal("resume cursor", cursor)
		}
		return nil
	})
	if e != nil || accepted != 1 {
		t.Fatal("resume skipped or duplicated rows", e)
	}
	if e = m.execute(context.Background(), run, &s); e != nil {
		t.Fatal(e)
	}
	if s.Read != 501 || s.Duplicates != 0 {
		t.Fatal("checkpoint accounting", s.Read, s.Duplicates)
	}
}

func TestIndexingCompleteMeaningAndIndependentCount(t *testing.T) {
	for _, wrongCount := range []bool{false, true} {
		t.Run(fmt.Sprint(wrongCount), func(t *testing.T) {
			papers := []byte(`{"id":"2401.00001","title":"Geology","abstract":"` + strings.Repeat("Grounded abstract text. ", 45) + `"}` + "\n")
			storage := indexFixtureStore(t, map[string][]byte{"papers.jsonl": papers})
			m, _ := indexFixtureManager(t, storage, "jsonl", "papers.jsonl")
			vectorIDs := map[string]bool{}
			embeddings := 0
			engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" {
					t.Error("chat credentials reached indexing engine")
				}
				var body map[string]any
				if r.Method == "POST" {
					json.NewDecoder(r.Body).Decode(&body)
				}
				switch r.URL.Path {
				case "/health":
					writeJSON(w, 200, semanticEngine())
				case "/tokenize":
					text := body["input"].(string)
					offsets := [][]int{}
					for n := range []rune(text) {
						offsets = append(offsets, []int{n, n + 1})
					}
					writeJSON(w, 200, map[string]any{"input_sha256": textSHA([]byte(text)), "max_input_tokens": 128, "special_tokens": 2, "offsets": offsets})
				case "/embeddings":
					embeddings++
					if body["truncate"] != false {
						t.Error("truncation requested")
					}
					counts := []int{}
					data := []any{}
					for n, text := range body["input"].([]any) {
						v := make([]float64, semanticDimensions)
						v[0] = 1
						counts = append(counts, len([]rune(text.(string)))+2)
						data = append(data, map[string]any{"index": n, "embedding": v})
					}
					writeJSON(w, 200, map[string]any{"model": "bge-m3", "input_contract": map[string]any{"truncated": false, "pooling": "cls", "max_input_tokens": 128, "input_tokens": counts}, "data": data})
				case "/v2/vectordb/collections/has":
					writeJSON(w, 200, map[string]any{"code": 0, "data": map[string]bool{"has": false}})
				case "/v2/vectordb/entities/upsert":
					ids := []string{}
					for _, row := range body["data"].([]any) {
						id := row.(map[string]any)["id"].(string)
						ids = append(ids, id)
						vectorIDs[id] = true
					}
					writeJSON(w, 200, map[string]any{"code": 0, "data": map[string]any{"upsertCount": len(ids), "upsertIds": ids}})
				case "/v2/vectordb/entities/query":
					count := len(vectorIDs)
					if wrongCount {
						count++
					}
					writeJSON(w, 200, map[string]any{"code": 0, "data": []any{map[string]int{"count(*)": count}}})
				default:
					if !strings.HasPrefix(r.URL.Path, "/v2/vectordb/") {
						t.Error("unexpected model route")
					}
					writeJSON(w, 200, map[string]any{"code": 0, "data": map[string]any{}})
				}
			}))
			defer engine.Close()
			m.config.EmbeddingOrigin = engine.URL
			m.config.VectorOrigin = engine.URL
			m.config.ModelSHA = strings.Repeat("b", 64)
			m.config.DocumentsPerSecond = 30
			s, e := m.submit("rebuild", []string{"papers"}, true)
			if e != nil {
				t.Fatal(e)
			}
			var run indexRun
			readIndexJSON(m.runPath(s.ID, "request.json"), &run)
			e = m.execute(context.Background(), run, &s)
			if wrongCount {
				if e == nil || s.Status == "ready" {
					t.Fatal("unverified vector coverage accepted")
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			if embeddings == 0 || s.Status != "ready" || s.Vectors != 1 || s.Candidate.Semantic == "" {
				t.Fatal("meaning workflow incomplete")
			}
			var coverage SemanticCoverage
			if e = readIndexJSON(s.Candidate.Semantic, &coverage); e != nil || coverage.State != "complete" || coverage.VerifiedCount != 1 {
				t.Fatal("meaning receipt", e)
			}
		})
	}
}
func TestIndexingSQLiteCheckpointOverridesStaleStatus(t *testing.T) {
	papers := []byte(`{"id":"2401.00001","title":"First geology","abstract":"Supplied evidence."}` + "\n" + `{"id":"2401.00002","title":"Second geology","abstract":"Other evidence."}` + "\n")
	storage := indexFixtureStore(t, map[string][]byte{"papers.jsonl": papers})
	m, _ := indexFixtureManager(t, storage, "jsonl", "papers.jsonl")
	s, _ := m.submit("rebuild", []string{"papers"}, false)
	var run indexRun
	readIndexJSON(m.runPath(s.ID, "request.json"), &run)
	a, _ := collectionStore(m.config.Collections[0])
	o, _ := indexHead(context.Background(), a, "papers.jsonl")
	o.Rows = 2
	s.Inventory = []indexInventory{{Collection: "papers", Objects: []indexObject{o}, Records: 2, ListingComplete: true}}
	s.Phase = "keyword"
	path := m.runPath(s.ID, "catalog.sqlite")
	if e := m.prepareCandidate(context.Background(), run, &s, path, m.config.Collections); e != nil {
		t.Fatal(e)
	}
	d, e := openDatabase(path, false)
	if e != nil {
		t.Fatal(e)
	}
	first, _ := normalize(Document{ID: "2401.00001", Source: "arxiv", Title: "First geology", Summary: "Supplied evidence."})
	d.exec("BEGIN IMMEDIATE")
	d.put(first)
	s.Read = 1
	s.Row = 1
	encoded, _ := json.Marshal(s)
	d.run("INSERT OR REPLACE INTO indexing_checkpoint VALUES(1,?)", string(encoded))
	d.exec("COMMIT")
	d.close()
	stale := s
	stale.Read = 0
	stale.Row = 0
	writeIndexJSON(m.runPath(s.ID, "state.json"), stale)
	if e = m.execute(context.Background(), run, &stale); e != nil {
		t.Fatal(e)
	}
	if stale.Read != 2 || stale.Duplicates != 0 || stale.Status != "ready" {
		t.Fatal("SQLite checkpoint lost or replay counted twice")
	}
}
