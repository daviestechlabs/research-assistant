package main

import (
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
)

const maxPhysicalQuoteWords = 80
const maxReadingChoiceBytes = 2400

var readingSentenceEnds = regexp.MustCompile(`[.!?](?:\s+|$)`)
var readingNavigation = regexp.MustCompile(`(?i)(methodology follows|paper is organized|remainder of this|\bEq\.$)`)

// Abstract quotes retain their smaller bound. Physical-page citations also
// need complete conditions and OCR text with spaces inside words.
func verifyPhysicalQuote(text, quote string) (string, error) {
	quote = strings.Join(strings.Fields(quote), " ")
	n := len(words.FindAllString(quote, -1))
	if n < 5 || n > maxPhysicalQuoteWords || !strings.Contains(strings.Join(strings.Fields(text), " "), quote) {
		return "", errors.New("citation does not match the selected physical page")
	}
	return quote, nil
}

type readingEvidenceSpan struct{ start, end int }

// Rank complete source spans across the consulted excerpt. Greedy selection
// favors new relevant text over overlapping copies. Short bullet labels stay
// with the next sentence; adjacent sentences can retain a qualification.
func readingEvidenceChoices(paper PaperText, page PDFPageText, excerpt, question string, limit int) []string {
	excerpt = strings.Join(strings.Fields(excerpt), " ")
	pageText := strings.Join(strings.Fields(page.Text), " ")
	if limit < 1 || excerpt == "" || !strings.Contains(pageText, excerpt) {
		return nil
	}
	units := []readingEvidenceSpan{}
	start := 0
	prior := strings.TrimSpace(pageText[:strings.Index(pageText, excerpt)])
	partialFirst := prior != "" && !strings.ContainsAny(prior[len(prior)-1:], ".!?")
	for _, boundary := range readingSentenceEnds.FindAllStringIndex(excerpt, -1) {
		if partialFirst {
			start = boundary[1]
			partialFirst = false
			continue
		}
		if len(words.FindAllString(excerpt[start:boundary[1]], -1)) < 5 {
			continue
		}
		units = append(units, readingEvidenceSpan{start, boundary[1]})
		start = boundary[1]
	}
	candidates := []readingEvidenceSpan{}
	seen := map[string]bool{}
	add := func(span readingEvidenceSpan) {
		quote := strings.TrimSpace(excerpt[span.start:span.end])
		if _, err := verifyPhysicalQuote(pageText, quote); err != nil || seen[quote] || strings.Count(quote, "(") != strings.Count(quote, ")") || strings.Count(quote, "[") != strings.Count(quote, "]") || readingNavigation.MatchString(quote) {
			return
		}
		seen[quote] = true
		candidates = append(candidates, span)
	}
	for n, unit := range units {
		add(unit)
		if n+1 < len(units) {
			add(readingEvidenceSpan{unit.start, units[n+1].end})
		}
	}
	if len(candidates) == 0 {
		// Unpunctuated extraction still supports exact bounded passages.
		// This fallback does not assert sentence completeness.
		fallback, total := []string{}, 0
		for _, quote := range completeEvidenceChoices(excerpt, min(limit, 8)) {
			if total+len(quote) <= maxReadingChoiceBytes {
				fallback = append(fallback, quote)
				total += len(quote)
			}
		}
		return fallback
	}
	hits := bookTermHits(excerpt, bookQueryTerms(paper, question))
	covered := make([]bool, len(excerpt))
	result, used, total := []string{}, map[int]bool{}, 0
	for len(result) < min(limit, 12) {
		best, score := -1, -1.0
		for n, span := range candidates {
			quote := strings.TrimSpace(excerpt[span.start:span.end])
			if used[n] || total+len(quote) > maxReadingChoiceBytes {
				continue
			}
			novel, counts := 0, map[int]int{}
			for at := span.start; at < span.end; at++ {
				if !covered[at] {
					novel++
				}
			}
			if novel == 0 {
				continue
			}
			for _, hit := range hits {
				if hit.start >= span.start && hit.start < span.end && !covered[hit.start] {
					counts[hit.term]++
				}
			}
			value := 0.0
			for _, count := range counts {
				value += 1 + float64(min(count, 3))*.1
			}
			// Break equal relevance in favor of new context, not repeated text.
			value += float64(novel) / float64(len(excerpt)) * .2
			if value > score {
				best, score = n, value
			}
		}
		if best < 0 {
			break
		}
		span := candidates[best]
		quote := strings.TrimSpace(excerpt[span.start:span.end])
		result = append(result, quote)
		total += len(quote)
		used[best] = true
		for at := span.start; at < span.end; at++ {
			covered[at] = true
		}
	}
	return result
}

// Diagnose one bounded operator-supplied cached page. No PDF, model, cache
// mutation, credential, or NAS lookup is part of this command.
func inspectPageEvidence(input io.Reader, question string) (map[string]any, error) {
	var record struct {
		ID         string      `json:"id"`
		Version    int64       `json:"version"`
		SHA        string      `json:"sha256"`
		Extraction string      `json:"extraction"`
		Page       PDFPageText `json:"page"`
	}
	data, err := io.ReadAll(io.LimitReader(input, 65537))
	if err != nil || len(data) > 65536 {
		return nil, errors.New("cached page input exceeds its bound")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&record) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("cached page input is invalid")
	}
	id, err := canonicalSavedID(record.ID)
	if err != nil || id != record.ID || record.Page.Page < 1 || record.Page.Page > readingPageLimit(id) || len(record.Page.Text) > 32768 || record.Version < 1 || !shaPattern.MatchString(record.SHA) || len(record.Extraction) > 256 || len(question) < 1 || len(question) > 2000 {
		return nil, errors.New("cached page metadata is invalid")
	}
	paper := PaperText{ID: record.ID}
	excerpt := readingPageExcerpt(paper, record.Page, question, 2200)
	return map[string]any{"scope": "Operator-supplied cached page text; not a new extraction or generated answer", "id": record.ID, "saved_version": record.Version, "saved_sha256": record.SHA, "physical_page": record.Page.Page, "page_text_sha256": textSHA([]byte(record.Page.Text)), "extraction": record.Extraction, "legacy_choices": completeEvidenceChoices(excerpt, 8), "choices": readingEvidenceChoices(paper, record.Page, excerpt, question, 12), "model_calls": 0}, nil
}
