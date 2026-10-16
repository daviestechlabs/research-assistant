package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed index.html
var page []byte

//go:embed manifest.webmanifest
var manifest []byte

//go:embed icon.svg
var appIcon []byte

//go:embed sw.js
var serviceWorker []byte

//go:embed graph.js
var graphScript []byte

//go:embed desk.js
var deskScript []byte

//go:embed library.js
var libraryScript []byte

//go:embed quality.js
var qualityScript []byte

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func printJSON(v any) error {
	e := json.NewEncoder(os.Stdout)
	e.SetIndent("", "  ")
	return e.Encode(v)
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("use import, search, answer, read, graph, serve, stats, verify, model-identity, check-page-evidence, check-pdf, check-reading, or check-tools; catalog commands require --db")
	}
	command := args[0]
	if command == "arxiv-sync" {
		return runArxivSync(args[1:])
	}
	if command == "migrate-postgres" {
		if len(args) != 1 {
			return errors.New("migrate-postgres accepts only its configured pool and generation receipts")
		}
		return migratePostgresCLI()
	}
	if command == "index-worker" {
		return indexingWorker()
	}
	if command == "check-indexing" {
		return checkIndexing()
	}
	if command == "model-identity" {
		if len(args) != 1 {
			return errors.New("model-identity uses only the configured engine and accepts no flags")
		}
		result, err := configuredServingIdentity(context.Background())
		if err != nil {
			return err
		}
		return printJSON(result)
	}
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	path := f.String("db", "", "explicit metadata database path")
	serverURL := f.String("server", "", "loopback research server for search, answer, graph, or stats")
	file := f.String("file", "", "metadata file (.jsonl, .csv, optionally .gz)")
	source := f.String("source", "", "arxiv or gutenberg")
	format := f.String("format", "jsonl", "jsonl or csv")
	maxRows := f.Int("max", 0, "maximum metadata rows; zero reads the entire stream")
	query := f.String("query", "", "search query or question")
	limit := f.Int("limit", 10, "search results (1 to 50)")
	broad := f.Bool("broad", false, "match any search term")
	rerank := f.Bool("rerank", false, "use configured reranker on up to twenty candidates")
	semanticCheck := f.Bool("semantic", false, "verify semantic search jobs instead of comparisons with check-jobs")
	smart := f.Bool("smart", false, "expand the question, fuse keyword searches, and rerank (two model calls)")
	language := f.String("language", "", "exact language filter")
	category := f.String("category", "", "exact arxiv category filter")
	from := f.String("from", "", "publication start date YYYY-MM-DD")
	to := f.String("to", "", "publication end date YYYY-MM-DD")
	ids := f.String("ids", "", "comma-separated selected source IDs")
	hypotheses := f.Bool("include-hypotheses", false, "include clearly marked speculative graph connections")
	readingQuestion := f.String("question", "", "passage selection question for check-reading")
	renderPage := f.Int("render-page", 0, "physical page to render during check-reading")
	renderOutput := f.String("render-output", "", "explicit PNG output path for check-reading")
	checkDeskPath := f.String("check-desk-db", "", "new absolute verification desk path for check-jobs; never the live desk")
	checkPDF := f.Bool("read-pdfs", false, "use saved PDF pages for check-jobs")
	qualityOutput := f.String("quality-output", "", "absolute NAS receipt path for an operator benchmark")
	generationCase := f.String("generation-case", "", "one fixed development case for benchmark-generation; empty runs all six")
	task := f.String("task", "explain", "explain, compare, or hypothesize")
	listen := f.String("listen", "127.0.0.1:8094", "HTTP address; remote binding requires an explicit HTTPS public configuration")
	maxMiB := f.Int("max-db-mib", 16384, "database page ceiling in MiB (journal needs additional space)")
	batchSize := f.Int("batch-size", 500, "durable import checkpoint interval (1 to 50000 records)")
	cacheMiB := f.Int("cache-mib", 32, "SQLite page cache ceiling in MiB (1 to 512)")
	memoryMiB := f.Int("memory-mib", 0, "SQLite RAM heap ceiling for import or serving (0 disables)")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if command == "check-page-evidence" {
		var input io.Reader = os.Stdin
		if *file != "-" {
			if !filepath.IsAbs(*file) {
				return errors.New("page evidence check requires --file - or an absolute input path")
			}
			f, err := os.Open(*file)
			if err != nil {
				return errors.New("cached page input is unavailable")
			}
			defer f.Close()
			input = f
		}
		result, err := inspectPageEvidence(input, *readingQuestion)
		if err != nil {
			return err
		}
		return printJSON(result)
	}
	valid := map[string]bool{"import": true, "search": true, "answer": true, "read": true, "graph": true, "serve": true, "stats": true, "verify": true, "check-pdf": true, "check-reading": true, "check-tools": true, "check-jobs": true, "benchmark-quality": true, "benchmark-quality-holdout": true, "benchmark-generation": true, "benchmark-retrieval": true}
	if !valid[command] {
		return errors.New("unknown command")
	}
	if command == "check-tools" {
		identity, err := pdfExtractionIdentity()
		if err != nil {
			return err
		}
		for _, name := range []string{"pdfinfo", "pdftotext", "pdftoppm"} {
			if _, err = pdfTool(context.Background(), name, nil, 65536, "-v"); err != nil {
				return err
			}
		}
		return printJSON(map[string]any{"tools": []string{"pdfinfo", "pdftotext", "pdftoppm"}, "extraction_identity": identity, "runtime_verified": true})
	}
	if command == "check-pdf" || command == "check-reading" {
		id, err := canonicalSavedID(*query)
		if err != nil {
			return err
		}
		archive, err := newPDFArchive()
		if err != nil {
			return err
		}
		if archive == nil {
			return errors.New("saved PDF reader is not configured")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancel()
		start := time.Now()
		pdf, err := archive.readSourcePDF(ctx, id)
		if err != nil {
			return err
		}
		result := map[string]any{"id": id, "version": pdf.Version, "bytes": len(pdf.Data), "sha256": pdf.SHA256, "checksum_verified": true, "source": "saved NAS PDF", "model_calls": 0}
		if command == "check-reading" {
			paper, err := extractPaperText(ctx, id, pdf)
			if err != nil {
				return err
			}
			pages := relevantPDFPages(paper, *readingQuestion, 4)
			for n := range pages {
				pages[n].Text = readingPageExcerpt(paper, pages[n], *readingQuestion, 2200)
			}
			result["physical_pages"], result["unreadable_pages"], result["extraction"], result["consulted_excerpts"] = len(paper.Pages), paper.UnreadablePages, paper.Extraction, pages
			if *renderPage != 0 || *renderOutput != "" {
				if *renderPage < 1 || *renderPage > len(paper.Pages) || !filepath.IsAbs(*renderOutput) || !strings.HasSuffix(*renderOutput, ".png") {
					return errors.New("rendering requires an existing physical page and an absolute PNG output path")
				}
				data, err := pdfTool(ctx, "pdftoppm", pdf.Data, 8<<20, "-f", strconv.Itoa(*renderPage), "-l", strconv.Itoa(*renderPage), "-singlefile", "-scale-to", "1400", "-png", "-")
				if err != nil {
					return err
				}
				if !bytes.HasPrefix(data, []byte{137, 80, 78, 71, 13, 10, 26, 10}) {
					return errors.New("PDF renderer did not return a PNG")
				}
				if err = os.WriteFile(*renderOutput, data, 0600); err != nil {
					return err
				}
				result["rendered_physical_page"] = *renderPage
			}
		}
		result["elapsed_ms"] = time.Since(start).Milliseconds()
		return printJSON(result)
	}
	if *serverURL != "" {
		if *path != "" || *memoryMiB != 0 {
			return errors.New("choose either --server or --db")
		}
		var body any
		switch command {
		case "search":
			body = map[string]any{"query": *query, "source": *source, "language": *language, "category": *category, "from": *from, "to": *to, "limit": *limit, "broad": *broad, "rerank": *rerank, "smart": *smart}
		case "answer":
			body = AnswerRequest{Question: *query, IDs: strings.Split(*ids, ","), Task: *task}
		case "graph":
			body = GraphRequest{Query: *query, IDs: strings.Split(*ids, ","), IncludeHypotheses: *hypotheses}
		case "stats":
		default:
			return errors.New("--server supports search, answer, graph, or stats")
		}
		result, err := researchServerRequest(*serverURL, command, body)
		if err != nil {
			return err
		}
		return printJSON(result)
	}
	if *maxRows < 0 || *maxMiB < 1 || *maxMiB > 65536 || *batchSize < 1 || *batchSize > 50000 || *cacheMiB < 1 || *cacheMiB > 512 {
		return errors.New("invalid row or storage limit")
	}
	if *memoryMiB < 0 || *memoryMiB > 16384 || (*memoryMiB > 0 && ((command != "import" && command != "serve") || *path == "" || *path == ":memory:")) {
		return errors.New("memory mode requires import or serve with a database path and a heap limit of at most 16384 MiB")
	}
	databasePath := *path
	if command == "serve" {
		os.Setenv("RESEARCH_INDEX_ORIGINAL_CATALOG", *path)
		var err error
		databasePath, err = resolveIndexCatalog(*path)
		if err != nil {
			return err
		}
	}
	if *memoryMiB > 0 {
		if err := enableSQLiteMemoryAccounting(); err != nil {
			return err
		}
		if command == "import" {
			databasePath = ":memory:"
		}
	}
	d, err := openDatabase(databasePath, command == "import")
	if err != nil {
		return err
	}
	defer d.close()
	if *memoryMiB > 0 {
		if err = d.sqliteHeapLimit(int64(*memoryMiB) * 1024 * 1024); err != nil {
			return err
		}
		if command == "import" {
			// Reserve exclusively before importing. Never overwrite a previous index.
			reserved, err := os.OpenFile(*path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			if err = reserved.Close(); err != nil {
				return err
			}
			fmt.Fprintln(os.Stderr, `{"checkpoint_storage":"volatile-memory","snapshot_path_reserved":true}`)
		}
	}
	if err = d.exec(fmt.Sprintf("PRAGMA cache_size=-%d", *cacheMiB*1024)); err != nil {
		return err
	}
	if command == "import" {
		if err = d.initialize(); err != nil {
			return err
		}
		r, err := d.rows("PRAGMA page_size")
		if err != nil {
			return err
		}
		size, _ := strconv.Atoi(r[0][0])
		capPages := int64(*maxMiB) * 1024 * 1024 / int64(size)
		r, err = d.rows("PRAGMA page_count")
		if err != nil {
			return err
		}
		count, _ := strconv.ParseInt(r[0][0], 10, 64)
		if count > capPages {
			return errors.New("existing database exceeds configured page ceiling")
		}
		if err = d.exec(fmt.Sprintf("PRAGMA max_page_count=%d", capPages)); err != nil {
			return err
		}
		in, err := openMetadata(*file)
		if err != nil {
			return err
		}
		defer in.Close()
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		report, err := importCatalogBatched(ctx, d, *source, *format, in, *maxRows, *batchSize, os.Stderr)
		if *memoryMiB > 0 {
			// Save committed batches even after cancellation or a malformed row.
			// Use a separate bounded context so cancellation can preserve progress.
			saveCtx, cancelSave := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancelSave()
			fmt.Fprintln(os.Stderr, `{"memory_integrity_check_started":true}`)
			if verifyErr := d.verify(); verifyErr != nil {
				return verifyErr
			}
			fmt.Fprintln(os.Stderr, `{"memory_database_integrity":"ok","memory_fts_content_integrity":"ok"}`)
			saveStart := time.Now()
			fmt.Fprintln(os.Stderr, `{"snapshot_saving":true}`)
			if saveErr := d.snapshot(saveCtx, *path); saveErr != nil {
				return fmt.Errorf("memory snapshot did not complete: %w", saveErr)
			}
			json.NewEncoder(os.Stderr).Encode(map[string]any{"snapshot_saved": true, "snapshot_elapsed_ms": time.Since(saveStart).Milliseconds()})
		}
		if e := printJSON(report); e != nil {
			return e
		}
		return err
	}
	version, err := d.rows("PRAGMA user_version")
	if err != nil {
		return err
	}
	if version[0][0] == "1" {
		if err = d.initialize(); err != nil {
			return err
		}
	}
	if version[0][0] != "1" && version[0][0] != "2" {
		return errors.New("database is not an initialized research catalog")
	}
	var i *inference
	if command == "benchmark-retrieval" {
		i, err = newEmbeddingInference(d)
	} else {
		i, err = newInference(d)
	}
	if err != nil {
		return err
	}
	defer i.close()
	switch command {
	case "benchmark-generation":
		ctx, cancel := context.WithTimeout(context.Background(), 1800*time.Second)
		defer cancel()
		result, err := i.benchmarkGeneration(ctx, *qualityOutput, *generationCase)
		if err != nil {
			return err
		}
		return printJSON(result)
	case "benchmark-retrieval":
		output := *qualityOutput
		if output == "" {
			output = retrievalPath(i)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 600*time.Second)
		defer cancel()
		result, err := i.benchmarkRetrieval(ctx, output, *checkDeskPath)
		if err != nil {
			return err
		}
		return printJSON(result)
	case "benchmark-quality", "benchmark-quality-holdout":
		ctx, cancel := context.WithTimeout(context.Background(), 420*time.Second)
		defer cancel()
		var result qualityReport
		var err error
		if command == "benchmark-quality-holdout" {
			result, err = i.benchmarkQualityHoldout(ctx, *qualityOutput)
		} else {
			result, err = i.benchmarkQuality(ctx, *qualityOutput)
		}
		if err != nil {
			return err
		}
		return printJSON(result)
	case "check-jobs":
		var archive *pdfArchive
		var err error
		if !*semanticCheck {
			archive, err = newPDFArchive()
			if err != nil {
				return err
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 540*time.Second)
		defer cancel()
		data := SessionData{Question: *query, SelectedIDs: strings.Split(*ids, ","), Task: *task, PDFMode: *checkPDF}
		if *semanticCheck {
			data = SessionData{Search: SearchRequest{Query: *query, Source: "arxiv", Limit: *limit, Meaning: true}, Task: "explain"}
		}
		result, err := checkResearchJobs(ctx, *checkDeskPath, i, archive, data)
		if err != nil {
			return err
		}
		return printJSON(result)
	case "search":
		q := SearchRequest{Query: *query, Source: *source, Language: *language, Category: *category, From: *from, To: *to, Limit: *limit, Broad: *broad}
		var r SearchResponse
		if *smart {
			r, err = i.smartSearch(context.Background(), q)
		} else if *rerank {
			r, err = i.rerank(context.Background(), q)
		} else {
			r, err = d.search(q)
		}
		if err != nil {
			return err
		}
		return printJSON(r)
	case "answer":
		r, err := i.answer(context.Background(), AnswerRequest{Question: *query, IDs: strings.Split(*ids, ","), Task: *task})
		if err != nil {
			return err
		}
		return printJSON(r)
	case "read":
		archive, err := newPDFArchive()
		if err != nil {
			return err
		}
		textDB, err := openDatabase(deskPath(i), true)
		if err != nil {
			return err
		}
		defer textDB.close()
		// Operator reading shares the text cache but never starts another job
		// worker or changes the deployed worker's restart state.
		desk := &researchDesk{db: textDB, i: i, archive: archive}
		ctx, cancel := context.WithTimeout(context.Background(), 420*time.Second)
		defer cancel()
		data := SessionData{Question: *query, SelectedIDs: strings.Split(*ids, ","), Task: *task, PDFMode: true}
		prepared, err := desk.prepareReading(ctx, data, func(string) {})
		if err != nil {
			return err
		}
		result, err := i.readingAnswer(ctx, data, prepared)
		if err != nil {
			return err
		}
		result.Sources = browserDocuments(result.Sources, true)
		return printJSON(result)
	case "graph":
		r, err := i.graph(context.Background(), GraphRequest{Query: *query, IDs: strings.Split(*ids, ","), IncludeHypotheses: *hypotheses})
		if err != nil {
			return err
		}
		return printJSON(r)
	case "stats":
		r, err := i.stats()
		if err != nil {
			return err
		}
		return printJSON(r)
	case "verify":
		if err := d.verify(); err != nil {
			return err
		}
		return printJSON(map[string]any{"database_integrity": "ok", "fts_content_integrity": "ok"})
	case "serve":
		if *memoryMiB > 0 && d.pg == nil {
			cache, err := openDatabase(":memory:", true)
			if err != nil {
				return err
			}
			defer cache.close()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
			defer cancel()
			fmt.Fprintln(os.Stderr, `{"metadata_cache_loading":true}`)
			if err = cache.load(ctx, d); err != nil {
				return err
			}
			i.db = cache
			cache.books = d.books
			fmt.Fprintln(os.Stderr, `{"metadata_cache_loaded":true,"model_budget_storage":"nas-database"}`)
			return serve(cache, i, *listen)
		}
		return serve(d, i, *listen)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, err error) {
	writeJSON(w, 400, map[string]string{"error": err.Error()})
}

func decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return errors.New("invalid request body")
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		return errors.New("request must contain one JSON object")
	}
	return nil
}

func handler(d *database, i *inference, authority string) http.Handler {
	return securedHandler(d, i, "http://"+authority, nil)
}

func securedHandler(d *database, i *inference, originURL string, auth *tokenVerifier) http.Handler {
	return securedPDFHandler(d, i, originURL, auth, nil)
}

func securedPDFHandler(d *database, i *inference, originURL string, auth *tokenVerifier, archive *pdfArchive, desks ...*researchDesk) http.Handler {
	authority := strings.TrimPrefix(strings.TrimPrefix(originURL, "https://"), "http://")
	mux := http.NewServeMux()
	var desk *researchDesk
	if len(desks) > 0 {
		desk = desks[0]
	}
	if desk != nil {
		desk.routes(mux)
		if desk.indexing != nil {
			desk.indexing.routes(mux)
		}
	}
	if archive != nil {
		mux.HandleFunc("GET /api/pdf", archive.servePDF)
		mux.HandleFunc("GET /api/book", archive.serveBook)
	}
	mux.HandleFunc("GET /api/books", func(w http.ResponseWriter, r *http.Request) {
		v, err := d.search(SearchRequest{Query: "*", Source: "dnd", Limit: 50})
		if err != nil {
			fail(w, err)
			return
		}
		v.Documents = browserDocuments(v.Documents, archive != nil)
		writeJSON(w, 200, v)
	})
	mux.HandleFunc("GET /api/quality", qualityHandler(i))
	mux.HandleFunc("GET /manifest.webmanifest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/manifest+json")
		w.Write(brandedManifest())
	})
	mux.HandleFunc("GET /icon.svg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Write(brandedIcon())
	})
	mux.HandleFunc("GET /sw.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Write(serviceWorker)
	})
	mux.HandleFunc("GET /graph.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Write(graphScript)
	})
	mux.HandleFunc("GET /desk.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Write(deskScript)
	})
	mux.HandleFunc("GET /library.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Write(libraryScript)
	})
	mux.HandleFunc("GET /quality.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Write(qualityScript)
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(brandedPage())
	})
	mux.HandleFunc("GET /api/stats", func(w http.ResponseWriter, r *http.Request) {
		v, err := i.stats()
		if err != nil {
			fail(w, err)
			return
		}
		var coverage *SemanticCoverage
		semanticError := ""
		if i.semantic != nil {
			status, err := i.semantic.coverage()
			if err == nil {
				coverage = &status
			} else {
				semanticError = err.Error()
			}
		}
		writeJSON(w, 200, map[string]any{"catalog": v, "private_books": len(d.books), "sessions_enabled": desk != nil, "indexing_enabled": desk != nil && desk.indexing != nil && desk.indexing.authorized(r), "semantic_search_enabled": desk != nil && coverage != nil, "semantic_coverage": coverage, "semantic_error": semanticError, "pdf_reading_enabled": desk != nil && archive != nil && i.answerURL != "", "graph_enabled": i.answerURL != "", "local_pdf_enabled": archive != nil, "search_basis": "abstract", "authentication_required": auth != nil, "answer_enabled": i.answerURL != "", "rerank_enabled": i.rerankURL != "", "smart_enabled": i.answerURL != "" && i.rerankURL != "", "limit_owner": i.limitOwner, "daily_calls": i.dailyCalls, "daily_units": i.dailyUnits})
	})
	mux.HandleFunc("POST /api/search", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			SearchRequest
			Rerank bool `json:"rerank"`
			Smart  bool `json:"smart"`
		}
		if err := decode(w, r, &req); err != nil {
			fail(w, err)
			return
		}
		if req.Meaning {
			fail(w, errors.New("meaning search requires a saved semantic-search job"))
			return
		}
		var v SearchResponse
		var err error
		if req.Smart {
			v, err = i.smartSearch(r.Context(), req.SearchRequest)
		} else if req.Rerank {
			v, err = i.rerank(r.Context(), req.SearchRequest)
		} else {
			v, err = d.search(req.SearchRequest)
		}
		if err != nil {
			fail(w, err)
			return
		}
		v.Documents = browserDocuments(v.Documents, archive != nil)
		writeJSON(w, 200, v)
	})
	mux.HandleFunc("POST /api/answer", func(w http.ResponseWriter, r *http.Request) {
		var req AnswerRequest
		if err := decode(w, r, &req); err != nil {
			fail(w, err)
			return
		}
		v, err := i.answer(r.Context(), req)
		if err != nil {
			fail(w, err)
			return
		}
		v.Sources = browserDocuments(v.Sources, archive != nil)
		writeJSON(w, 200, v)
	})
	mux.HandleFunc("POST /api/graph", func(w http.ResponseWriter, r *http.Request) {
		var req GraphRequest
		if err := decode(w, r, &req); err != nil {
			fail(w, err)
			return
		}
		v, err := i.graph(r.Context(), req)
		if err != nil {
			fail(w, err)
			return
		}
		v.Sources = browserDocuments(v.Sources, archive != nil)
		writeJSON(w, 200, v)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == "GET" && r.URL.Path == "/healthz" {
			w.Write([]byte("ready\n"))
			return
		}
		if r.Host != authority {
			http.Error(w, "invalid host", 403)
			return
		}
		if origin := r.Header.Get("Origin"); (r.Method == "POST" && origin != originURL) || (origin != "" && origin != originURL) {
			http.Error(w, "invalid origin", 403)
			return
		}
		owner := "loopback-operator"
		if auth != nil {
			var ok bool
			owner, ok = auth.subject(r.Context(), gatewayToken(r))
			if !ok {
				http.Error(w, "authentication required", http.StatusUnauthorized)
				return
			}
		}
		r = r.WithContext(context.WithValue(r.Context(), ownerKey{}, owner))
		if desk != nil && desk.indexing != nil && r.Method == "POST" {
			m := desk.indexing
			switchRequest := strings.HasPrefix(r.URL.Path, "/api/library/indexing/") && (strings.HasSuffix(r.URL.Path, "/activate") || strings.HasSuffix(r.URL.Path, "/rollback"))
			if switchRequest {
				m.admission.Lock()
				defer m.admission.Unlock()
			} else {
				m.admission.RLock()
				defer m.admission.RUnlock()
			}
			if m.reloading.Load() {
				http.Error(w, "catalog reload is in progress", 503)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func gatewayToken(r *http.Request) string {
	if token := r.Header.Get("x-envoy-oidc-id-token"); token != "" {
		return token
	}
	// Authentik's access JWT carries the same signed identity claims.
	// It still passes every signature, issuer, audience, subject, and time check.
	if value := r.Header.Get("Authorization"); strings.HasPrefix(value, "Bearer ") {
		return strings.TrimPrefix(value, "Bearer ")
	}
	return ""
}

func serve(d *database, i *inference, address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	publicOrigin, verifier, err := publicConfiguration()
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || (!ip.IsLoopback() && publicOrigin == "") {
		return errors.New("local preview must bind a numeric loopback address")
	}
	if verifier != nil {
		verifier.publicKey(context.Background(), "")
		if len(verifier.keys) == 0 {
			return errors.New("cannot load public OIDC signing keys")
		}
	}
	archive, err := newPDFArchive()
	if err != nil {
		return err
	}
	if archive != nil && publicOrigin != "" && verifier == nil {
		return errors.New("saved PDF access requires sign-in on public installations")
	}
	var desk *researchDesk
	if i.budget.path != ":memory:" && (publicOrigin == "" || verifier != nil) {
		desk, err = openResearchDesk(deskPath(i), i, archive)
		if err != nil {
			return errors.New("research session storage is unavailable")
		}
		defer desk.close()
		desk.indexing, err = newIndexManager(os.Getenv("RESEARCH_INDEX_ORIGINAL_CATALOG"), desk)
		if err != nil {
			return err
		}
	}
	l, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	defer l.Close()
	actual := l.Addr().String()
	origin := "http://" + actual
	if publicOrigin != "" {
		origin = publicOrigin
	}
	fmt.Fprintln(os.Stderr, deskName()+": "+origin)
	s := &http.Server{Handler: securedPDFHandler(d, i, origin, verifier, archive, desk), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 190 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16384}
	shutdownDone := make(chan struct{})
	var reload sync.Once
	if desk != nil && desk.indexing != nil {
		desk.indexing.restart = func() {
			reload.Do(func() {
				go func() {
					time.Sleep(300 * time.Millisecond)
					ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
					defer cancel()
					if s.Shutdown(ctx) != nil {
						s.Close()
					}
					close(shutdownDone)
				}()
			})
		}
	}
	if desk != nil && desk.indexing != nil {
		policy, policyErr := arxivAutoConfig()
		if policyErr != nil {
			return policyErr
		}
		if policy.Enabled {
			ctx, cancel := context.WithCancel(context.Background())
			done := desk.indexing.startAutoMerge(ctx, policy)
			defer func() { cancel(); <-done }()
		}
		if retentionPolicy().Enabled {
			ctx, cancel := context.WithCancel(context.Background())
			done := desk.indexing.startRetention(ctx)
			defer func() { cancel(); <-done }()
		}
	}
	err = s.Serve(l)
	if errors.Is(err, http.ErrServerClosed) {
		<-shutdownDone
		return nil
	}
	return err
}

// Browser clients receive public metadata, never internal storage locations.
// Copy the records so operator commands retain their NAS references.
func browserDocuments(documents []Document, localPDF ...bool) []Document {
	result := make([]Document, len(documents))
	copy(result, documents)
	for n := range result {
		result[n].NASRef = ""
		if len(localPDF) > 0 && localPDF[0] && result[n].Source == "dnd" {
			result[n].BookURL = "/api/book?id=" + result[n].ID
			if result[n].MediaType == "application/pdf" {
				result[n].PDFURL = bookPDFLink(result[n].ID)
			}
		}
		if len(localPDF) > 0 && localPDF[0] && result[n].Source == "arxiv" {
			result[n].PDFURL = "/api/pdf?id=" + result[n].ID
		}
	}
	return result
}
