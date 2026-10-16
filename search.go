package main

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type SearchRequest struct {
	Query    string `json:"query"`
	Source   string `json:"source"`
	Language string `json:"language"`
	Category string `json:"category"`
	From     string `json:"from"`
	To       string `json:"to"`
	Limit    int    `json:"limit"`
	Broad    bool   `json:"broad"`
	Meaning  bool   `json:"meaning,omitempty"`
}

type SearchAdjustment struct {
	Reason       string   `json:"reason"`
	Terms        []string `json:"terms"`
	MinimumMatch int      `json:"minimum_match"`
}

type SearchResponse struct {
	Adjustment        *SearchAdjustment     `json:"search_adjustment,omitempty"`
	Documents         []Document            `json:"documents"`
	ElapsedMS         int64                 `json:"elapsed_ms"`
	Method            string                `json:"method"`
	ModelCalls        int                   `json:"model_calls"`
	Queries           []string              `json:"queries,omitempty"`
	Query             string                `json:"query,omitempty"`
	Coverage          *SemanticCoverage     `json:"coverage,omitempty"`
	EmbeddingCached   bool                  `json:"embedding_cached,omitempty"`
	ReservedUnits     int                   `json:"reserved_units,omitempty"`
	EvidenceLimit     string                `json:"evidence_limit,omitempty"`
	ChatModelIdentity *ServingModelIdentity `json:"chat_model_identity,omitempty"`
}

var words = regexp.MustCompile(`[\pL\pN]+`)
var doiID = regexp.MustCompile(`^10\.\d{4,9}/\S+$`)

const documentColumns = `d.id,d.source,d.title,d.authors,d.summary,d.summary_kind,d.categories,d.language,d.published,d.doi,d.nas_ref,d.fingerprint`

func (q *SearchRequest) validate() error {
	q.Query = strings.TrimSpace(q.Query)
	if q.Query == "" || len(q.Query) > 512 {
		return errors.New("query must contain 1 to 512 bytes")
	}
	if q.Limit == 0 {
		q.Limit = 10
	}
	if q.Limit < 1 || q.Limit > 50 {
		return errors.New("limit must be 1 to 50")
	}
	if q.Source != "" && q.Source != "arxiv" && q.Source != "gutenberg" && q.Source != "dnd" {
		return errors.New("invalid source")
	}
	if len(q.Language) > 16 || len(q.Category) > 128 {
		return errors.New("filter exceeds size limit")
	}
	for _, date := range []string{q.From, q.To} {
		if date != "" {
			if _, err := time.Parse("2006-01-02", date); err != nil {
				return errors.New("date filters use YYYY-MM-DD")
			}
		}
	}
	if q.From != "" && q.To != "" && q.From > q.To {
		return errors.New("from date exceeds to date")
	}
	if len(words.FindAllString(q.Query, -1)) > 24 {
		return errors.New("query has more than 24 terms")
	}
	return nil
}

func filterSQL(q SearchRequest) (string, []string) {
	where := ""
	args := []string{}
	for _, f := range []struct{ column, value string }{{"source", q.Source}, {"language", q.Language}} {
		if f.value != "" {
			where += " AND d." + f.column + "=?"
			args = append(args, f.value)
		}
	}
	if q.Category != "" {
		where += " AND instr(' ' || d.categories || ' ',' ' || ? || ' ')>0"
		args = append(args, q.Category)
	}
	if q.From != "" {
		where += " AND d.published>=?"
		args = append(args, q.From)
	}
	if q.To != "" {
		where += " AND d.published<=? AND d.published!=''"
		args = append(args, q.To)
	}
	return where, args
}

func rowsToDocuments(rows [][]string) []Document {
	docs := make([]Document, 0, len(rows))
	for _, r := range rows {
		d := Document{ID: r[0], Source: r[1], Title: r[2], Authors: r[3], Summary: r[4], SummaryKind: r[5], Categories: r[6], Language: r[7], Published: r[8], DOI: r[9], NASRef: r[10], Fingerprint: r[11]}
		if len(r) > 12 {
			d.Score, _ = strconv.ParseFloat(r[12], 64)
		}
		docs = append(docs, d)
	}
	return docs
}

