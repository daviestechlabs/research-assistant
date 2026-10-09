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

func roleReviewFixture(t *testing.T) GraphResponse {
	t.Helper()
	// Real saved abstract evidence from arxiv:2110.14440. Expected judgments
	// are manual development labels, not evidence of the live model's accuracy.
	quote := "Then, forward modeling is used to compute the airborne magnetic responses of the synthetic models, which are associated with their counterpart surficial lithologies."
	evidence := []evidenceQuote{{"arxiv:2110.14440", quote}}
	docs := []Document{{ID: evidence[0].ID, Title: "Geological mapping", Summary: quote}}
	wire := graphWire{Concepts: []graphConcept{{"synthetic models", evidence}, {"airborne magnetic responses", evidence}, {"forward modeling", evidence}}, Relations: []graphRelation{
		{From: "synthetic models", To: "airborne magnetic responses", Label: "computes", Kind: "reported", Evidence: evidence},
		{From: "forward modeling", To: "airborne magnetic responses", Label: "computes", Kind: "reported", Evidence: evidence},
	}}
	result, err := validateGraph(wire, GraphRequest{Query: "geological mapping"}, docs)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestGraphReviewAppliesBoundDecisionsWithoutRewritingCandidates(t *testing.T) {
	result := roleReviewFixture(t)
	before := graphReviewCandidates(&result)
	decisions := []graphReviewDecision{{before[1].EdgeID, "supported", "direct-evidence"}, {before[0].EdgeID, "insufficient", "wrong-roles"}}
	if err := applyGraphRelationReview(&result, decisions); err != nil {
		t.Fatal(err)
	}
	if len(result.Edges) != 4 || result.Edges[3].From != "concept:3" || result.OmittedItems != 1 || len(result.RelationReview) != 2 || result.RelationReview[0].Verdict != "insufficient" {
		t.Fatalf("incorrect filtering: %+v", result)
	}
	for n, review := range result.RelationReview {
		a, _ := json.Marshal(review.Edge)
		b, _ := json.Marshal(before[n].Edge)
		if string(a) != string(b) || review.FromLabel != before[n].FromLabel || review.ToLabel != before[n].ToLabel {
			t.Fatal("review rewrote the original candidate or evidence")
		}
	}
}

func TestGraphReviewRequiresCompleteConsistentDecisionsBeforeMutation(t *testing.T) {
	good := []graphReviewDecision{{"edge:4", "insufficient", "wrong-roles"}, {"edge:5", "supported", "direct-evidence"}}
	for _, tc := range []struct {
		name      string
		decisions []graphReviewDecision
	}{
		{"missing", good[:1]},
		{"duplicate", []graphReviewDecision{good[0], good[0]}},
		{"mention", []graphReviewDecision{{"edge:1", "supported", "direct-evidence"}, good[1]}},
		{"unknown reason", []graphReviewDecision{{"edge:4", "insufficient", "invented"}, good[1]}},
		{"negative supported", []graphReviewDecision{{"edge:4", "supported", "wrong-roles"}, good[1]}},
		{"positive rejected", []graphReviewDecision{{"edge:4", "insufficient", "direct-evidence"}, good[1]}},
		{"hypothesis verdict for reported", []graphReviewDecision{{"edge:4", "supported", "qualified-test-plan"}, good[1]}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := roleReviewFixture(t)
			before, _ := json.Marshal(result)
			if err := applyGraphRelationReview(&result, tc.decisions); err == nil {
				t.Fatal("invalid decision accepted")
			}
			after, _ := json.Marshal(result)
			if string(before) != string(after) {
				t.Fatal("invalid review partially changed graph")
			}
		})
	}
}

func TestGraphReviewKeepsSpeculativeTestPlanDistinctFromReportedEvidence(t *testing.T) {
	docs, wire := graphFixture()
	wire.Relations = append(wire.Relations, graphRelation{From: "attention", To: "Retrieval", Label: "could guide", Kind: "hypothesis", Evidence: []evidenceQuote{{docs[0].ID, docs[0].Summary}, {docs[1].ID, docs[1].Summary}}, Rationale: "Attention might help retrieval if the contexts and scales align.", Test: "Compare recall against a matched retrieval baseline; reject if recall does not improve."})
	result, err := validateGraph(wire, GraphRequest{Query: "retrieval", IncludeHypotheses: true}, docs)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(result)
	if err := applyGraphRelationReview(&result, []graphReviewDecision{{"edge:4", "supported", "direct-evidence"}, {"edge:5", "supported", "direct-evidence"}}); err == nil {
		t.Fatal("hypothesis accepted as reported evidence")
	}
	after, _ := json.Marshal(result)
	if string(before) != string(after) {
		t.Fatal("invalid hypothesis verdict changed graph")
	}
	if err := applyGraphRelationReview(&result, []graphReviewDecision{{"edge:4", "supported", "direct-evidence"}, {"edge:5", "supported", "qualified-test-plan"}}); err != nil || result.Edges[4].Kind != "hypothesis" || result.Edges[4].Test == "" {
		t.Fatal("qualified hypothesis lost its speculative kind or test")
	}
}

