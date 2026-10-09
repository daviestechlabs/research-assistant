package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// Existing orchestration fixtures need a distinct reply for the new review
// request. This mock establishes protocol behavior, never scientific truth.
func answerReviewFixtureContent(req map[string]any) (string, bool) {
	format, ok := req["response_format"].(map[string]any)
	if !ok {
		return "", false
	}
	schema, ok := format["json_schema"].(map[string]any)
	if !ok || schema["name"] != "answer_evidence_review" {
		return "", false
	}
	decisions := []AnswerEvidenceReview{}
	for _, kind := range []string{"reported", "interpretation", "limits"} {
		decisions = append(decisions, AnswerEvidenceReview{kind, "supported", "direct-evidence", "Protocol fixture only: source supports this paragraph."})
	}
	b, _ := json.Marshal(map[string]any{"decisions": namedAnswerReviewFixture(decisions)})
	return string(b), true
}

func namedAnswerReviewFixture(decisions []AnswerEvidenceReview) map[string]map[string]any {
	result := map[string]map[string]any{}
	for _, decision := range decisions {
		result[decision.Kind] = map[string]any{"assessment": decision.Assessment, "supported": decision.Verdict == "supported"}
	}
	return result
}

func graphEvidenceFixtureContent(req map[string]any) (string, bool) {
	format, ok := req["response_format"].(map[string]any)
	if !ok {
		return "", false
	}
	schema, ok := format["json_schema"].(map[string]any)
	if !ok || schema["name"] != "graph_evidence_review" {
		return "", false
	}
	var messages []struct {
		Content string `json:"content"`
	}
	b, _ := json.Marshal(req["messages"])
	if json.Unmarshal(b, &messages) != nil || len(messages) != 2 {
		return "", false
	}
	var inputs []struct {
		QuoteID string `json:"quote_id"`
	}
	if json.Unmarshal([]byte(messages[1].Content), &inputs) != nil {
		return "", false
	}
	decisions := []map[string]string{}
	for _, input := range inputs {
		decisions = append(decisions, map[string]string{"quote_id": input.QuoteID, "verdict": "supported", "reason": "source-content"})
	}
	b, _ = json.Marshal(map[string]any{"decisions": decisions})
	return string(b), true
}

func TestAnswerEvidenceReviewOmitsUnsafeClaimsAndRejectsIncompleteReview(t *testing.T) {
	for _, mode := range []string{"filter", "missing", "duplicate", "invalid-support", "missing-support", "null-support", "duplicate-support", "duplicate-slot", "unknown-field", "trailing", "missing-assessment", "http-failure"} {
		t.Run(mode, func(t *testing.T) {
			d := testDB(t)
			loadFixture(t, d)
			i := testInference(t, d)
			posts := 0
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				posts++
				var req map[string]any
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Fatal(err)
				}
				content := identityAnswerContent
				if _, review := answerReviewFixtureContent(req); review {
					if mode == "http-failure" {
						w.WriteHeader(503)
						return
					}
					decisions := []AnswerEvidenceReview{{"reported", "insufficient", "source-instruction", "Quote commands an assistant."}, {"interpretation", "insufficient", "overstated-certainty", "The guarantee exceeds the source."}, {"limits", "supported", "direct-evidence", "The unknown details are absent from this abstract."}}
					switch mode {
					case "missing":
						decisions = decisions[:2]
					case "duplicate":
						decisions[1] = decisions[0]
					case "missing-assessment":
						decisions[0].Assessment = ""
					}
					named := namedAnswerReviewFixture(decisions)
					switch mode {
					case "invalid-support":
						named["reported"]["supported"] = "yes"
					case "null-support":
						named["reported"]["supported"] = nil
					case "missing-support":
						delete(named["reported"], "supported")
					case "unknown-field":
						named["reported"]["verdict"] = "supported"
					}
					b, _ := json.Marshal(map[string]any{"decisions": named})
					content = string(b)
					if mode == "duplicate-support" {
						content = strings.Replace(content, `"supported":false`, `"supported":false,"supported":true`, 1)
					}
					if mode == "duplicate-slot" {
						content = strings.Replace(content, `"interpretation":`, `"reported":`, 1)
					}
					if mode == "trailing" {
						content += " {}"
					}
				} else {
					content = strings.Replace(content, "Attention changes the mechanism.", "This mechanism guarantees an outcome unsupported by the source.", 1)
				}
				writeJSON(w, 200, map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": content}}}})
			}))
			defer model.Close()
			i.answerURL = model.URL
			i.model = "fixture-model"
			result, err := i.answer(context.Background(), AnswerRequest{Question: "Explain the mechanism", IDs: []string{"arxiv:1706.03762"}, Task: "explain"})
			if posts != 2 || identityUsage(t, i) != 2 {
				t.Fatal("review did not preserve both started reservations")
			}
			if mode == "filter" {
				if err != nil || result.ModelCalls != 2 || len(result.EvidenceReview) != 3 || strings.Contains(result.Answer, "guarantees an outcome") || strings.Contains(result.Answer, "Verified source excerpts: “") || !strings.Contains(result.Answer, "abstract omits full evaluation details") {
					t.Fatal("unsafe claims survived or supported limits were discarded", result.Answer, err)
				}
			} else if err == nil || result.Answer != "" {
				t.Fatal("invalid review produced a public answer")
			}
		})
	}
}

func TestAnswerReviewPhaseAllowsCancellationBeforeAnotherRequest(t *testing.T) {
	d := testDB(t)
	loadFixture(t, d)
	i := testInference(t, d)
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(w, 200, map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": identityAnswerContent}}}})
	}))
	defer model.Close()
	i.answerURL, i.model = model.URL, "fixture"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	phases := []string{}
	result, err := i.answerWithProgress(ctx, AnswerRequest{Question: "Explain this mechanism", IDs: []string{"arxiv:1706.03762"}, Task: "explain"}, func(phase string) { phases = append(phases, phase); cancel() })
	if err == nil || result.Answer != "" || calls.Load() != 1 || identityUsage(t, i) != 1 || len(phases) != 1 || !strings.Contains(phases[0], "Reviewing answer claims") {
		t.Fatal("review phase lost cancellation or charged an unstarted request", err, phases)
	}
}
