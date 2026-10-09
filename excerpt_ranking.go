package main

import (
	"regexp"
	"sort"
	"strings"
)

const excerptRankingContract = "complete-question-excerpts-v1"

func completeEvidenceExcerptChoices(summary string) []string {
	summary = strings.Join(strings.Fields(summary), " ")
	choices := []string{}
	seen := map[string]bool{}
	add := func(value string) {
		value = strings.TrimSpace(value)
		count := len(words.FindAllString(value, -1))
		if count >= 5 && count <= 25 && !seen[value] {
			choices = append(choices, value)
			seen[value] = true
		}
	}
	start := 0
	for _, end := range regexp.MustCompile(`[.!?](?:\s+|$)`).FindAllStringIndex(summary, -1) {
		add(summary[start:end[1]])
		start = end[1]
	}
	add(summary[start:])
	// Complete overlapping windows also cover long sentences, short fragments,
	// and a clipped final sentence. No first/middle/last sampling remains here.
	positions := words.FindAllStringIndex(summary, -1)
	for start := 0; start < len(positions); start += 17 {
		if len(positions)-start < 5 {
			start = max(0, len(positions)-25)
		}
		end := min(start+25, len(positions))
		if end-start < 5 {
			break
		}
		add(summary[positions[start][0]:positions[end-1][1]])
		if end == len(positions) {
			break
		}
	}
	return choices
}

// Reorder the complete verified set; never add, paraphrase, or discard text.
// Lexical overlap helps selection but establishes no scientific support.
func questionExcerptChoices(summary, question string) []string {
	choices := completeEvidenceExcerptChoices(summary)
	stop := map[string]bool{}
	for _, term := range strings.Fields("and the this that which how what does do did in on an to of for from by with show explain abstract paper study source can about is it at") {
		stop[term] = true
	}
	terms := map[string]bool{}
	for _, term := range words.FindAllString(strings.ToLower(question), -1) {
		if len(term) >= 2 && !stop[term] {
			terms[term] = true
		}
	}
	scores := map[string]int{}
	for _, choice := range choices {
		matched := map[string]bool{}
		for _, term := range words.FindAllString(strings.ToLower(choice), -1) {
			if terms[term] && !matched[term] {
				scores[choice]++
				matched[term] = true
			}
		}
	}
	sort.SliceStable(choices, func(a, b int) bool { return scores[choices[a]] > scores[choices[b]] })
	return choices
}
