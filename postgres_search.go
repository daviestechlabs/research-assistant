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
	search := func(normalized, broad, two bool) ([][]string, error) {
		labels := raw
		if normalized {
			labels = []string{}
			for _, t := range terms {
				label := t.label
				if label == "I/O" {
					label = "input output io"
				}
				labels = append(labels, label)
			}
		}
		if len(labels) == 0 {
			return [][]string{}, nil
		}
		queries := []string{}
		values := []string{}
		for _, label := range labels {
			if label == "input output io" {
				queries = append(queries, "(phraseto_tsquery('english','input output') || plainto_tsquery('english','io'))")
			} else {
				queries = append(queries, "plainto_tsquery('english',?)")
				values = append(values, label)
			}
		}
		joiner := " && "
		if broad || two {
			joiner = " || "
		}
		expr := "(" + strings.Join(queries, joiner) + ")"
		vector := "d.abstract_search"
		if q.Source == "gutenberg" {
			vector = "d.catalog_search"
		}
		// Put each bound term in a CTE once; ranking and filtering reuse its query.
		cte := "WITH terms AS (SELECT " + expr + " AS query"
		minimum := ""
		if two {
			cases := []string{}
			for i, query := range queries {
				cte += fmt.Sprintf(",%s AS term%d", query, i)
				if labels[i] != "input output io" {
					values = append(values, labels[i])
				}
				cases = append(cases, fmt.Sprintf("CASE WHEN %s @@ t.term%d THEN 1 ELSE 0 END", vector, i))
			}
			minimum = " AND (" + strings.Join(cases, "+") + ")>=2"
		}
		cte += ") "
		sql := cte + "SELECT " + documentColumns + ",ts_rank_cd(" + vector + ",t.query,32) FROM documents d CROSS JOIN terms t WHERE " + vector + " @@ t.query" + minimum + filter + " ORDER BY ts_rank_cd(" + vector + ",t.query,32) DESC,d.id LIMIT CAST(? AS BIGINT)"
		values = append(values, args...)
		values = append(values, strconv.Itoa(q.Limit))
		return p.rows(sql, values...)
	}
	rows, e := search(q.Broad, q.Broad, false)
	if e != nil {
		return out, e
	}
	if q.Broad {
		out.Method += "-broad"
	}
	if len(rows) == 0 && !q.Broad && len(terms) > 0 {
		rows, e = search(true, false, false)
		if e != nil {
			return out, e
		}
		minimum := len(terms)
		reason := "Removed filler words and normalized technical terms after no exact keyword results."
		if len(rows) == 0 && len(terms) > 2 {
			rows, e = search(true, false, true)
			if e != nil {
				return out, e
			}
			minimum = 2
			reason = "No exact keyword results. Broadened to records matching at least two topic terms."
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
