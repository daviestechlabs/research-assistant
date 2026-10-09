package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestBookQuestionFindsRuleInsteadOfCatalogPadding(t *testing.T) {
	approvedBookFixture(t)
	question := "What does this 2014 Player's Handbook say about maintaining concentration and what ends it?"
	text := PaperText{ID: "dnd:players-handbook", Pages: []PDFPageText{
		{5, strings.Repeat("This handbook says what this handbook does. ", 100)},
		{19, "The index lists concentration in this handbook."},
		{185, "C o n c e n t r a t i o n. You maintain concentration. Damage can end concentration. Becoming incapacitated ends concentration."},
		{215, "One particular spell requires concentration and has its own special ending."},
	}}
	if got := bookQueryTerms(text, question); !reflect.DeepEqual(got, []string{"maintain", "concentration", "end"}) {
		t.Fatalf("catalog padding enters retrieval: %v", got)
	}
	pages := relevantPDFPages(text, question, 1)
	if len(pages) != 1 || pages[0].Page != 185 {
		t.Fatalf("the core rule loses to question padding: %+v", pages)
	}
	if got := relevantPDFPages(text, "Where does this handbook mention concentration?", 0); got != nil {
		t.Fatal("zero page limit accepted")
	}
}

func TestBookExcerptPreservesOCRAndFindsLateRule(t *testing.T) {
	paper := PaperText{ID: "dnd:players-handbook"}
	rule := "C o n c e n t r a t i o n. You m a i n t a i n concentration. Taking damage ends concentration."
	page := PDFPageText{185, strings.Repeat("Unrelated magic school descriptions. ", 150) + rule}
	excerpt := readingPageExcerpt(paper, page, "How do I maintain concentration and what ends it?", 450)
	if !strings.Contains(excerpt, rule) || !strings.Contains(page.Text, excerpt) || len(excerpt) > 450 {
		t.Fatalf("rule passage omitted or original text changed: %q", excerpt)
	}
	pages := relevantPDFPages(PaperText{ID: paper.ID, Pages: []PDFPageText{{1, ""}, {2, "A readable first page with no query match."}}}, "unfindable", 4)
	if len(pages) != 1 || pages[0].Page != 2 {
		t.Fatalf("no-match context fallback failed: %+v", pages)
	}
}
