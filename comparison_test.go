package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func comparisonFixture(t *testing.T) preparedComparison {
	t.Helper()
	d := testDB(t)
	loadFixture(t, d)
	docs, err := d.getDocuments([]string{"arxiv:1706.03762", "arxiv:2005.11401"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := abstractComparison(docs)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func comparisonQuote(p preparedComparison, n int) pageQuote {
	source := p.Sources[n]
	return pageQuote{ID: source.ID, Page: source.Pages[0].Page, Quote: source.Pages[0].Choices[0]}
}

func comparisonFixtureWire(p preparedComparison) comparisonWire {
	q1, q2 := comparisonQuote(p, 0), comparisonQuote(p, 1)
	return comparisonWire{Sections: map[string]comparisonWireSection{
		"agreements":    {Status: "supported", Findings: []comparisonWireFinding{{Kind: "inference", Text: "Both passages describe components for generating language, although they do not establish equivalent results.", Citations: []pageQuote{q1, q2}}}},
		"disagreements": {Status: "insufficient", Findings: []comparisonWireFinding{}},
		"methods":       {Status: "supported", Findings: []comparisonWireFinding{{Kind: "reported", Citations: []pageQuote{q1}}, {Kind: "reported", Citations: []pageQuote{q2}}}},
		"limits":        {Status: "supported", Findings: []comparisonWireFinding{{Kind: "inference", Text: "The excerpts do not establish performance on matched datasets and metrics.", Citations: []pageQuote{q1, q2}}}},
		"open_questions": {Status: "supported", Findings: []comparisonWireFinding{{Kind: "proposal", Citations: []pageQuote{q1, q2}, Hypothesis: &hypothesisPlan{
			Mechanism: "Test a speculative combination of attention with retrieved context.", Intervention: "Vary the amount of retrieved context.",
			Baseline: "Use the same generator and prompts without that context.", Measurement: "Measure factual accuracy on a held-out dataset.",
			FailureCondition: "The combination does not improve accuracy under matched settings.", ExistingOverlap: "Retrieval with a generator already appears in the supplied evidence.", Assumptions: "Check architecture compatibility and existing attention components in the full papers.",
		}}}},
	}}
}

func encodeComparison(t *testing.T, wire comparisonWire) string {
	t.Helper()
	b, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func comparisonModelReply(t *testing.T, request map[string]any, content string) string {
	t.Helper()
	format := request["response_format"].(map[string]any)["json_schema"].(map[string]any)
	if format["name"] != "comparison_evidence_review" {
		return content
	}
	var wire comparisonWire
	if err := json.Unmarshal([]byte(content), &wire); err != nil {
		t.Fatal(err)
	}
	decisions := []ComparisonReview{}
	for _, category := range comparisonCategories {
		for n := range wire.Sections[category].Findings {
			decisions = append(decisions, ComparisonReview{category, n, "supported", "direct-evidence"})
		}
	}
	data, _ := json.Marshal(map[string]any{"decisions": decisions})
	return string(data)
}

func TestComparisonEvidenceKindsAndAbstention(t *testing.T) {
	p := comparisonFixture(t)
	result, err := validateComparison(encodeComparison(t, comparisonFixtureWire(p)), p)
	if err != nil || result.Comparison == nil || len(result.Comparison.Sections) != 5 {
		t.Fatalf("structured comparison: %+v %v", result, err)
	}
	for n, category := range comparisonCategories {
		if result.Comparison.Sections[n].Category != category {
			t.Fatal("comparison category order changed")
		}
	}
	if result.Comparison.Sections[1].Status != "insufficient" || len(result.Comparison.Sections[1].Findings) != 0 || !strings.Contains(result.Comparison.Sections[1].Note, "full papers") {
		t.Fatal("unknown disagreement was replaced by a finding")
	}
	method := result.Comparison.Sections[2].Findings[0]
	if method.Kind != "reported" || method.Text != "“"+comparisonQuote(p, 0).Quote+"”" || method.Citations[0].Page != 0 || method.Citations[0].Scope != "abstract" || method.Citations[0].SourceFingerprint != p.Documents[0].Fingerprint {
		t.Fatalf("reported evidence was not source-bound: %+v", method)
	}
	proposal := result.Comparison.Sections[4].Findings[0]
	if proposal.Kind != "proposal" || proposal.Hypothesis == nil || !strings.Contains(proposal.Text, "Failure condition") || !strings.Contains(result.Comparison.Limits, "novelty is not established") {
		t.Fatal("proposal lost its test plan or evidence limits")
	}
	unknown := comparisonWire{Sections: map[string]comparisonWireSection{}}
	for _, category := range comparisonCategories {
		unknown.Sections[category] = comparisonWireSection{Status: "insufficient", Findings: []comparisonWireFinding{}}
	}
	result, err = validateComparison(encodeComparison(t, unknown), p)
	if err != nil || !strings.Contains(result.Answer, "Insufficient evidence") || len(result.Sources) != 2 {
		t.Fatalf("complete abstention was rejected: %+v %v", result, err)
	}
}

func TestComparisonRejectsUnsupportedStructureAndCitations(t *testing.T) {
	p := comparisonFixture(t)
	for name, change := range map[string]func(*comparisonWire){
		"fabricated excerpt": func(w *comparisonWire) {
			s := w.Sections["methods"]
			s.Findings[0].Citations[0].Quote = "This invented quotation proves a universal scientific claim."
			w.Sections["methods"] = s
		},
		"wrong physical page": func(w *comparisonWire) {
			s := w.Sections["methods"]
			s.Findings[0].Citations[0].Page = 2
			w.Sections["methods"] = s
		},
		"unselected source": func(w *comparisonWire) {
			s := w.Sections["methods"]
			s.Findings[0].Citations[0].ID = "arxiv:9999.99999"
			w.Sections["methods"] = s
		},
		"reported paraphrase": func(w *comparisonWire) {
			s := w.Sections["methods"]
			s.Findings[0].Text = "The model invents a reported claim."
			w.Sections["methods"] = s
		},
		"one-source agreement": func(w *comparisonWire) {
			s := w.Sections["agreements"]
			s.Findings[0].Citations = s.Findings[0].Citations[:1]
			w.Sections["agreements"] = s
		},
		"duplicate citation": func(w *comparisonWire) {
			s := w.Sections["agreements"]
			s.Findings[0].Citations[1] = s.Findings[0].Citations[0]
			w.Sections["agreements"] = s
		},
		"uncited inference": func(w *comparisonWire) {
			s := w.Sections["limits"]
			s.Findings[0].Citations = nil
			w.Sections["limits"] = s
		},
		"unselected prose citation": func(w *comparisonWire) {
			s := w.Sections["limits"]
			s.Findings[0].Text += " [arxiv:9999.99999]"
			w.Sections["limits"] = s
		},
		"missing section": func(w *comparisonWire) { delete(w.Sections, "limits") },
		"unknown section": func(w *comparisonWire) { w.Sections["invented"] = comparisonWireSection{} },
		"claim under insufficient": func(w *comparisonWire) {
			s := w.Sections["agreements"]
			s.Status = "insufficient"
			w.Sections["agreements"] = s
		},
		"empty supported section": func(w *comparisonWire) { w.Sections["disagreements"] = comparisonWireSection{Status: "supported"} },
		"unknown evidence kind": func(w *comparisonWire) {
			s := w.Sections["limits"]
			s.Findings[0].Kind = "established"
			w.Sections["limits"] = s
		},
		"experiment without a baseline": func(w *comparisonWire) {
			s := w.Sections["open_questions"]
			s.Findings[0].Hypothesis.Baseline = ""
			w.Sections["open_questions"] = s
		},
		"proposal posing as disagreement": func(w *comparisonWire) { w.Sections["disagreements"] = w.Sections["open_questions"] },
	} {
		t.Run(name, func(t *testing.T) {
			wire := comparisonFixtureWire(p)
			change(&wire)
			if _, err := validateComparison(encodeComparison(t, wire), p); err == nil {
				t.Fatal("invalid comparison accepted")
			}
		})
	}
	valid := encodeComparison(t, comparisonFixtureWire(p))
	for _, bad := range []string{valid + " {}", strings.Replace(valid, `"sections":`, `"extra":true,"sections":`, 1)} {
		if _, err := validateComparison(bad, p); err == nil {
			t.Fatal("extra response content accepted")
		}
	}
}

func twoPaperReadingFixture() preparedReading {
	p := readingFixture()
	other := readingFixture()
	other.Documents[0].ID, other.Documents[0].Title = "arxiv:0704.0002", "Different river survey"
	other.Papers[0].ID, other.Papers[0].SHA256, other.Papers[0].Version = "arxiv:0704.0002", strings.Repeat("b", 64), 4
	other.Sources[0].ID, other.Sources[0].Title, other.Sources[0].Version = "arxiv:0704.0002", "Different river survey", 4
	p.Documents = append(p.Documents, other.Documents...)
	p.Papers = append(p.Papers, other.Papers...)
	p.Sources = append(p.Sources, other.Sources...)
	return p
}

func TestComparisonPhysicalPageReceipts(t *testing.T) {
	reading := twoPaperReadingFixture()
	p, err := pageComparison(reading)
	if err != nil {
		t.Fatal(err)
	}
	result, err := validateComparison(encodeComparison(t, comparisonFixtureWire(p)), p)
	if err != nil {
		t.Fatal(err)
	}
	for n, finding := range result.Comparison.Sections[2].Findings {
		c := finding.Citations[0]
		if c.Scope != "pdf-physical-page" || c.Page != 1 || c.SHA256 != reading.Papers[n].SHA256 || c.Version != reading.Papers[n].Version || !strings.HasSuffix(c.PDFURL, "#page=1") || !strings.Contains(c.PDFURL, "sha256="+c.SHA256) {
			t.Fatalf("physical evidence was not pinned: %+v", c)
		}
	}
	q := comparisonQuote(p, 0)
	q.Page = 3
	if _, err := p.citation(q); err == nil {
		t.Fatal("an unreadable, unconsulted page accepted a citation")
	}
}

func TestComparisonDurableJobCacheAndBrowserResponse(t *testing.T) {
	s, i := deskFixture(t)
	docs, err := i.db.getDocuments([]string{"arxiv:1706.03762", "arxiv:2005.11401"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := abstractComparison(docs)
	if err != nil {
		t.Fatal(err)
	}
	wire := encodeComparison(t, comparisonFixtureWire(p))
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		encoded, _ := json.Marshal(request)
		if strings.Contains(string(encoded), "nas_ref") || strings.Contains(string(encoded), "s3://") || (request["max_completion_tokens"] != float64(2800) && request["max_completion_tokens"] != float64(1200)) {
			t.Error("model input exposed storage or omitted its output bound")
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": comparisonModelReply(t, request, wire)}}}})
	}))
	defer model.Close()
	i.answerURL, i.model = model.URL, "fixture"
	data := sessionFixture()
	data.Task, data.SelectedIDs = "compare", []string{docs[0].ID, docs[1].ID}
	x, err := s.save("owner", "", 0, data)
	if err != nil {
		t.Fatal(err)
	}
	lastID := ""
	for n := 0; n < 2; n++ {
		j, err := s.submit("owner", x.ID, x.Revision, "answer")
		if err != nil {
			t.Fatal(err)
		}
		j = awaitDeskJob(t, s, "owner", j.ID)
		lastID = j.ID
		if j.Status != "complete" || j.Cached != (n == 1) || calls.Load() != 2 {
			t.Fatalf("comparison job: %+v calls=%d", j, calls.Load())
		}
		var result AnswerResponse
		if json.Unmarshal(j.Result, &result) != nil || result.Comparison == nil || strings.Contains(string(j.Result), "nas_ref") || strings.Contains(string(j.Result), "s3://") {
			t.Fatal("saved browser response lost comparison or leaked storage")
		}
	}
	stats, err := i.stats()
	if err != nil || stats.(map[string]any)["model_calls_today"].(int) != 2 {
		t.Fatalf("cached comparison reserved usage: %+v %v", stats, err)
	}
	// A second database connection sees the durable structured result.
	copy, err := openDatabase(s.db.path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer copy.close()
	rows, err := copy.rows("SELECT result FROM desk_jobs WHERE status='complete' ORDER BY updated DESC LIMIT 1")
	if err != nil || len(rows) != 1 || !strings.Contains(rows[0][0], `"open_questions"`) {
		t.Fatalf("durable comparison missing: %v %v", rows, err)
	}
	if _, err := s.job("different-owner", lastID); err == nil {
		t.Fatal("another owner accessed the comparison")
	}
	data.SelectedIDs = data.SelectedIDs[:1]
	y, err := s.save("owner", "", 0, data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.submit("owner", y.ID, y.Revision, "answer"); err == nil || calls.Load() != 2 {
		t.Fatal("single-source comparison reached the model")
	}
}

func TestOperatorComparisonCheckUsesFreshDesk(t *testing.T) {
	p := comparisonFixture(t)
	d := testDB(t)
	loadFixture(t, d)
	i := testInference(t, d)
	content := encodeComparison(t, comparisonFixtureWire(p))
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": comparisonModelReply(t, request, content)}}}})
	}))
	defer model.Close()
	i.answerURL, i.model = model.URL, "fixture"
	path := filepath.Join(t.TempDir(), "research-check.sqlite")
	data := SessionData{Question: "Compare these methods", Task: "compare", SelectedIDs: []string{p.Documents[0].ID, p.Documents[1].ID}}
	result, err := checkResearchJobs(context.Background(), path, i, nil, data)
	if err != nil || calls.Load() != 2 || result.(map[string]any)["repeat_cached"] != true {
		t.Fatalf("operator proof: %+v %v calls=%d", result, err, calls.Load())
	}
	if _, err := checkResearchJobs(context.Background(), path, i, nil, data); err == nil || calls.Load() != 2 {
		t.Fatal("operator verification reopened an existing worker database")
	}
}

func TestPDFComparisonUsesSharedStructuredValidator(t *testing.T) {
	d := testDB(t)
	i := testInference(t, d)
	reading := twoPaperReadingFixture()
	p, err := pageComparison(reading)
	if err != nil {
		t.Fatal(err)
	}
	content := encodeComparison(t, comparisonFixtureWire(p))
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": comparisonModelReply(t, request, content)}}}})
	}))
	defer model.Close()
	i.answerURL, i.model = model.URL, "fixture"
	result, err := i.readingAnswer(context.Background(), SessionData{Question: "Compare these river surveys", Task: "compare"}, reading)
	if err != nil || result.Comparison == nil || len(result.Papers) != 2 || result.Papers[1].SHA256 != reading.Papers[1].SHA256 || result.ModelCalls != 2 || result.EvidenceScope != "consulted-saved-pdf-page-excerpts" {
		t.Fatalf("PDF comparison: %+v %v", result, err)
	}
}