// A deterministic fallback keeps topic retrieval available without a model call.
// Each term is quoted; user text never becomes FTS query syntax.
var ioTerm = regexp.MustCompile(`(?i)\bi\s*/\s*o\b`)
var searchFillers = func() map[string]bool {
	out := map[string]bool{}
	for _, word := range strings.Fields("a an the to of for with in on at by from and or that which how what are is can could would should do does please me my find show about") {
		out[word] = true
	}
	return out
}()

type topicTerm struct{ label, match string }

func topicTerms(query string) []topicTerm {
	query = ioTerm.ReplaceAllString(query, " researchioterm ")
	terms := []topicTerm{}
	seen := map[string]bool{}
	for _, word := range words.FindAllString(strings.ToLower(query), -1) {
		if searchFillers[word] || seen[word] {
			continue
		}
		seen[word] = true
		term := topicTerm{word, `"` + word + `"`}
		if word == "researchioterm" {
			term = topicTerm{"I/O", `("i o" OR "input output" OR "io")`}
		}
		terms = append(terms, term)
	}
	return terms
}

func topicExpression(terms []topicTerm, broad bool) string {
	parts := make([]string, len(terms))
	for n, term := range terms {
		parts[n] = term.match
	}
	if broad {
		return strings.Join(parts, " OR ")
	}
	return strings.Join(parts, " AND ")
}

// Requiring two distinct terms avoids returning a paper for filler or a lone
// generic word. The 24-term request bound also bounds this expression.
func twoTermExpression(terms []topicTerm) string {
	parts := []string{}
	for n := 0; n < len(terms)-1; n++ {
		remaining := []string{}
		for _, term := range terms[n+1:] {
			remaining = append(remaining, term.match)
		}
		parts = append(parts, "("+terms[n].match+" AND ("+strings.Join(remaining, " OR ")+"))")
	}
	return strings.Join(parts, " OR ")
}

