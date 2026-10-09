package main

import (
	"math"
	"sort"
	"strings"
)

// Book questions contain conversational padding and catalog labels. Neither
// should displace a rule passage. Score terms within this selected book only.
func bookQueryTerms(paper PaperText, question string) []string {
	ignored := map[string]bool{}
	for _, word := range strings.Fields("a an the what which who when where why how does do did is are was were be been this that these those it its in on at of for from to with and or about say says tell explain please book books pdf saved") {
		ignored[word] = true
	}
	books, _ := approvedBooks()
	if book, ok := books[paper.ID]; ok {
		for _, word := range words.FindAllString(strings.ToLower(book.Title+" "+book.Edition+" "+book.Ruleset), -1) {
			ignored[bookWordRoot(word)] = true
		}
	}
	seen, result := map[string]bool{}, []string{}
	for _, word := range words.FindAllString(strings.ToLower(question), -1) {
		if ignored[word] {
			continue
		}
		word = bookWordRoot(word)
		if len(word) < 3 || ignored[word] || seen[word] || word[0] < 'a' || word[0] > 'z' {
			continue
		}
		seen[word] = true
		result = append(result, word)
	}
	return result
}

func bookWordRoot(word string) string {
	for _, suffix := range []string{"ing", "ed", "s"} {
		if len(word) >= len(suffix)+3 && strings.HasSuffix(word, suffix) {
			return strings.TrimSuffix(word, suffix)
		}
	}
	return word
}

type bookTermHit struct {
	start int
	term  int
}

// OCR can insert spaces inside words. Fold letters for matching only; retain
// original byte positions so passages and verified quotations never change.
func bookTermHits(text string, terms []string) []bookTermHit {
	hits := []bookTermHit{}
	folded, offsets := []byte{}, []int{}
	for n, c := range []byte(text) {
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c >= 'a' && c <= 'z' {
			folded, offsets = append(folded, c), append(offsets, n)
		}
	}
	for term, value := range terms {
		if len(value) >= 5 {
			for start := 0; start < len(folded); {
				at := strings.Index(string(folded[start:]), value)
				if at < 0 {
					break
				}
				at += start
				hits = append(hits, bookTermHit{offsets[at], term})
				start = at + len(value)
			}
		} else {
			for _, position := range words.FindAllStringIndex(text, -1) {
				if bookWordRoot(strings.ToLower(text[position[0]:position[1]])) == value {
					hits = append(hits, bookTermHit{position[0], term})
				}
			}
		}
	}
	return hits
}

func relevantBookPages(paper PaperText, question string, limit int) []PDFPageText {
	terms := bookQueryTerms(paper, question)
	type candidate struct {
		page   PDFPageText
		counts []int
		length int
		score  float64
	}
	candidates, frequencies, length := []candidate{}, make([]int, len(terms)), 0
	for _, page := range paper.Pages {
		n := len(words.FindAllString(page.Text, -1))
		if n < 5 {
			continue
		}
		c := candidate{page: page, counts: make([]int, len(terms)), length: n}
		for _, hit := range bookTermHits(page.Text, terms) {
			c.counts[hit.term]++
		}
		for term, count := range c.counts {
			if count > 0 {
				frequencies[term]++
			}
		}
		candidates, length = append(candidates, c), length+n
	}
	if len(candidates) == 0 || limit < 1 {
		return nil
	}
	average := float64(length) / float64(len(candidates))
	for n := range candidates {
		c := &candidates[n]
		for term, count := range c.counts {
			if count == 0 {
				continue
			}
			idf := math.Log(1 + (float64(len(candidates)-frequencies[term])+.5)/(float64(frequencies[term])+.5))
			c.score += idf * float64(count) * 2.2 / (float64(count) + 1.2*(.25+.75*float64(c.length)/average))
		}
	}
	sort.SliceStable(candidates, func(a, b int) bool { return candidates[a].score > candidates[b].score })
	result := []PDFPageText{}
	for _, c := range candidates[:min(limit, len(candidates))] {
		if c.score == 0 && len(result) > 0 {
			break
		}
		result = append(result, c.page)
	}
	sort.Slice(result, func(a, b int) bool { return result[a].Page < result[b].Page })
	return result
}

func readingPageExcerpt(paper PaperText, page PDFPageText, question string, bound int) string {
	if !strings.HasPrefix(paper.ID, "dnd:") {
		return relevantPageExcerpt(page.Text, question, bound)
	}
	if len(page.Text) <= bound {
		return page.Text
	}
	hits := bookTermHits(page.Text, bookQueryTerms(paper, question))
	best, score := 0, 0.0
	for _, hit := range hits {
		start := max(0, hit.start-bound/4)
		counts := map[int]int{}
		for _, other := range hits {
			if other.start >= start && other.start < start+bound {
				counts[other.term]++
			}
		}
		value := 0.0
		for _, count := range counts {
			value += 1 + math.Log1p(float64(count))
		}
		if value > score {
			best, score = start, value
		}
	}
	for best > 0 && page.Text[best-1] != ' ' {
		best--
	}
	excerpt := clip(page.Text[best:], bound)
	if end := strings.LastIndexByte(excerpt, ' '); best+len(excerpt) < len(page.Text) && end > 0 {
		excerpt = excerpt[:end]
	}
	return strings.TrimSpace(excerpt)
}
