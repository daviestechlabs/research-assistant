package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPhysicalChoicesRetainLongOCRConditionsAndQualifications(t *testing.T) {
	condition := "If a reader receives an interrupt while m a i n t a i n i n g focus on a task, the reader must check the recorded interruption level before deciding whether focus ends."
	qualification := "The required level is ten or half the measured interruption, whichever value is higher."
	ending := "Focus also ends when the reader becomes unable to respond or deliberately switches to another focused task."
	text := strings.Repeat("This unrelated background sentence describes a different practice. ", 12) + "• Interruption. " + condition + " " + qualification + " " + ending
	page := PDFPageText{3, text}
	paper := PaperText{ID: "dnd:players-handbook"}
	choices := readingEvidenceChoices(paper, page, text, "What maintains focus and what ends it?", 12)
	combined := strings.Join(choices, "\n")
	if !strings.Contains(combined, condition) || !strings.Contains(combined, qualification) || !strings.Contains(combined, ending) || !strings.Contains(combined, "• Interruption.") {
		t.Fatalf("conditions or original OCR lost: %v", choices)
	}
	bytes := 0
	for _, quote := range choices {
		bytes += len(quote)
		if _, err := verifyPhysicalQuote(text, quote); err != nil {
			t.Fatal(err)
		}
	}
	if len(choices) > 12 || bytes > maxReadingChoiceBytes {
		t.Fatal("evidence choices exceed their budget")
	}
	if _, err := verifyEvidenceQuote(text, condition); err == nil {
		t.Fatal("abstract quote limit expanded")
	}
}

func TestPhysicalChoicesRankLateEvidenceAndSkipTruncatedSentences(t *testing.T) {
	background := "A different sentence describes unrelated background topics. "
	rule := "The instrument maintains calibration when its measured temperature stays within the specified operating range."
	page := PDFPageText{1, strings.Repeat(background, 15) + rule + " An unfinished qualification says unless"}
	choices := readingEvidenceChoices(PaperText{ID: "arxiv:0704.0001"}, page, page.Text, "When does the instrument maintain calibration?", 2)
	if len(choices) == 0 || !strings.Contains(choices[0], rule) {
		t.Fatalf("late relevant evidence lost: %v", choices)
	}
	for _, quote := range choices {
		if strings.Contains(quote, "unfinished qualification") {
			t.Fatal("unfinished tail presented as complete evidence")
		}
	}
	page = PDFPageText{1, "A source sentence starts with context that is outside the window and continues here. The instrument maintains calibration within the recorded range."}
	excerpt := "continues here. The instrument maintains calibration within the recorded range."
	for _, quote := range readingEvidenceChoices(PaperText{}, page, excerpt, "calibration", 12) {
		if strings.HasPrefix(quote, "continues here") {
			t.Fatal("truncated first sentence retained")
		}
	}
	if readingEvidenceChoices(PaperText{}, page, "Invented text outside the physical page.", "calibration", 12) != nil || readingEvidenceChoices(PaperText{}, page, page.Text, "calibration", 0) != nil {
		t.Fatal("invalid page window or limit accepted")
	}
}

func TestLongPhysicalCitationsRequireSelectedChoiceAndExactPage(t *testing.T) {
	p := readingFixture()
	quote := "The observations support a conditional relationship between erosion and changes in river channels when the measured flow stays inside the surveyed range, while leaving causal direction and other environments unresolved."
	p.Papers[0].Pages[1].Text = quote
	p.Sources[0].Pages[1].Text = quote
	p.Sources[0].Pages[1].Choices = []string{quote}
	result, err := validateReadingAnswer(readingContent(2, quote), "explain", p)
	if err != nil || result.Statements[0].Citations[0].Page != 2 || !strings.HasSuffix(result.Statements[0].Citations[0].PDFURL, "#page=2") {
		t.Fatalf("long citation binding failed: %v", err)
	}
	if _, err = validateReadingAnswer(readingContent(1, quote), "explain", p); err == nil {
		t.Fatal("long quote accepted on wrong page")
	}
	p.Sources[0].Pages[1].Choices = []string{p.Sources[0].Pages[0].Choices[0]}
	if _, err = validateReadingAnswer(readingContent(2, quote), "explain", p); err == nil {
		t.Fatal("unselected long excerpt accepted")
	}
	if _, err = verifyPhysicalQuote(strings.Repeat("word ", 81), strings.Repeat("word ", 81)); err == nil {
		t.Fatal("physical quote bound bypassed")
	}
	if _, err = verifyPhysicalQuote(quote, "Invented exact quotation outside the physical page."); err == nil {
		t.Fatal("fabricated physical quote accepted")
	}
}

func TestOfflinePageEvidenceInspectionBoundsAndSourceScope(t *testing.T) {
	record := map[string]any{"id": "arxiv:0704.0001", "version": 1, "sha256": strings.Repeat("a", 64), "extraction": "fixture", "page": PDFPageText{1, "A calibrated instrument measures changes in rock structures."}}
	raw, _ := json.Marshal(record)
	r, err := inspectPageEvidence(strings.NewReader(string(raw)), "rock structures")
	if err != nil || r["model_calls"] != 0 || r["physical_page"] != 1 || !strings.Contains(r["scope"].(string), "not a new extraction") {
		t.Fatalf("offline check: %v", err)
	}
	for _, bad := range []string{string(raw) + ` {}`, strings.Repeat("x", 65537), strings.Replace(string(raw), `"page":1`, `"page":0`, 1), strings.Replace(string(raw), `"version":1`, `"version":0`, 1)} {
		if _, err := inspectPageEvidence(strings.NewReader(bad), "rock structures"); err == nil {
			t.Fatal("invalid cached page metadata accepted")
		}
	}
}
