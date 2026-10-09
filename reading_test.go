package main

import (
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

func TestReadingPhysicalPagesKeepBlankPages(t *testing.T) {
	text := "The first physical page has readable text.\f\fThe third physical page has readable text.\f"
	pages, unreadable, err := splitPhysicalPages(text, 3)
	if err != nil || len(pages) != 3 || pages[2].Page != 3 || pages[1].Text != "" || len(unreadable) != 1 || unreadable[0] != 2 {
		t.Fatalf("page identity changed: %+v %v %v", pages, unreadable, err)
	}
	if _, _, err = splitPhysicalPages(text, 2); err == nil {
		t.Fatal("boundary disagreement accepted")
	}
	if _, _, err = splitPhysicalPages(string([]byte{255}), 1); err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
	for _, info := range []string{"Pages: 0", "Pages: 401", "Pages: bad", "PDF metadata only"} {
		if _, err := physicalPageCount(info); err == nil {
			t.Fatalf("bad page count accepted: %s", info)
		}
	}
	if count, err := physicalPageCount("Title: Example\nPages:          3\nEncrypted: no\n"); err != nil || count != 3 {
		t.Fatalf("page count: %d %v", count, err)
	}
}

func readingFixture() preparedReading {
	quote := "These observations link erosion to changes in river channels."
	other := "This introduction describes the scope of the selected investigation."
	return preparedReading{
		Documents: []Document{{ID: "arxiv:0704.0001", Source: "arxiv", Title: "River channels"}},
		Papers:    []PaperText{{ID: "arxiv:0704.0001", Version: 3, SHA256: strings.Repeat("a", 64), Extraction: "fixture", Pages: []PDFPageText{{1, other}, {2, quote}, {3, ""}}, UnreadablePages: []int{3}}},
		Sources:   []readingSourceInput{{ID: "arxiv:0704.0001", PhysicalPages: 3, Version: 3, Pages: []readingPageInput{{1, other, []string{other}}, {2, quote, []string{quote}}}}},
	}
}

func readingContent(page int, quote string) string {
	q := pageQuote{"arxiv:0704.0001", page, quote}
	data, _ := json.Marshal(map[string]any{"reported": []pageQuote{q}, "interpretation": map[string]any{"text": "The quoted observation suggests a connection to examine; it does not establish causation.", "citations": []pageQuote{q}}})
	return string(data)
}

func TestReadingRejectsWrongPagesAndFabricatedExcerpts(t *testing.T) {
	p := readingFixture()
	quote := p.Sources[0].Pages[1].Choices[0]
	result, err := validateReadingAnswer(readingContent(2, quote), "explain", p)
	if err != nil || len(result.Statements) != 2 || result.Statements[0].Citations[0].SHA256 != p.Papers[0].SHA256 || result.Papers[0].ConsultedPages[1] != 2 {
		t.Fatalf("page reading: %+v %v", result, err)
	}
	for _, wire := range []string{readingContent(1, quote), readingContent(3, quote), readingContent(2, "The study establishes a new causal mechanism for every river."), strings.ReplaceAll(readingContent(2, quote), "0704.0001", "0704.9999")} {
		if _, err := validateReadingAnswer(wire, "explain", p); err == nil {
			t.Fatalf("invalid page evidence accepted: %s", wire)
		}
	}
	base := readingCacheKey("request", p)
	p.Papers[0].SHA256 = strings.Repeat("b", 64)
	if readingCacheKey("request", p) == base {
		t.Fatal("PDF checksum did not invalidate the result cache")
	}
	base = readingCacheKey("request", p)
	p.Papers[0].Extraction = "new-parser"
	if readingCacheKey("request", p) == base {
		t.Fatal("parser identity did not invalidate the result cache")
	}
	long := strings.Repeat("Unrelated background words appear here. ", 150) + "These observations link erosion to changes in river channels." + strings.Repeat(" More background words appear here.", 100)
	excerpt := relevantPageExcerpt(long, "erosion river channels", 400)
	if !strings.Contains(excerpt, "erosion") || !strings.Contains(long, excerpt) || len(excerpt) > 400 {
		t.Fatal("the relevant continuous passage was not selected")
	}
}

func installFixturePDFTools(t *testing.T, text string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("RESEARCH_PDF_TOOL_DIR", dir)
	// These parser process doubles test the boundary protocol. They do not
	// author PDFs or replace the separate real saved-paper acceptance check.
	for name, output := range map[string]string{"pdfinfo": "Pages: 3\n", "pdftotext": text} {
		script := "#!/bin/sh\nprintf '%s' '" + strings.ReplaceAll(output, "'", "'\"'\"'") + "'\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReadingJobCacheChecksSavedVersionBeforeModel(t *testing.T) {
	quote := readingFixture().Sources[0].Pages[1].Choices[0]
	installFixturePDFTools(t, "This introduction describes the scope of the selected investigation.\f"+quote+"\f\f")
	s, i := deskFixture(t)
	s.archive = fakePDFArchive(t, false, false, 0)
	if _, err := importCatalog(i.db, "arxiv", "jsonl", strings.NewReader(`{"id":"0704.0001","title":"River channels","abstract":"These observations link erosion to changes in river channels."}`), 0); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": readingContent(2, quote)}}}})
	}))
	defer model.Close()
	i.answerURL, i.model = model.URL, "fixture"
	data := sessionFixture()
	data.SelectedIDs = []string{"arxiv:0704.0001"}
	data.PDFMode = true
	x, err := s.save("owner", "", 0, data)
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 2; n++ {
		j, err := s.submit("owner", x.ID, x.Revision, "pdf-answer")
		if err != nil {
			t.Fatal(err)
		}
		j = awaitDeskJob(t, s, "owner", j.ID)
		if j.Status != "complete" || j.Cached != (n == 1) || calls.Load() != 1 {
			t.Fatalf("reading cache: %+v calls=%d", j, calls.Load())
		}
		if strings.Contains(string(j.Result), "nas_ref") || strings.Contains(string(j.Result), "s3://") {
			t.Fatal("reading leaked internal references")
		}
	}
	s.archive.mu.Lock()
	s.archive.cachedPDF.Version = 4
	s.archive.cachedAt = time.Now()
	s.archive.mu.Unlock()
	j, err := s.submit("owner", x.ID, x.Revision, "pdf-answer")
	if err != nil {
		t.Fatal(err)
	}
	j = awaitDeskJob(t, s, "owner", j.ID)
	if j.Status != "complete" || j.Cached || calls.Load() != 2 {
		t.Fatalf("saved version invalidation: %+v calls=%d", j, calls.Load())
	}
}

