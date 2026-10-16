package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

func postgresFilter(q SearchRequest) (string, []string) {
	where := ""
	args := []string{}
	for _, f := range []struct{ column, value string }{{"source", q.Source}, {"language", q.Language}} {
		if f.value != "" {
			where += " AND d." + f.column + "=?"
			args = append(args, f.value)
		}
	}
	if q.Category != "" {
		where += " AND d.category_tags @> ARRAY[?]::text[]"
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
func (p *postgresCatalog) search(q SearchRequest) (SearchResponse, error) {
	start := time.Now()
	out := SearchResponse{Documents: []Document{}, Method: "postgres-abstract-tsrank"}
	if e := q.validate(); e != nil {
		return out, e
	}
	if q.Source == "gutenberg" {
		out.Method = "postgres-catalog-tsrank"
	}
	filter, args := postgresFilter(q)
	ids := []string{}
	for _, source := range []string{"arxiv", "gutenberg"} {
		if id, e := canonicalID(source, q.Query); e == nil {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		rows, e := p.rows("SELECT "+documentColumns+",0 FROM documents d WHERE d.id=?"+filter+" LIMIT 1", append([]string{id}, args...)...)
		if e != nil {
			return out, e
		}
		if len(rows) > 0 {
			out.Documents = rowsToDocuments(rows)
			out.Method = "identifier"
			out.ElapsedMS = time.Since(start).Milliseconds()
			return out, nil
		}
	}
	if doiID.MatchString(q.Query) {
		rows, e := p.rows("SELECT "+documentColumns+",0 FROM documents d WHERE d.doi=?"+filter+" LIMIT 1", append([]string{q.Query}, args...)...)
		if e != nil {
			return out, e
		}
		out.Documents = rowsToDocuments(rows)
	}
	if len(ids) > 0 || doiID.MatchString(q.Query) {
		out.Method = "identifier"
		out.ElapsedMS = time.Since(start).Milliseconds()
		return out, nil
	}
	raw := words.FindAllString(q.Query, -1)
	if len(raw) == 0 {
		return out, errors.New("query needs searchable words or an identifier")
	}
	terms := topicTerms(q.Query)
	search := func(normalized bool, minimum int) ([][]string, error) {
		labels := raw
		if normalized {
			labels = []string{}
			for _, term := range terms {
				label := term.label
				if label == "I/O" {
					label = "input output io"
				}
				labels = append(labels, label)
			}
		}
		if len(labels) == 0 {
			return [][]string{}, nil
		}
		queries, names, values := []string{}, []string{}, []string{}
		for n, label := range labels {
			name := fmt.Sprintf("term%d", n)
			query := "plainto_tsquery('english',?)"
			if label == "input output io" {
				query = "(phraseto_tsquery('english','input output') || plainto_tsquery('english','io'))"
			} else {
				values = append(values, label)
			}
			queries = append(queries, query+" AS "+name)
			names = append(names, name)
		}
		expression, e := postgresTopicMatch(names, minimum)
		if e != nil {
			return nil, e
		}
		vector := "d.abstract_search"
		if q.Source == "gutenberg" {
			vector = "d.catalog_search"
		}
		// Keep the minimum inside the indexed tsquery. A CASE count over a broad
		// OR makes PostgreSQL scan the corpus before it can reject weak matches.
		cte := "WITH terms AS (SELECT " + strings.Join(queries, ",") + "),chosen AS (SELECT " + expression + " AS query FROM terms) "
		sql := cte + "SELECT " + documentColumns + ",ts_rank_cd(" + vector + ",t.query,32) FROM documents d CROSS JOIN chosen t WHERE " + vector + " @@ t.query" + filter + " ORDER BY ts_rank_cd(" + vector + ",t.query,32) DESC,d.id LIMIT CAST(? AS BIGINT)"
		values = append(values, args...)
		values = append(values, strconv.Itoa(q.Limit))
		return p.rows(sql, values...)
	}
	minimum := len(raw)
	if q.Broad {
		minimum = 1
	}
	rows, e := search(q.Broad, minimum)
	if e != nil {
		return out, e
	}
	if q.Broad {
		out.Method += "-broad"
	}
	if len(rows) == 0 && !q.Broad && len(terms) > 0 {
		rows, e = search(true, len(terms))
		if e != nil {
			return out, e
		}
		minimum = len(terms)
		reason := "Removed filler words and normalized technical terms after no exact keyword results."
		for _, required := range postgresFallbackMinimums(len(terms)) {
			if len(rows) > 0 {
				break
			}
			rows, e = search(true, required)
			if e != nil {
				return out, e
			}
			minimum = required
			reason = fmt.Sprintf("No exact keyword results. Broadened to records matching at least %d topic terms.", minimum)
		}
		labels := []string{}
		for _, term := range terms {
			labels = append(labels, term.label)
		}
		out.Adjustment = &SearchAdjustment{Reason: reason, Terms: labels, MinimumMatch: minimum}
		out.Method += "-fallback"
	}
	out.Documents = rowsToDocuments(rows)
	out.ElapsedMS = time.Since(start).Milliseconds()
	return out, nil
}

// Prefer the closest available topic match before reaching the two-term floor.
// Bound combination expansion for long questions; the two-term expression is
// factorized and remains quadratic even at the request's 24-term limit.
func postgresFallbackMinimums(terms int) []int {
	out := []int{}
	for n := terms - 1; n >= 2; n-- {
		if n == 2 || postgresCombinations(terms, n) <= 128 {
			out = append(out, n)
		}
	}
	return out
}
func postgresCombinations(n, k int) int {
	if k < 0 || k > n {
		return 0
	}
	if k > n-k {
		k = n - k
	}
	x := 1
	for i := 1; i <= k; i++ {
		x = x * (n - k + i) / i
	}
	return x
}
func postgresTopicMatch(names []string, minimum int) (string, error) {
	if len(names) < 1 || len(names) > 24 || minimum < 1 || minimum > len(names) {
		return "", errors.New("invalid PostgreSQL topic threshold")
	}
	if minimum == 1 {
		return "(" + strings.Join(names, " || ") + ")", nil
	}
	if minimum == len(names) {
		return "(" + strings.Join(names, " && ") + ")", nil
	}
	parts := []string{}
	if minimum == 2 {
		for n := 0; n < len(names)-1; n++ {
			parts = append(parts, "("+names[n]+" && ("+strings.Join(names[n+1:], " || ")+"))")
		}
	} else {
		if postgresCombinations(len(names), minimum) > 128 {
			return "", errors.New("PostgreSQL topic threshold exceeds query bounds")
		}
		var choose func(int, []string)
		choose = func(start int, selected []string) {
			if len(selected) == minimum {
				parts = append(parts, "("+strings.Join(selected, " && ")+")")
				return
			}
			for n := start; n <= len(names)-(minimum-len(selected)); n++ {
				choose(n+1, append(selected, names[n]))
			}
		}
		choose(0, nil)
	}
	return "(" + strings.Join(parts, " || ") + ")", nil
}
