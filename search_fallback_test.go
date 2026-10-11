package main

import (
	"strings"
	"testing"
)

func TestNaturalLanguageFallbackKeepsAbstractScopeAndFilters(t *testing.T) {
	d := testDB(t)
	fixture := `{"id":"2401.10001","title":"Storage scheduling","abstract":"Adaptive I/O scheduling reduces saturation in storage systems.","categories":"cs.OS","published":"2024-01-01"}
{"id":"2401.10002","title":"Experimental algorithms to reduce I/O saturation","abstract":"A geological experiment measures minerals.","categories":"cs.OS","published":"2024-01-01"}
{"id":"2401.10003","title":"Other field","abstract":"Algorithms reduce saturation in chemical experiments.","categories":"physics.chem-ph","published":"2024-01-01"}
{"id":"2401.10004","title":"Single-term distraction","abstract":"Algorithms for sorting numbers.","categories":"cs.OS","published":"2024-01-01"}`
	if _, err := importCatalog(d, "arxiv", "jsonl", strings.NewReader(fixture), 0); err != nil {
		t.Fatal(err)
	}
	query := SearchRequest{Query: "experimental algorithms to reduce I/O saturation", Source: "arxiv", Category: "cs.OS", From: "2024-01-01", To: "2024-12-31", Limit: 1}
	result, err := d.search(query)
	if err != nil || len(result.Documents) != 1 || result.Documents[0].ID != "arxiv:2401.10001" {
		t.Fatalf("fallback: %+v %v", result, err)
	}
	if result.Adjustment == nil || result.Adjustment.MinimumMatch != 2 || result.ModelCalls != 0 || strings.Contains(strings.Join(result.Adjustment.Terms, " "), " to ") {
		t.Fatalf("disclosure: %+v", result)
	}
	query.From = "2025-01-01"
	query.To = "2025-12-31"
	result, err = d.search(query)
	if err != nil || len(result.Documents) != 0 {
		t.Fatalf("date filter: %+v %v", result, err)
	}
}

func TestFallbackNormalizesIOAndRetainsStrictResults(t *testing.T) {
	d := testDB(t)
	fixture := `{"id":"2401.10001","title":"Storage","abstract":"Input output scheduling controls saturation."}`
	if _, err := importCatalog(d, "arxiv", "jsonl", strings.NewReader(fixture), 0); err != nil {
		t.Fatal(err)
	}
	result, err := d.search(SearchRequest{Query: "how to control I/O saturation"})
	if err != nil || len(result.Documents) != 1 || result.Adjustment == nil || result.Adjustment.MinimumMatch != 3 {
		t.Fatalf("normalized: %+v %v", result, err)
	}
	result, err = d.search(SearchRequest{Query: "scheduling saturation"})
	if err != nil || len(result.Documents) != 1 || result.Adjustment != nil || result.Method != "abstract-bm25" {
		t.Fatalf("strict results changed: %+v %v", result, err)
	}
	for _, q := range []SearchRequest{{Query: "2401.10001", Source: "gutenberg"}, {Query: "2401.99999"}, {Query: "10.1234/missing"}, {Query: "how to find"}, {Query: "unrelated nonexistent"}} {
		result, err = d.search(q)
		if err != nil || len(result.Documents) != 0 {
			t.Fatalf("unrelated/identifier: %+v %+v %v", q, result, err)
		}
	}
}

func TestBroadSearchDropsFillerAndNeverAcceptsQuerySyntax(t *testing.T) {
	d := testDB(t)
	loadFixture(t, d)
	for _, q := range []string{"how to find zebras", "how to find"} {
		result, err := d.search(SearchRequest{Query: q, Broad: true})
		if err != nil || len(result.Documents) != 0 {
			t.Fatalf("filler: %+v %v", result, err)
		}
	}
	result, err := d.search(SearchRequest{Query: `unmatched " OR attention NEAR zebras`, Broad: true})
	if err != nil || len(result.Documents) != 1 || result.Documents[0].ID != "arxiv:1706.03762" {
		t.Fatalf("literal query: %+v %v", result, err)
	}
}
