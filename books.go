package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Only the reviewed library's exact objects can enter the private reader.
//
//go:embed books.json
var bookManifest []byte

const maxBookBytes = 192 << 20

var bookID = regexp.MustCompile(`^dnd:[a-z0-9]+(?:-[a-z0-9]+)*$`)

type libraryBook struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Edition   string `json:"edition"`
	Ruleset   string `json:"ruleset"`
	Category  string `json:"category"`
	MediaType string `json:"media_type"`
	Bytes     int64  `json:"bytes"`
	SHA256    string `json:"sha256"`
	Key       string `json:"key"`
}

func approvedBooks() (map[string]libraryBook, error) {
	data := bookManifest
	if file := os.Getenv("RESEARCH_BOOK_MANIFEST"); file != "" {
		var err error
		data, err = os.ReadFile(file)
		if err != nil || len(data) > 1<<20 {
			return nil, errors.New("private book manifest is unavailable or exceeds its limit")
		}
	}
	if string(bytes.TrimSpace(data)) == "{}" {
		return map[string]libraryBook{}, nil
	}
	expectedPlan := os.Getenv("RESEARCH_BOOK_PLAN_SHA256")
	expectedCount, countErr := strconv.Atoi(os.Getenv("RESEARCH_BOOK_COUNT"))
	var manifest struct {
		Schema       string        `json:"schema"`
		Plan         string        `json:"mirror_plan_file_sha256"`
		Retrieval    bool          `json:"local_retrieval"`
		Distribution bool          `json:"distribution"`
		Entries      []libraryBook `json:"entries"`
	}
	if json.Unmarshal(data, &manifest) != nil || manifest.Schema != "research-private-book-library-v1" || !shaPattern.MatchString(expectedPlan) || manifest.Plan != expectedPlan || !manifest.Retrieval || manifest.Distribution || countErr != nil || expectedCount < 1 || len(manifest.Entries) != expectedCount {
		return nil, errors.New("private book library lacks verified retrieval authority")
	}
	books := map[string]libraryBook{}
	for _, book := range manifest.Entries {
		ext := ".pdf"
		if book.MediaType == "application/epub+zip" {
			ext = ".epub"
		} else if book.MediaType != "application/pdf" {
			return nil, errors.New("unsupported private book format")
		}
		if !bookID.MatchString(book.ID) || !shaPattern.MatchString(book.SHA256) || book.Bytes < 1 || book.Bytes > maxBookBytes || book.Title == "" || book.Edition == "" || book.Ruleset == "" || !strings.HasPrefix(book.Key, "books/") || strings.Contains(strings.TrimPrefix(book.Key, "books/"), "/") || path.Clean(book.Key) != book.Key || !strings.HasSuffix(book.Key, ext) || books[book.ID].ID != "" {
			return nil, errors.New("private book identity or object bounds are invalid")
		}
		books[book.ID] = book
	}
	return books, nil
}

func (b libraryBook) document() Document {
	d := Document{ID: b.ID, Source: "dnd", Title: b.Title, Authors: "Private tabletop book library", Summary: "Saved book · edition: " + b.Edition + " · ruleset: " + b.Ruleset + " · category: " + b.Category + ". Catalog metadata only. Questions use selected saved PDF pages. Physical PDF page numbers can differ from printed page numbers.", SummaryKind: "book-catalog", Categories: b.Category, Language: "en", Edition: b.Edition, Ruleset: b.Ruleset, MediaType: b.MediaType}
	d.Fingerprint = textSHA([]byte(string(mustBookJSON(b))))
	return d
}

func (d *database) searchBooks(q SearchRequest) (SearchResponse, error) {
	start := time.Now()
	r := SearchResponse{Documents: []Document{}, Method: "private-book-catalog", ModelCalls: 0, EvidenceLimit: "Book search matches titles and catalog metadata. Select a PDF book and ask a question to search its saved pages. EPUB titles are available for download."}
	if err := q.validate(); err != nil {
		return r, err
	}
	if d.books == nil {
		return r, errors.New("private book library is unavailable")
	}
	if q.Meaning {
		return r, errors.New("abstract meaning search does not cover private books")
	}
	tokens := words.FindAllString(strings.ToLower(q.Query), -1)
	for _, b := range d.books {
		text := strings.ToLower(b.Title + " " + b.Ruleset + " " + b.Edition + " " + b.Category)
		matches := 0
		for _, token := range tokens {
			if strings.Contains(text, token) {
				matches++
			}
		}
		if q.Query == "*" || q.Query == b.ID || (len(tokens) > 0 && (matches == len(tokens) || (q.Broad && matches > 0))) {
			r.Documents = append(r.Documents, b.document())
		}
	}
	sortBookDocuments(r.Documents)
	if len(r.Documents) > q.Limit {
		r.Documents = r.Documents[:q.Limit]
	}
	r.ElapsedMS = time.Since(start).Milliseconds()
	return r, nil
}

