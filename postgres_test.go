package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestPostgresSQLBindingAndGenerationIsolation(t *testing.T) {
	p := &postgresCatalog{schema: "research_g_" + strings.Repeat("a", 32), baseline: "research_g_" + strings.Repeat("b", 32)}
	s, e := p.sql("SELECT d.id,'documents ? literal' FROM documents d JOIN baseline.documents b ON b.id=d.id WHERE d.rowid>? AND d.id=? LIMIT ?")
	if e != nil || !strings.Contains(s, "'documents ? literal'") || !strings.Contains(s, "rowid>CAST($1 AS BIGINT)") || !strings.Contains(s, "id=$2") || !strings.Contains(s, "LIMIT CAST($3 AS BIGINT)") {
		t.Fatal(s, e)
	}
	stats, e := p.sql("SELECT source,documents,missing_summaries FROM catalog_stats")
	if e != nil || !strings.HasPrefix(stats, "SELECT source,documents,missing_summaries FROM ") {
		t.Fatal("column name was treated as a table", stats, e)
	}

	if _, e = postgresGenerationSchema("a; DROP SCHEMA public"); e == nil {
		t.Fatal("unsafe generation accepted")
	}
}
func TestPostgresIntegrationMigrationSearchCloneActivationAndRollback(t *testing.T) {
	if os.Getenv("RESEARCH_POSTGRES_INTEGRATION") != "true" {
		t.Skip("requires a scoped verified-TLS PostgreSQL test pool")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	t.Setenv("RESEARCH_POSTGRES_WRITER", "true")
	root := t.TempDir()
	t.Setenv("RESEARCH_INDEX_ROOT", root)
	id, _ := randomDeskID()
	next, _ := randomDeskID()
	path := filepath.Join(root, "runs", id, "catalog.sqlite")
	os.MkdirAll(filepath.Dir(path), 0700)
	source, e := openSQLiteDatabase(path, true)
	if e != nil {
		t.Fatal(e)
	}
	if e = source.initialize(); e != nil {
		t.Fatal(e)
	}
	for _, doc := range []Document{
		{ID: "2610.00001", Source: "arxiv", Title: "Synthetic storage study", Summary: "Adaptive scheduling algorithms reduce storage input output saturation.", Categories: "cs.DC", Published: "2026-10-01", NASRef: "s3://synthetic/paper.pdf"},
		{ID: "2610.00002", Source: "arxiv", Title: "Volcano word appears only in title", Summary: "A galaxy model discusses stellar rotation.", Categories: "astro-ph", Published: "2026-10-02"},
		{ID: "1", Source: "gutenberg", Title: "Synthetic geology catalog", Summary: "Catalog subjects: geology", Language: "en"},
	} {
		n, e := normalize(doc)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = source.put(n); e != nil {
			t.Fatal(e)
		}
	}
	source.close()
	sha, _ := indexRawFileSHA(path)
	g := indexGeneration{ID: id, Catalog: path, SHA: sha}
	t.Setenv("RESEARCH_INDEX_ORIGINAL_CATALOG", path)
	pool, e := postgresPool(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer pool.Close()
	defer func() {
		for _, x := range []string{next, id} {
			schema, _ := postgresGenerationSchema(x)
			pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			pool.Exec(context.Background(), "DELETE FROM research_catalog.generations WHERE id=$1", x)
		}
	}()
	if e = postgresMigrateGeneration(ctx, g, nil); e != nil {
		t.Fatal(e)
	}
	if e = postgresMigrateGeneration(ctx, g, nil); e != nil {
		t.Fatal("resumed immutable import", e)
	}
	writeIndexJSON(filepath.Join(root, "active.json"), indexActive{Current: g})
	d, e := openDatabase(path, false)
	if e != nil {
		t.Fatal(e)
	}
	defer d.close()
	r, e := d.search(SearchRequest{Query: "experimental algorithms to reduce I/O saturation", Source: "arxiv", Category: "cs.DC", From: "2026-01-01"})
	if e != nil || len(r.Documents) != 1 || r.Adjustment == nil || r.Adjustment.MinimumMatch != 4 {
		t.Fatalf("native fallback: %+v %v", r, e)
	}
	r, e = d.search(SearchRequest{Query: "unknown algorithms saturation pulsar quantum", Source: "arxiv", Category: "cs.DC"})
	if e != nil || len(r.Documents) != 1 || r.Adjustment == nil || r.Adjustment.MinimumMatch != 2 {
		t.Fatal("two-term floor", r, e)
	}
	r, e = d.search(SearchRequest{Query: "experimental algorithms to reduce I/O saturation", Source: "arxiv", Category: "astro-ph"})
	if e != nil || len(r.Documents) != 0 {
		t.Fatal("relaxation escaped category", r, e)
	}
	r, e = d.search(SearchRequest{Query: "volcano", Source: "arxiv"})
	if e != nil || len(r.Documents) != 0 {
		t.Fatal("title contaminated abstract search", r, e)
	}
	r, e = d.search(SearchRequest{Query: "geology", Source: "gutenberg"})
	if e != nil || len(r.Documents) != 1 {
		t.Fatal("catalog search", r, e)
	}
	docs, e := d.getDocuments([]string{"arxiv:2610.00001"})
	if e != nil || len(docs) != 1 || docs[0].NASRef != "s3://synthetic/paper.pdf" {
		t.Fatal("saved link", e)
	}
	observed, e := indexFileSHA(path)
	if e != nil || observed != sha {
		t.Fatal("preserved vector identity", observed, e)
	}
	m := &indexManager{root: root, base: g, configSHA: strings.Repeat("a", 64), restart: func() {}}
	dir := m.runPath(next, "")
	os.MkdirAll(dir, 0700)
	run := indexRun{ID: next, Kind: "arxiv-update", Base: g, ConfigSHA: m.configSHA}
	state := indexState{ID: next, Status: "running", Phase: "inventory"}
	writeIndexJSON(m.runPath(next, "request.json"), run)
	if e = m.prepareCandidate(ctx, run, &state, m.runPath(next, "catalog.sqlite"), nil); e != nil {
		t.Fatal("server-side clone", e)
	}
	n, e := openDatabase(m.runPath(next, "catalog.sqlite"), false)
	if e != nil {
		t.Fatal(e)
	}
	defer n.close()
	if e = n.exec("BEGIN IMMEDIATE"); e != nil {
		t.Fatal(e)
	}
	doc, _ := normalize(Document{ID: "2610.00003", Source: "arxiv", Title: "Added synthetic source", Summary: "Ocean geology and rock chemistry.", Categories: "physics.geo-ph"})
	if _, e = n.put(doc); e != nil {
		t.Fatal("native insert", e)
	}
	if _, e = n.run("DELETE FROM documents WHERE id=?", "arxiv:2610.00002"); e != nil {
		t.Fatal(e)
	}
	if e = n.exec("COMMIT"); e != nil {
		t.Fatal(e)
	}
	if e = n.verify(); e != nil {
		t.Fatal("native validation", e)
	}
	counts, e := n.rows("SELECT source,count(*),sum(summary_kind='missing') FROM documents GROUP BY source ORDER BY source")
	if e != nil || counts[0][1] != "2" {
		t.Fatal(counts, e)
	}
	stats, e := n.rows("SELECT source,documents,missing_summaries FROM catalog_stats ORDER BY source")
	if e != nil || stats[0][1] != "2" {
		t.Fatal("independent trigger counts", stats, e)
	}
	nextSHA, e := indexFileSHA(n.path)
	if e != nil || nextSHA == sha {
		t.Fatal(e)
	}
	candidate := indexGeneration{ID: next, Catalog: n.path, SHA: nextSHA, Parent: id, ParentSHA: sha}
	state.Candidate = &candidate
	state.Status = "ready"
	m.saveState(&state)
	if e = m.activate(next, false); e != nil {
		t.Fatal("activate", e)
	}
	var active indexActive
	readIndexJSON(filepath.Join(root, "active.json"), &active)
	if active.Current.ID != next || active.Previous.ID != id {
		t.Fatal("activation slots", active)
	}
	m.base = candidate
	m.reloading.Store(false)
	if e = m.activate("", true); e != nil {
		t.Fatal("rollback", e)
	}
	readIndexJSON(filepath.Join(root, "active.json"), &active)
	if active.Current.ID != id || active.Previous.ID != next {
		t.Fatal("rollback slots", active)
	}
	// Native retention removes only a retired schema and its owned file.
	retired, _ := randomDeskID()
	retiredPath := m.runPath(retired, "catalog.sqlite")
	os.MkdirAll(filepath.Dir(retiredPath), 0700)
	bytes, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	os.WriteFile(retiredPath, bytes, 0600)
	oldGen := indexGeneration{ID: retired, Catalog: retiredPath, SHA: sha}
	defer func() {
		schema, _ := postgresGenerationSchema(retired)
		pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		pool.Exec(context.Background(), "DELETE FROM research_catalog.generations WHERE id=$1", retired)
	}()
	if e = postgresMigrateGeneration(ctx, oldGen, nil); e != nil {
		t.Fatal(e)
	}
	writeIndexJSON(m.runPath(retired, "request.json"), indexRun{ID: retired, ConfigSHA: m.configSHA, Base: g})
	m.saveState(&indexState{ID: retired, Status: "complete", Phase: "validated", Candidate: &oldGen})
	writeIndexJSON(m.runPath(id, "request.json"), indexRun{ID: id, ConfigSHA: m.configSHA, Base: g})
	m.saveState(&indexState{ID: id, Status: "complete", Phase: "validated", Candidate: &g})
	t.Setenv("RESEARCH_INDEX_RETENTION", "true")
	m.base = g
	m.reloading.Store(false)
	if e = m.retentionStep(ctx); e != nil {
		t.Fatal(e)
	}
	if m.retentionPublicStatus().State != "current-plus-one-backup" {
		t.Fatal("PostgreSQL retention needs attention")
	}
	if _, e = os.Stat(retiredPath); !os.IsNotExist(e) {
		t.Fatal("retired catalog file survived", e)
	}
	absent, e := postgresGenerationAbsent(ctx, retired)
	if e != nil || !absent {
		t.Fatal("retired PostgreSQL generation survived", e)
	}
	if _, e = indexFileSHA(path); e != nil {
		t.Fatal("active PostgreSQL catalog changed", e)
	}

	// Exercise the actual incremental worker through its database validation phase.
	updateID, _ := randomDeskID()
	os.MkdirAll(m.runPath(updateID, ""), 0700)
	defer func() {
		schema, _ := postgresGenerationSchema(updateID)
		pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		pool.Exec(context.Background(), "DELETE FROM research_catalog.generations WHERE id=$1", updateID)
	}()
	harvest, _ := syncFixture(t)
	t.Setenv("RESEARCH_ARXIV_SYNC_ROOT", harvest.root)
	var rec arxivRecord
	decodeSyncFixture(oaiFixture("2610.00001", "", "Revised synthetic I/O saturation results."), &rec)
	if e = harvest.storeRecord(rec); e != nil {
		t.Fatal(e)
	}
	decodeSyncFixture(oaiFixture("2610.00004", "", "New synthetic geology findings."), &rec)
	if e = harvest.storeRecord(rec); e != nil {
		t.Fatal(e)
	}
	if e = freezeArxivUpdates(ctx, m.runPath(updateID, "updates.sqlite")); e != nil {
		t.Fatal(e)
	}
	updateRun := indexRun{ID: updateID, Kind: "arxiv-update", Base: g, ConfigSHA: m.configSHA, Meaning: true}
	updateState := indexState{ID: updateID, Status: "queued", Phase: "inventory"}
	writeIndexJSON(m.runPath(updateID, "request.json"), updateRun)
	writeIndexJSON(m.runPath(updateID, "control.json"), map[string]string{"desired": "running"})
	m.config.ModelSHA = strings.Repeat("c", 64)
	e = m.executeArxivUpdate(ctx, updateRun, &updateState)
	if e == nil || updateState.Phase != "meaning" || updateState.Validation["metadata_backend"] != "postgres" || updateState.Validation["added_records"] != "1" || updateState.Validation["changed_records"] != "1" {
		t.Fatalf("native incremental validation did not finish: %s %v %v", updateState.Phase, updateState.Validation, e)
	}
	updateSHA := updateState.Candidate.SHA
	updateState.LastVectorRow = 1
	m.saveState(&updateState)
	e = m.executeArxivUpdate(ctx, updateRun, &updateState)
	if e == nil || updateState.Phase != "meaning" || updateState.LastVectorRow != 1 || updateState.Candidate.SHA != updateSHA || updateState.Read != 2 {
		t.Fatal("meaning resume reset its saved metadata or vector cursor", e)
	}
	raw, _ := os.ReadFile(m.runPath(updateID, "validation-progress.json"))
	var validationProgress indexValidationProgress
	json.Unmarshal(raw, &validationProgress)
	if validationProgress.State != "passed" || validationProgress.WorkUnit != "completed-postgres-query" {
		t.Fatal("PostgreSQL validation progress receipt", string(raw))
	}

	// Resume the real vector loop from a durable row cursor, using only fake model/vector services.
	var upserted atomic.Int32
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			writeJSON(w, 200, semanticEngine())
		case "/tokenize":
			var b struct {
				Input string `json:"input"`
			}
			json.NewDecoder(r.Body).Decode(&b)
			offsets := [][]int{}
			for n := range []rune(b.Input) {
				offsets = append(offsets, []int{n, n + 1})
			}
			writeJSON(w, 200, map[string]any{"input_sha256": textSHA([]byte(b.Input)), "max_input_tokens": 128, "special_tokens": 2, "offsets": offsets})
		case "/embeddings":
			var b struct {
				Input []string `json:"input"`
			}
			json.NewDecoder(r.Body).Decode(&b)
			counts, data := []int{}, []any{}
			for n, text := range b.Input {
				v := make([]float64, semanticDimensions)
				v[0] = 1
				counts = append(counts, len([]rune(text))+2)
				data = append(data, map[string]any{"index": n, "embedding": v})
			}
			writeJSON(w, 200, map[string]any{"model": "bge-m3", "input_contract": map[string]any{"truncated": false, "pooling": "cls", "max_input_tokens": 128, "input_tokens": counts}, "data": data})
		case "/v2/vectordb/collections/has":
			writeJSON(w, 200, map[string]any{"code": 0, "data": map[string]any{"has": true}})
		case "/v2/vectordb/collections/load", "/v2/vectordb/collections/flush":
			writeJSON(w, 200, map[string]any{"code": 0})
		case "/v2/vectordb/entities/upsert":
			var b struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			json.NewDecoder(r.Body).Decode(&b)
			ids := []string{}
			for _, v := range b.Data {
				if v.ID == "arxiv:2610.00001" {
					t.Error("saved row cursor replayed completed source")
				}
				ids = append(ids, v.ID)
			}
			upserted.Add(int32(len(ids)))
			writeJSON(w, 200, map[string]any{"code": 0, "data": map[string]any{"upsertCount": len(ids), "upsertIds": ids}})
		case "/v2/vectordb/entities/query":
			writeJSON(w, 200, map[string]any{"code": 0, "data": []any{map[string]any{"count(*)": 3}}})
		default:
			t.Errorf("unexpected fake-service path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer engine.Close()
	m.config.EmbeddingOrigin = engine.URL
	m.config.VectorOrigin = engine.URL
	m.config.DocumentsPerSecond = 1000
	updateState.Vectors = 1
	resumeRun := updateRun
	resumeRun.Kind = "rebuild"
	if e = m.indexMeaning(ctx, resumeRun, &updateState); e != nil {
		t.Fatal("PostgreSQL vector resume", e)
	}
	if updateState.Vectors != 3 || upserted.Load() != 2 || updateState.Candidate.SHA != updateSHA {
		t.Fatal("vector resume reset cursor or identity")
	}
	if e = postgresSeal(ctx, *updateState.Candidate); e != nil {
		t.Fatal("seal after vectors", e)
	}
	pgDB, e := openDatabase(updateState.Candidate.Catalog, false)
	if e != nil {
		t.Fatal(e)
	}
	defer pgDB.close()
	t.Setenv("RESEARCH_EMBEDDING_ORIGIN", engine.URL)
	t.Setenv("RESEARCH_VECTOR_ORIGIN", engine.URL)
	t.Setenv("RESEARCH_SEMANTIC_STATUS", updateState.Candidate.Semantic)
	t.Setenv("RESEARCH_CATALOG_SHA256", updateSHA)
	t.Setenv("RESEARCH_EMBEDDING_SHA256", m.config.ModelSHA)
	sc, e := newSemantic(&inference{db: pgDB, client: engine.Client()})
	if e != nil {
		t.Fatal("semantic startup with PostgreSQL marker", e)
	}
	coverage, e := sc.coverage()
	if e != nil || coverage.VerifiedCount != 3 {
		t.Fatal("PostgreSQL semantic receipt", coverage, e)
	}

	// Unexpected metadata edits are detected even when the stored fingerprint is unchanged.
	if _, e = d.run("UPDATE documents SET title=? WHERE id=?", "Tampered title", "arxiv:2610.00001"); e == nil {
		t.Fatal("sealed metadata accepted a write")
	}
	if e = d.exec("TRUNCATE TABLE documents"); e == nil {
		t.Fatal("sealed metadata accepted truncation")
	}
	if _, e = indexFileSHA(path); e != nil {
		t.Fatal("immutable source identity changed", e)
	}
}

func TestPostgresIntegrationFullRebuildPreservesUnselectedSources(t *testing.T) {
	if os.Getenv("RESEARCH_POSTGRES_INTEGRATION") != "true" {
		t.Skip("requires a scoped verified-TLS PostgreSQL test pool")
	}
	t.Setenv("RESEARCH_POSTGRES_WRITER", "true")
	rows := `{"id":"2610.00011","title":"Synthetic geology","abstract":"Geology explains rocks."}` + "\n" + `{"id":"2610.00012","title":"Synthetic tectonics","abstract":"Geology studies plates."}` + "\n"
	store := indexFixtureStore(t, map[string][]byte{"papers.jsonl": []byte(rows)})
	m, sourcePath := indexFixtureManager(t, store, "jsonl", "papers.jsonl")
	m.restart = func() {}
	id, _ := randomDeskID()
	path := m.runPath(id, "catalog.sqlite")
	os.MkdirAll(filepath.Dir(path), 0700)
	data, e := os.ReadFile(sourcePath)
	if e != nil {
		t.Fatal(e)
	}
	os.WriteFile(path, data, 0600)
	sha, _ := indexRawFileSHA(path)
	g := indexGeneration{ID: id, Catalog: path, SHA: sha}
	m.base = g
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, e := postgresPool(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer pool.Close()
	ids := []string{id}
	defer func() {
		for _, x := range ids {
			schema, _ := postgresGenerationSchema(x)
			pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			pool.Exec(context.Background(), "DELETE FROM research_catalog.generations WHERE id=$1", x)
		}
	}()
	if e = postgresMigrateGeneration(ctx, g, nil); e != nil {
		t.Fatal(e)
	}
	writeIndexJSON(filepath.Join(m.root, "active.json"), indexActive{Current: g})
	writeIndexJSON(m.runPath(id, "request.json"), indexRun{ID: id, ConfigSHA: m.configSHA, Base: g})
	m.saveState(&indexState{ID: id, Status: "complete", Candidate: &g})
	s, e := m.submit("rebuild", []string{"papers"}, false)
	if e != nil {
		t.Fatal(e)
	}
	ids = append(ids, s.ID)
	var run indexRun
	readIndexJSON(m.runPath(s.ID, "request.json"), &run)
	if e = m.execute(ctx, run, &s); e != nil {
		t.Fatal("full PostgreSQL rebuild", e)
	}
	if s.Status != "ready" || s.Validation["metadata_backend"] != "postgres" {
		t.Fatal("native full rebuild was not validated", s)
	}
	d, e := openDatabase(s.Candidate.Catalog, false)
	if e != nil {
		t.Fatal(e)
	}
	defer d.close()
	counts, e := d.rows("SELECT source,documents FROM catalog_stats WHERE documents>0 ORDER BY source")
	if e != nil || len(counts) != 2 || counts[0][1] != "2" || counts[1][1] != "1" {
		t.Fatal("unselected books were lost", counts, e)
	}
	if e = m.activate(s.ID, false); e != nil {
		t.Fatal("full rebuild activation", e)
	}
}

func TestPostgresTopicThresholdsAreBoundedAndIndexable(t *testing.T) {
	terms := []string{"term0", "term1", "term2", "term3", "term4"}
	for _, minimum := range []int{1, 2, 3, 4, 5} {
		q, e := postgresTopicMatch(terms, minimum)
		if e != nil || !strings.Contains(q, "term0") || strings.Contains(q, "CASE") {
			t.Fatal(minimum, q, e)
		}
	}
	if q, _ := postgresTopicMatch(terms, 4); strings.Count(q, " && ") != 15 {
		t.Fatal("four-of-five threshold", q)
	}
	if q, _ := postgresTopicMatch(terms, 2); !strings.Contains(q, "term0 && (term1 || term2 || term3 || term4)") {
		t.Fatal("factorized pair threshold", q)
	}
	long := []string{}
	for n := 0; n < 24; n++ {
		long = append(long, fmt.Sprintf("term%d", n))
	}
	q, e := postgresTopicMatch(long, 2)
	if e != nil || len(q) > 5000 {
		t.Fatal("unbounded pair expansion", len(q), e)
	}
	if _, e = postgresTopicMatch(long, 12); e == nil {
		t.Fatal("exponential combination expansion accepted")
	}
	if got := fmt.Sprint(postgresFallbackMinimums(24)); got != "[23 2]" {
		t.Fatal(got)
	}
	if got := fmt.Sprint(postgresFallbackMinimums(5)); got != "[4 3 2]" {
		t.Fatal(got)
	}
}
