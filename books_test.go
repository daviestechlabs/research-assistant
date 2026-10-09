package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
)

func approvedBookFixture(t *testing.T) {
	t.Helper()
	entries := []libraryBook{}
	for _, id := range []string{"players-handbook", "players-handbook-corrected"} {
		entries = append(entries, libraryBook{ID: "dnd:" + id, Title: "Player's Handbook", Edition: "5e-2014", Ruleset: "dnd-5e-2014", Category: "rules", MediaType: "application/pdf", Bytes: 100, SHA256: strings.Repeat("a", 64), Key: "books/" + id + ".pdf"})
	}
	data, _ := json.Marshal(map[string]any{"schema": "research-private-book-library-v1", "mirror_plan_file_sha256": strings.Repeat("b", 64), "local_retrieval": true, "distribution": false, "entries": entries})
	file := filepath.Join(t.TempDir(), "books.json")
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RESEARCH_BOOK_MANIFEST", file)
	t.Setenv("RESEARCH_BOOK_PLAN_SHA256", strings.Repeat("b", 64))
	t.Setenv("RESEARCH_BOOK_COUNT", "2")
}

func TestPrivateBookCatalogKeepsCorpusSnapshotUnchanged(t *testing.T) {
	approvedBookFixture(t)
	d := testDB(t)
	loadFixture(t, d)
	before, _ := os.ReadFile(d.path)
	i, err := newInference(d)
	if err != nil {
		t.Fatal(err)
	}
	defer i.close()
	all, err := d.search(SearchRequest{Source: "dnd", Query: "*", Limit: 50})
	if err != nil || len(all.Documents) != 2 || all.ModelCalls != 0 {
		t.Fatal("book shelf", err, len(all.Documents))
	}
	result, err := d.search(SearchRequest{Source: "dnd", Query: "Player's Handbook", Limit: 10})
	if err != nil || len(result.Documents) != 2 {
		t.Fatal("original and corrected editions", err)
	}
	for _, doc := range result.Documents {
		if doc.Edition == "" || doc.Ruleset != "dnd-5e-2014" || doc.SummaryKind != "book-catalog" || doc.Fingerprint == "" {
			t.Fatal("missing book identity")
		}
	}
	docs, err := d.getDocuments([]string{"arxiv:1706.03762", "dnd:players-handbook"})
	if err != nil || len(docs) != 2 {
		t.Fatal("mixed selection", err)
	}
	after, _ := os.ReadFile(d.path)
	if textSHA(before) != textSHA(after) {
		t.Fatal("books changed the semantic catalog snapshot")
	}
	wire, _ := json.Marshal(browserDocuments(all.Documents, true))
	if strings.Contains(string(wire), "books/") || strings.Contains(string(wire), "s3://") || strings.Contains(string(wire), "sha256\":") {
		t.Fatal("storage manifest entered browser metadata")
	}
	if _, err = d.getDocuments([]string{"dnd:unknown-book"}); err == nil {
		t.Fatal("unknown book admitted")
	}
}