func (a *pdfArchive) readSourcePDF(ctx context.Context, id string) (savedPDF, error) {
	if strings.HasPrefix(id, "arxiv:") {
		return a.readPDF(ctx, strings.TrimPrefix(id, "arxiv:"))
	}
	book, ok := a.books[id]
	if !ok || book.MediaType != "application/pdf" {
		return savedPDF{}, errors.New("select a saved PDF book; EPUB files can be downloaded")
	}
	return a.readBook(ctx, book)
}

func (a *pdfArchive) readBook(ctx context.Context, book libraryBook) (savedPDF, error) {
	a.mu.Lock()
	if a.cachedID == book.ID && a.cachedPDF.SHA256 == book.SHA256 && time.Since(a.cachedAt) < 5*time.Minute {
		p := a.cachedPDF
		a.mu.Unlock()
		return p, nil
	}
	a.mu.Unlock()
	// Re-read the actual immutable reviewed object; ETag alone is never a hash.
	r, err := a.request(ctx, "HEAD", book.Key, nil, nil)
	if err != nil {
		return savedPDF{}, err
	}
	etag := r.Header.Get("ETag")
	size := r.ContentLength
	r.Body.Close()
	if size != book.Bytes || etag == "" || size > maxBookBytes {
		return savedPDF{}, errors.New("saved book changed or exceeds its verified bounds")
	}
	r, err = a.request(ctx, "GET", book.Key, nil, http.Header{"If-Match": {etag}})
	if err != nil {
		return savedPDF{}, err
	}
	defer r.Body.Close()
	if r.ContentLength != book.Bytes {
		return savedPDF{}, errors.New("saved book byte count changed")
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, book.Bytes+1))
	if err != nil || int64(len(data)) != book.Bytes || textSHA(data) != book.SHA256 {
		return savedPDF{}, errors.New("saved book failed its reviewed content checksum")
	}
	if (book.MediaType == "application/pdf" && !bytes.HasPrefix(data, []byte("%PDF-"))) || (book.MediaType == "application/epub+zip" && !bytes.HasPrefix(data, []byte("PK"))) {
		return savedPDF{}, errors.New("saved book format is invalid")
	}
	p := savedPDF{Data: data, Version: 1, SHA256: book.SHA256}
	a.mu.Lock()
	a.cachedID = book.ID
	a.cachedPDF = p
	a.cachedAt = time.Now()
	a.mu.Unlock()
	return p, nil
}

func (a *pdfArchive) serveBook(w http.ResponseWriter, r *http.Request) {
	book, ok := a.books[r.URL.Query().Get("id")]
	if !ok {
		http.Error(w, "Unknown saved book", 404)
		return
	}
	if book.MediaType == "application/pdf" {
		a.servePDF(w, r)
		return
	}
	select {
	case a.busy <- struct{}{}:
	default:
		http.Error(w, "Another saved book is loading. Try again shortly.", 429)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	data, err := a.readBook(ctx, book)
	<-a.busy
	if err != nil {
		http.Error(w, "The saved book failed verification or could not load.", 502)
		return
	}
	if sha := r.URL.Query().Get("sha256"); sha != "" && sha != data.SHA256 {
		http.Error(w, "The saved book checksum does not match.", 409)
		return
	}
	w.Header().Set("Content-Type", book.MediaType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+strings.TrimPrefix(book.ID, "dnd:")+`.epub"`)
	w.Header().Set("ETag", `"`+data.SHA256+`"`)
	http.ServeContent(w, r, book.ID+".epub", time.Time{}, bytes.NewReader(data.Data))
}

func canonicalSavedID(id string) (string, error) {
	if bookID.MatchString(id) {
		return id, nil
	}
	return canonicalID("arxiv", id)
}

func bookPDFLink(id string) string { return "/api/pdf?" + url.Values{"id": {id}}.Encode() }

func sortBookDocuments(docs []Document) {
	sort.Slice(docs, func(i, j int) bool { return docs[i].Title < docs[j].Title })
}

func mustBookJSON(b libraryBook) []byte { data, _ := json.Marshal(b); return data }