func TestComparisonReviewOmitsMethodDifferencesAndGuarantees(t *testing.T) {
	p := comparisonFixture(t)
	wire := comparisonFixtureWire(p)
	wire.Sections["disagreements"] = comparisonWireSection{Status: "supported", Findings: []comparisonWireFinding{{Kind: "inference", Text: "The methods use different approaches, so they disagree.", Citations: []pageQuote{comparisonQuote(p, 0), comparisonQuote(p, 1)}}}}
	wire.Sections["limits"] = comparisonWireSection{Status: "supported", Findings: []comparisonWireFinding{{Kind: "inference", Text: "The penalty guarantees the outcome under all conditions.", Citations: []pageQuote{comparisonQuote(p, 0)}}}}
	result, err := validateComparison(encodeComparison(t, wire), p)
	if err != nil {
		t.Fatal(err)
	}
	reviews := []ComparisonReview{}
	for _, section := range result.Comparison.Sections {
		for n := range section.Findings {
			review := ComparisonReview{section.Category, n, "supported", "direct-evidence"}
			if section.Category == "disagreements" {
				review.Verdict, review.Reason = "insufficient", "different-methods"
			}
			if section.Category == "limits" {
				review.Verdict, review.Reason = "insufficient", "overstated-certainty"
			}
			reviews = append(reviews, review)
		}
	}
	for _, bad := range [][]ComparisonReview{reviews[:len(reviews)-1], append(append([]ComparisonReview{}, reviews...), reviews[0])} {
		if err := applyComparisonReview(&result, bad); err == nil {
			t.Fatal("incomplete or duplicate review accepted")
		}
	}
	if err := applyComparisonReview(&result, reviews); err != nil {
		t.Fatal(err)
	}
	if result.Comparison.Sections[1].Status != "insufficient" || len(result.Comparison.Sections[1].Findings) != 0 || strings.Contains(result.Answer, "so they disagree") || len(result.Comparison.Sections[3].Findings) != 0 {
		t.Fatal("review rejection left an unsupported comparison finding")
	}
}

func TestEvidenceChoicesKeepMethodsAndRejectBrokenReferences(t *testing.T) {
	text := "This opening sentence provides a useful general background. Our implementation combines multiple survey channels using learned superpixels. The methodology follows in three parts. This broken statement ends inside an equation reference (Eq. 4) and continues here. The regularization penalty promotes connected components without guaranteeing them."
	choices := completeEvidenceChoices(text, 8)
	found := false
	for _, quote := range choices {
		if quote == "Our implementation combines multiple survey channels using learned superpixels." {
			found = true
		}
		if strings.Contains(quote, "methodology follows") || strings.HasSuffix(quote, "(Eq.") || strings.Count(quote, "(") != strings.Count(quote, ")") {
			t.Fatal("a poor or unbalanced excerpt survived")
		}
		if _, err := verifyEvidenceQuote(text, quote); err != nil {
			t.Fatal("excerpt selection rewrote source text")
		}
	}
	if !found {
		t.Fatal("method sentence was lost by sparse sampling")
	}
}