func bookArchiveFixture(t *testing.T, corrupt bool) (*pdfArchive, *atomic.Int32) {
	t.Helper()
	body := []byte("%PDF-1.7\nPrivate synthetic book fixture.\n%%EOF\n")
	b := libraryBook{ID: "dnd:players-handbook", Title: "Player's Handbook", Edition: "5e-2014", Ruleset: "dnd-5e-2014", MediaType: "application/pdf", Key: "books/Private_fixture.pdf", Bytes: int64(len(body)), SHA256: textSHA(body)}
	if corrupt {
		b.SHA256 = strings.Repeat("0", 64)
	}
	calls := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/training-data/"+b.Key || r.URL.RawQuery != "" {
			t.Error("unapproved object path")
		}
		if r.Method == "GET" && r.Header.Get("If-Match") != `"fixed"` {
			t.Error("GET not pinned to HEAD")
		}
		w.Header().Set("ETag", `"fixed"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if r.Method != "HEAD" {
			w.Write(body)
		}
	}))
	t.Cleanup(server.Close)
	return &pdfArchive{books: map[string]libraryBook{b.ID: b}, endpoint: server.URL, credentials: aws.Credentials{AccessKeyID: "fixture", SecretAccessKey: "fixture"}, client: server.Client(), busy: make(chan struct{}, 1)}, calls
}

func TestPrivateBookReaderChecksContentAndPhysicalPageLinks(t *testing.T) {
	a, calls := bookArchiveFixture(t, false)
	p, err := a.readSourcePDF(context.Background(), "dnd:players-handbook")
	if err != nil {
		t.Fatal(err)
	}
	if textSHA(p.Data) != p.SHA256 || p.Version != 1 {
		t.Fatal("unverified book")
	}
	_, err = a.readSourcePDF(context.Background(), "dnd:players-handbook")
	if err != nil || calls.Load() != 2 {
		t.Fatal("verified RAM cache not reused")
	}
	w := httptest.NewRecorder()
	a.servePDF(w, httptest.NewRequest("GET", strings.Split(pdfPageLink("dnd:players-handbook", p.SHA256, 2), "#")[0], nil))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	a.servePDF(w, httptest.NewRequest("GET", "/api/pdf?id=dnd:players-handbook&sha256="+strings.Repeat("f", 64), nil))
	if w.Code != 409 {
		t.Fatal("wrong PDF version accepted")
	}
	w = httptest.NewRecorder()
	a.serveBook(w, httptest.NewRequest("GET", "/api/book?id=dnd:unknown-book", nil))
	if w.Code != 404 {
		t.Fatal("unknown book served")
	}
	bad, _ := bookArchiveFixture(t, true)
	if _, err = bad.readSourcePDF(context.Background(), "dnd:players-handbook"); err == nil {
		t.Fatal("wrong content hash accepted")
	}
	if _, err = physicalPageCountWithin("Pages: 1000", readingPageLimit("dnd:players-handbook")); err != nil {
		t.Fatal(err)
	}
	if _, err = physicalPageCount("Pages: 1000"); err == nil {
		t.Fatal("paper page bound changed")
	}
}

func TestPrivateBooksCannotAnswerFromCatalogMetadata(t *testing.T) {
	approvedBookFixture(t)
	s, i := deskFixture(t)
	i.answerURL = "http://127.0.0.1:1"
	i.model = "fixture"
	if _, err := i.answer(context.Background(), AnswerRequest{Question: "Explain concentration", IDs: []string{"dnd:players-handbook"}, Task: "explain"}); err == nil {
		t.Fatal("catalog used as rule evidence")
	}
	if _, err := i.graph(context.Background(), GraphRequest{Query: "concentration", IDs: []string{"dnd:players-handbook"}}); err == nil {
		t.Fatal("catalog used as graph evidence")
	}
	a, _ := bookArchiveFixture(t, false)
	s.archive = a
	installFixturePDFTools(t, "This introduction explains what this synthetic book covers.\fConcentration requires maintaining focus on the spell during its duration.\f\f")
	p, err := s.prepareReading(context.Background(), SessionData{SelectedIDs: []string{"dnd:players-handbook"}, Question: "concentration", Task: "explain", PDFMode: true}, func(string) {})
	if err != nil || p.Sources[0].Edition != "5e-2014" || p.Sources[0].Ruleset != "dnd-5e-2014" || len(p.Papers[0].Pages) != 3 {
		t.Fatal("book page evidence or edition", err)
	}
	stats, _ := i.stats()
	if stats.(map[string]any)["model_calls_today"] != 0 {
		t.Fatal("book checks charged a model call")
	}
}

func TestLongReadingBudgetRemainsBoundedAndDurable(t *testing.T) {
	d := testDB(t)
	loadFixture(t, d)
	t.Setenv("RESEARCH_DAILY_UNITS", "300000")
	i, err := newInference(d)
	if err != nil {
		t.Fatal(err)
	}
	if i.dailyCalls != 20 || i.dailyUnits != 300000 {
		t.Fatal("budget policy")
	}
	if err = i.reserve("pdf-reading", 250001); err != nil {
		t.Fatal(err)
	}
	i.close()
	i, err = newInference(d)
	if err != nil {
		t.Fatal(err)
	}
	defer i.close()
	if err = i.reserve("pdf-reading", 50000); err == nil {
		t.Fatal("reopened budget forgot prior usage")
	}
	for _, value := range []string{"0", "199999", "500001", "unlimited"} {
		t.Setenv("RESEARCH_DAILY_UNITS", value)
		if other, err := newInference(d); err == nil {
			other.close()
			t.Fatal("unbounded or invalid resource policy accepted")
		}
	}
}
