package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const maxReadingPages = 400
const maxBookReadingPages = 2000
const maxReadingText = 8 << 20
const textExtractionContract = "poppler-reading-order-physical-pages-v1"

var bibliographyMarkers = regexp.MustCompile(`\[[0-9]{1,3}\]`)
var bibliographyYears = regexp.MustCompile(`\b(?:19|20)[0-9]{2}\b`)

type PDFPageText struct {
	Page int    `json:"page"`
	Text string `json:"text"`
}

type PaperText struct {
	ID              string        `json:"id"`
	Version         int64         `json:"version"`
	SHA256          string        `json:"sha256"`
	Pages           []PDFPageText `json:"pages"`
	UnreadablePages []int         `json:"unreadable_pages"`
	Extraction      string        `json:"extraction"`
}

// Limit both successful and malformed parser output. Command cancellation also
// kills a parser that waits after its output pipe exceeds this limit.
type limitedPDFOutput struct {
	bytes.Buffer
	limit int
}

func (w *limitedPDFOutput) Write(p []byte) (int, error) {
	if len(p) > w.limit-w.Len() {
		return 0, errors.New("PDF parser output exceeds its limit")
	}
	return w.Buffer.Write(p)
}

func pdfTool(ctx context.Context, name string, pdf []byte, limit int, args ...string) ([]byte, error) {
	dir := os.Getenv("RESEARCH_PDF_TOOL_DIR")
	if dir == "" {
		dir = "/usr/bin"
	}
	if !filepath.IsAbs(dir) || (name != "pdfinfo" && name != "pdftotext" && name != "pdftoppm") {
		return nil, errors.New("PDF text tools are not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(dir, name), args...)
	cmd.Env = []string{"LC_ALL=C", "LANG=C", "HOME=/tmp"}
	cmd.Stdin = bytes.NewReader(pdf)
	output := &limitedPDFOutput{limit: limit}
	stderr := &limitedPDFOutput{limit: 8192}
	cmd.Stdout, cmd.Stderr = output, stderr
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("PDF text extraction exceeded its deadline")
		}
		// Parser diagnostics can contain source text and file metadata.
		return nil, errors.New("PDF text extraction failed; the saved PDF may be encrypted or unreadable")
	}
	return output.Bytes(), nil
}

// The distro receipt binds shared libraries as well as executable hashes.
// Local operator tools use their executable hashes when no receipt exists.
func pdfExtractionIdentity() (string, error) {
	if receipt, err := os.ReadFile("/pdf-runtime-receipt.json"); err == nil {
		if len(receipt) > 1<<20 {
			return "", errors.New("PDF runtime receipt exceeds its bound")
		}
		h := sha256.Sum256(receipt)
		return textExtractionContract + ":" + hex.EncodeToString(h[:]), nil
	}
	dir := os.Getenv("RESEARCH_PDF_TOOL_DIR")
	if dir == "" {
		dir = "/usr/bin"
	}
	if !filepath.IsAbs(dir) {
		return "", errors.New("PDF text tools are not configured")
	}
	h := sha256.New()
	for _, name := range []string{"pdfinfo", "pdftotext"} {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return "", errors.New("PDF text tools are unavailable")
		}
		_, err = io.Copy(h, io.LimitReader(f, 32<<20))
		f.Close()
		if err != nil {
			return "", errors.New("PDF text tool identity is unavailable")
		}
	}
	return textExtractionContract + ":" + hex.EncodeToString(h.Sum(nil)), nil
}

func physicalPageCount(info string) (int, error) {
	return physicalPageCountWithin(info, maxReadingPages)
}

func physicalPageCountWithin(info string, limit int) (int, error) {
	for _, line := range strings.Split(info, "\n") {
		if strings.HasPrefix(line, "Pages:") {
			n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Pages:")))
			if err != nil || n < 1 || n > limit {
				return 0, fmt.Errorf("selected PDF must contain 1 to %d pages", limit)
			}
			return n, nil
		}
	}
	return 0, errors.New("PDF page count is unavailable")
}

func splitPhysicalPages(text string, count int) ([]PDFPageText, []int, error) {
	return splitPhysicalPagesWithin(text, count, maxReadingPages)
}

func splitPhysicalPagesWithin(text string, count, limit int) ([]PDFPageText, []int, error) {
	if len(text) > maxReadingText || !utf8.ValidString(text) || count < 1 || count > limit {
		return nil, nil, errors.New("extracted PDF text exceeds its contract")
	}
	parts := strings.Split(text, "\f")
	// Poppler writes one trailing form feed, even for a blank final page.
	if len(parts) == count+1 && strings.TrimSpace(parts[len(parts)-1]) == "" {
		parts = parts[:len(parts)-1]
	}
	if len(parts) != count {
		return nil, nil, errors.New("extracted page boundaries do not match the physical PDF pages")
	}
	pages := make([]PDFPageText, count)
	unreadable := []int{}
	for n, p := range parts {
		p = strings.Join(strings.Fields(p), " ")
		if len(words.FindAllString(p, -1)) < 5 {
			unreadable = append(unreadable, n+1)
		}
		pages[n] = PDFPageText{Page: n + 1, Text: p}
	}
	return pages, unreadable, nil
}