func TestGraphReviewRequestBoundsRedactionAndProtocol(t *testing.T) {
	i := testInference(t, testDB(t))
	response := `{"decisions":[{"edge_id":"edge:4","verdict":"insufficient","reason":"wrong-roles"},{"edge_id":"edge:5","verdict":"supported","reason":"direct-evidence"}]}`
	calls := 0
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		serialized, _ := json.Marshal(body)
		if strings.Contains(string(serialized), "s3://") || strings.Contains(string(serialized), "private-key") || strings.Contains(string(serialized), "whole-source-title") || strings.Contains(string(serialized), "nas_ref") {
			t.Error("non-evidence metadata reached reviewer")
		}
		if body["max_completion_tokens"] != float64(1200) || body["temperature"] != float64(0) || body["include_reasoning"] != false {
			t.Error("review request not bounded")
		}
		if !strings.Contains(string(serialized), "from_label") || !strings.Contains(string(serialized), "synthetic models") || !strings.Contains(string(serialized), "forward modeling") {
			t.Error("review lacks endpoint roles")
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": response}}}})
	}))
	defer model.Close()
	i.answerURL, i.model = model.URL, "fixture"
	result := roleReviewFixture(t)
	result.Sources[0].NASRef, result.Sources[0].Title = "s3://private-key", "whole-source-title"
	if err := i.reviewGraphRelations(context.Background(), &result); err != nil || result.ModelCalls != 1 || result.ReservedUnits <= 1200 || calls != 1 {
		t.Fatalf("review: %+v %v", result, err)
	}
	for _, bad := range []string{response + ` {}`, strings.Replace(response, `"reason":"wrong-roles"`, `"reason":"wrong-roles","rewrite":"new actor"`, 1), `{"decisions":[]}`} {
		response = bad
		candidate := roleReviewFixture(t)
		if err := i.reviewGraphRelations(context.Background(), &candidate); err == nil || len(candidate.Edges) != 5 || candidate.ModelCalls != 0 {
			t.Fatal("invalid reviewer response accepted or partially applied")
		}
	}
	mentions := roleReviewFixture(t)
	mentions.Edges = mentions.Edges[:3]
	before := calls
	if err := i.reviewGraphRelations(context.Background(), &mentions); err != nil || calls != before || mentions.ModelCalls != 0 {
		t.Fatal("mention-only graph spent review budget")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	candidate := roleReviewFixture(t)
	if err := i.reviewGraphRelations(ctx, &candidate); err == nil || calls != before {
		t.Fatal("cancelled review reached model")
	}
	i.dailyCalls = calls
	if err := i.reviewGraphRelations(context.Background(), &candidate); err == nil || calls != before {
		t.Fatal("review bypassed shared budget")
	}
}

func TestGraphJobStoresOnlyReviewedResultsAndReusesReview(t *testing.T) {
	s, i := deskFixture(t)
	var calls atomic.Int32
	var invalid atomic.Bool
	invalid.Store(true)
	_, wire := graphFixture()
	generated, _ := json.Marshal(wire)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		name := body["response_format"].(map[string]any)["json_schema"].(map[string]any)["name"]
		response := string(generated)
		if reviewed, ok := graphEvidenceFixtureContent(body); ok {
			response = reviewed
		}
		if name == "graph_relation_review" {
			response = `{"decisions":[{"edge_id":"edge:4","verdict":"supported","reason":"direct-evidence"}]}`
			if invalid.Load() {
				response += ` {}`
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": response}}}})
	}))
	defer model.Close()
	i.answerURL, i.model = model.URL, "fixture"
	data := sessionFixture()
	data.SelectedIDs = []string{"arxiv:1706.03762", "arxiv:2005.11401"}
	x, err := s.save("owner", "", 0, data)
	if err != nil {
		t.Fatal(err)
	}
	// A previously valid cache key lacks the new relation-review contract.
	docs, err := i.db.getDocuments(data.SelectedIDs)
	if err != nil {
		t.Fatal(err)
	}
	fingerprints := []string{}
	for _, doc := range docs {
		fingerprints = append(fingerprints, doc.ID+":"+doc.Fingerprint)
	}
	oldInput, _ := json.Marshal([]any{deskContract, "graph", i.answerURL, i.model, data.Question, data.Task, data.IncludeHypotheses, fingerprints})
	oldKey := textSHA(oldInput)
	key, err := s.cacheKey("graph", data)
	if err != nil || key == oldKey {
		t.Fatal("old graph cache remains eligible")
	}
	s.db.mu.Lock()
	_, err = s.db.run("INSERT INTO desk_cache(owner,key,updated,result) VALUES(?,?,?,?)", "owner", oldKey, deskTime(), `{"unreviewed":true}`)
	s.db.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.submit("owner", x.ID, x.Revision, "graph")
	if err != nil {
		t.Fatal(err)
	}
	failed := awaitDeskJob(t, s, "owner", job.ID)
	if failed.Status != "failed" || len(failed.Result) != 0 || calls.Load() != 3 {
		t.Fatalf("failed review persisted output: %+v", failed)
	}
	invalid.Store(false)
	job, err = s.submit("owner", x.ID, x.Revision, "graph")
	if err != nil {
		t.Fatal(err)
	}
	complete := awaitDeskJob(t, s, "owner", job.ID)
	var result GraphResponse
	json.Unmarshal(complete.Result, &result)
	if complete.Status != "complete" || result.ModelCalls != 3 || len(result.RelationReview) != 1 || calls.Load() != 6 {
		t.Fatalf("review not persisted: %+v", complete)
	}
	job, err = s.submit("owner", x.ID, x.Revision, "graph")
	if err != nil {
		t.Fatal(err)
	}
	repeat := awaitDeskJob(t, s, "owner", job.ID)
	if !repeat.Cached || calls.Load() != 6 || string(repeat.Result) != string(complete.Result) {
		t.Fatal("cached repeat lost review or made model calls")
	}
}