func (d *database) search(q SearchRequest) (SearchResponse, error) {
	if q.Source == "dnd" {
		return d.searchBooks(q)
	}
	if d.pg != nil {
		return d.pg.search(q)
	}
	start := time.Now()
	result := SearchResponse{Method: "bm25", Documents: []Document{}}
	if err := q.validate(); err != nil {
		return result, err
	}
	filter, args := filterSQL(q)
	d.mu.Lock()
	defer d.mu.Unlock()
	// Exact paper, book, and DOI lookups bypass tokenization. Respect all filters.
	ids := []string{}
	for _, source := range []string{"arxiv", "gutenberg"} {
		if id, err := canonicalID(source, q.Query); err == nil {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		rows, err := d.rows("SELECT "+documentColumns+",0 FROM documents d WHERE d.id=?"+filter+" LIMIT 1", append([]string{id}, args...)...)
		if err != nil {
			return result, err
		}
		if len(rows) > 0 {
			result.Documents = rowsToDocuments(rows)
			result.Method = "identifier"
			result.ElapsedMS = time.Since(start).Milliseconds()
			return result, nil
		}
	}
	if doiID.MatchString(q.Query) {
		rows, err := d.rows("SELECT "+documentColumns+",0 FROM documents d WHERE d.doi=?"+filter+" LIMIT 1", append([]string{q.Query}, args...)...)
		if err != nil {
			return result, err
		}
		if len(rows) > 0 {
			result.Documents = rowsToDocuments(rows)
			result.Method = "identifier"
			result.ElapsedMS = time.Since(start).Milliseconds()
			return result, nil
		}
	}
	if len(ids) > 0 || doiID.MatchString(q.Query) {
		result.Method = "identifier"
		result.ElapsedMS = time.Since(start).Milliseconds()
		return result, nil
	}
	tokens := words.FindAllString(q.Query, -1)
	if len(tokens) == 0 {
		return result, errors.New("query needs searchable words or an identifier")
	}
	for i, t := range tokens {
		tokens[i] = `"` + t + `"`
	}
	terms := topicTerms(q.Query)
	match := strings.Join(tokens, " AND ")
	result.Method = "bm25"
	if q.Source != "gutenberg" {
		result.Method = "abstract-bm25"
	}
	if q.Broad {
		if len(terms) == 0 {
			result.ElapsedMS = time.Since(start).Milliseconds()
			return result, nil
		}
		match = topicExpression(terms, true)
		result.Method += "-broad"
	}
	searchMatch := func(expression string) ([][]string, error) {
		if q.Source != "gutenberg" {
			expression = "summary : (" + expression + ")"
		}
		// Use FTS5's optimized ordered LIMIT path with the same filters at every stage.
		return d.rows("SELECT "+documentColumns+",a.rank FROM abstracts a JOIN documents d ON d.rowid=a.rowid WHERE abstracts MATCH ? AND a.rank MATCH 'bm25(5,2,1,1,3)'"+filter+" ORDER BY a.rank LIMIT ?", append(append([]string{expression}, args...), strconv.Itoa(q.Limit))...)
	}
	rows, err := searchMatch(match)
	if err != nil {
		return result, err
	}
	if len(rows) == 0 && !q.Broad && len(terms) > 0 {
		normalized := topicExpression(terms, false)
		if normalized != match {
			rows, err = searchMatch(normalized)
			if err != nil {
				return result, err
			}
		}
		minimum := len(terms)
		reason := "Removed filler words and normalized technical terms after no exact keyword results."
		if len(rows) == 0 && len(terms) > 2 {
			rows, err = searchMatch(twoTermExpression(terms))
			if err != nil {
				return result, err
			}
			minimum = 2
			reason = "No exact keyword results. Broadened to abstracts matching at least two topic terms."
			if q.Source == "gutenberg" {
				reason = "No exact keyword results. Broadened to catalog records matching at least two topic terms."
			}
		}
		if normalized != match || len(terms) > 2 {
			labels := make([]string, len(terms))
			for n, term := range terms {
				labels[n] = term.label
			}
			result.Adjustment = &SearchAdjustment{reason, labels, minimum}
			result.Method += "-fallback"
		}
	}
	result.Documents = rowsToDocuments(rows)
	result.ElapsedMS = time.Since(start).Milliseconds()
	return result, nil
}

func (d *database) getDocuments(ids []string) ([]Document, error) {
	if len(ids) < 1 || len(ids) > 8 {
		return nil, errors.New("select 1 to 8 sources")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	docs := []Document{}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			return nil, errors.New("duplicate source")
		}
		seen[id] = true
		if strings.HasPrefix(id, "dnd:") {
			b, ok := d.books[id]
			if !ok {
				return nil, errors.New("unknown private book")
			}
			docs = append(docs, b.document())
			continue
		}
		r, err := d.rows("SELECT "+documentColumns+" FROM documents d WHERE d.id=?", id)
		if err != nil {
			return nil, err
		}
		if len(r) != 1 {
			return nil, fmt.Errorf("source does not exist: %s", id)
		}
		docs = append(docs, rowsToDocuments(r)[0])
	}
	return docs, nil
}

func (d *database) stats() (any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, err := d.rows("SELECT source,documents,missing_summaries FROM catalog_stats WHERE documents>0 ORDER BY source")
	if err != nil {
		return nil, err
	}
	type count struct {
		Source         string `json:"source"`
		Documents      int    `json:"documents"`
		MissingSummary int    `json:"missing_summary"`
	}
	sources := []count{}
	for _, row := range r {
		n, _ := strconv.Atoi(row[1])
		missing, _ := strconv.Atoi(row[2])
		sources = append(sources, count{row[0], n, missing})
	}
	usage, err := d.rows("SELECT day,kind,calls,units FROM usage WHERE day=?", time.Now().UTC().Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	calls, units := 0, 0
	for _, row := range usage {
		n, _ := strconv.Atoi(row[2])
		calls += n
		n, _ = strconv.Atoi(row[3])
		units += n
	}
	return map[string]any{"metadata_backend": func() string {
		if d.pg != nil {
			return "postgres"
		}
		return "sqlite"
	}(), "sources": sources, "usage_today": usage, "model_calls_today": calls, "reserved_units_today": units, "search_pdf_reads": 0, "schema": 2}, nil
}