func TestReadingUnreadablePagesFailBeforeModelReservation(t *testing.T) {
	installFixturePDFTools(t, "\f\f\f")
	s, i := deskFixture(t)
	s.archive = fakePDFArchive(t, false, false, 0)
	if _, err := importCatalog(i.db, "arxiv", "jsonl", strings.NewReader(`{"id":"0704.0001","title":"Scanned paper","abstract":"This supplied abstract has sufficient words for source discovery."}`), 0); err != nil {
		t.Fatal(err)
	}
	i.answerURL, i.model = "http://127.0.0.1:1", "unreachable-model"
	data := sessionFixture()
	data.SelectedIDs = []string{"arxiv:0704.0001"}
	data.PDFMode = true
	x, err := s.save("owner", "", 0, data)
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.submit("owner", x.ID, x.Revision, "pdf-answer")
	if err != nil {
		t.Fatal(err)
	}
	j = awaitDeskJob(t, s, "owner", j.ID)
	if j.Status != "failed" || !strings.Contains(j.Error, "no usable text") {
		t.Fatalf("scanned paper accepted: %+v", j)
	}
	stats, err := i.stats()
	if err != nil || stats.(map[string]any)["model_calls_today"].(int) != 0 {
		t.Fatalf("unreadable PDF charged a model: %+v %v", stats, err)
	}
}

func TestReadingOutputBoundsAndSelectedPages(t *testing.T) {
	w := &limitedPDFOutput{limit: 10}
	if n, err := w.Write([]byte("12345678")); err != nil || n != 8 {
		t.Fatal("initial output failed")
	}
	if _, err := w.Write([]byte("9012")); err == nil || w.Len() != 8 {
		t.Fatal("output was not bounded")
	}
	text := PaperText{Pages: []PDFPageText{{1, "This introduction has enough words to read."}, {2, "These methods discuss geological maps and erosion."}, {3, ""}, {4, "This conclusion discusses unrelated language models."}}}
	pages := relevantPDFPages(text, "geological maps", 2)
	if len(pages) != 2 || pages[0].Page != 1 || pages[1].Page != 2 {
		t.Fatalf("wrong page selection: %+v", pages)
	}
	text.Pages = append(text.Pages, PDFPageText{5, strings.Repeat("[1] Geological maps and erosion datasets. 2023. ", 8)})
	pages = relevantPDFPages(text, "geological maps", 2)
	if len(pages) != 2 || pages[1].Page != 2 {
		t.Fatal("bibliography titles displaced the selected paper's content")
	}
	pages = relevantPDFPages(text, "geological references", 2)
	if len(pages) != 2 || pages[1].Page != 5 {
		t.Fatal("explicit reference lookup omitted the bibliography")
	}
	link := pdfPageLink("arxiv:hep-th/9901001", strings.Repeat("a", 64), 3)
	if !strings.HasPrefix(link, "/api/pdf?") || !strings.Contains(link, "sha256="+strings.Repeat("a", 64)) || !strings.HasSuffix(link, "#page=3") {
		t.Fatalf("unbound citation: %s", link)
	}
}