func extractPaperText(ctx context.Context, id string, pdf savedPDF) (PaperText, error) {
	contract, err := pdfExtractionIdentity()
	result := PaperText{ID: id, Version: int64(pdf.Version), SHA256: pdf.SHA256, Extraction: contract}
	if err != nil {
		return result, err
	}
	info, err := pdfTool(ctx, "pdfinfo", pdf.Data, 65536, "-")
	if err != nil {
		return result, err
	}
	count, err := physicalPageCountWithin(string(info), readingPageLimit(id))
	if err != nil {
		return result, err
	}
	// Keep Poppler's reading order and default form-feed page boundaries.
	text, err := pdfTool(ctx, "pdftotext", pdf.Data, maxReadingText, "-enc", "UTF-8", "-eol", "unix", "-", "-")
	if err != nil {
		return result, err
	}
	result.Pages, result.UnreadablePages, err = splitPhysicalPagesWithin(string(text), count, readingPageLimit(id))
	if err == nil && len(result.UnreadablePages) == count {
		err = errors.New("this PDF has no usable text; scanned pages need OCR before page-cited answers")
	}
	return result, err
}

func (s *researchDesk) paperText(ctx context.Context, id string) (PaperText, error) {
	if s.archive == nil {
		return PaperText{}, errors.New("saved PDF reading is not configured")
	}
	if !strings.HasPrefix(id, "arxiv:") && !bookID.MatchString(id) {
		return PaperText{}, errors.New("deep reading supports saved papers and private PDF books")
	}
	contract, err := pdfExtractionIdentity()
	if err != nil {
		return PaperText{}, err
	}
	select {
	case s.archive.busy <- struct{}{}:
	case <-ctx.Done():
		return PaperText{}, ctx.Err()
	}
	pdf, err := s.archive.readSourcePDF(ctx, id)
	<-s.archive.busy
	if err != nil {
		return PaperText{}, err
	}
	s.db.mu.Lock()
	_, err = s.db.run(`CREATE TABLE IF NOT EXISTS desk_pdf_text(
 id TEXT NOT NULL,sha TEXT NOT NULL,contract TEXT NOT NULL,updated TEXT NOT NULL,data TEXT NOT NULL,
 PRIMARY KEY(id,sha,contract))`)
	var rows [][]string
	if err == nil {
		rows, err = s.db.rows("SELECT data FROM desk_pdf_text WHERE id=? AND sha=? AND contract=?", id, pdf.SHA256, contract)
	}
	s.db.mu.Unlock()
	if err != nil {
		return PaperText{}, err
	}
	if len(rows) == 1 {
		var cached PaperText
		if json.Unmarshal([]byte(rows[0][0]), &cached) == nil && cached.ID == id && cached.SHA256 == pdf.SHA256 && cached.Version == int64(pdf.Version) && cached.Extraction == contract && len(cached.Pages) > 0 && len(cached.Pages) <= readingPageLimit(id) {
			return cached, nil
		}
	}
	text, err := extractPaperText(ctx, id, pdf)
	if err != nil {
		return text, err
	}
	data, err := json.Marshal(text)
	if err != nil || len(data) > 2*maxReadingText {
		return text, errors.New("PDF text cache exceeds its storage limit")
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	if err = s.db.exec("BEGIN IMMEDIATE"); err != nil {
		return text, err
	}
	defer s.db.exec("ROLLBACK")
	if _, err = s.db.run("INSERT OR REPLACE INTO desk_pdf_text VALUES(?,?,?,?,?)", id, pdf.SHA256, contract, deskTime(), string(data)); err != nil {
		return text, err
	}
	if _, err = s.db.run("DELETE FROM desk_pdf_text WHERE rowid NOT IN (SELECT rowid FROM desk_pdf_text ORDER BY updated DESC LIMIT 64)"); err != nil {
		return text, err
	}
	return text, s.db.exec("COMMIT")
}

func pdfPageLink(id, sha string, page int) string {
	return "/api/pdf?" + url.Values{"id": {id}, "sha256": {sha}}.Encode() + "#page=" + strconv.Itoa(page)
}

func readingPageLimit(id string) int {
	if bookID.MatchString(id) {
		return maxBookReadingPages
	}
	return maxReadingPages
}

// Discovery still searches abstracts. Deep reading chooses explicit physical
// pages and reports the selection; it never claims to interpret every figure.
func relevantPDFPages(paper PaperText, question string, limit int) []PDFPageText {
	if strings.HasPrefix(paper.ID, "dnd:") {
		return relevantBookPages(paper, question, limit)
	}
	if limit < 1 {
		return nil
	}
	terms := map[string]bool{}
	for _, term := range words.FindAllString(strings.ToLower(question), -1) {
		if len(term) > 2 {
			terms[term] = true
		}
	}
	type scored struct {
		page  PDFPageText
		score int
	}
	candidates := []scored{}
	bibliographyQuestion := strings.Contains(strings.ToLower(question), "reference") || strings.Contains(strings.ToLower(question), "bibliograph")
	for _, page := range paper.Pages {
		if len(words.FindAllString(page.Text, -1)) < 5 {
			continue
		}
		// Bibliographies repeat topic words from cited titles. They do not
		// provide the selected paper's methods or findings.
		if !bibliographyQuestion && len(bibliographyMarkers.FindAllString(page.Text, -1)) >= 6 && len(bibliographyYears.FindAllString(page.Text, -1)) >= 6 {
			continue
		}
		score := 0
		for _, word := range words.FindAllString(strings.ToLower(page.Text), -1) {
			if terms[word] {
				score++
			}
		}
		if page.Page == 1 {
			score += 1000
		}
		candidates = append(candidates, scored{page, score})
	}
	sort.SliceStable(candidates, func(a, b int) bool { return candidates[a].score > candidates[b].score })
	result := []PDFPageText{}
	for _, p := range candidates[:min(limit, len(candidates))] {
		result = append(result, p.page)
	}
	sort.Slice(result, func(a, b int) bool { return result[a].Page < result[b].Page })
	return result
}

// Retain this assertion: a parser output limiter must stay an io.Writer.
var _ io.Writer = (*limitedPDFOutput)(nil)
