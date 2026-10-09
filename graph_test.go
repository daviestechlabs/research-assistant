package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func graphFixture() ([]Document, graphWire) {
	a := "A transformer replaces recurrence with attention for machine translation."
	b := "Retrieval combines a generator with external knowledge from retrieved documents."
	docs := []Document{{ID: "arxiv:1706.03762", Title: "Attention", Summary: a}, {ID: "arxiv:2005.11401", Title: "Retrieval", Summary: b}}
	wire := graphWire{Concepts: []graphConcept{{"attention", []evidenceQuote{{docs[0].ID, a}}}, {"machine translation", []evidenceQuote{{docs[0].ID, a}}}, {"Retrieval", []evidenceQuote{{docs[1].ID, b}}}}, Relations: []graphRelation{{From: "attention", To: "machine translation", Label: "used for", Kind: "reported", Evidence: []evidenceQuote{{docs[0].ID, a}}}}}
	return docs, wire
}

func TestGraphValidatesProvenanceAndSpeculation(t *testing.T) {
	docs, wire := graphFixture()
	req := GraphRequest{Query: "language models", IDs: []string{docs[0].ID, docs[1].ID}}
	result, err := validateGraph(wire, req, docs)
	if err != nil || len(result.Nodes) != 5 || len(result.Edges) != 4 {
		t.Fatalf("%+v %v", result, err)
	}
	hypothesis := graphRelation{From: "attention", To: "Retrieval", Label: "could guide", Kind: "hypothesis", Evidence: []evidenceQuote{{docs[0].ID, docs[0].Summary}, {docs[1].ID, docs[1].Summary}}, Rationale: "Attention might help retrieval rank useful context.", Test: "Compare a matched retrieval baseline; reject if recall does not improve."}
	wire.Relations = append(wire.Relations, hypothesis)
	if _, err = validateGraph(wire, req, docs); err == nil {
		t.Fatal("speculation accepted without opt-in")
	}
	req.IncludeHypotheses = true
	result, err = validateGraph(wire, req, docs)
	if err != nil || result.Edges[4].Kind != "hypothesis" || result.Edges[4].Test == "" {
		t.Fatalf("%+v %v", result, err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*graphWire)
	}{
		{"unselected citation", func(w *graphWire) { w.Concepts[0].Evidence[0].ID = "arxiv:invented" }},
		{"invented quote", func(w *graphWire) {
			w.Concepts[0].Evidence[0].Quote = "Attention proves that the moon is green cheese."
		}},
		{"ungrounded concept", func(w *graphWire) { w.Concepts[0].Label = "causality" }},
		{"unknown endpoint", func(w *graphWire) { w.Relations[0].To = "invented" }},
		{"reported cross-source inference", func(w *graphWire) { w.Relations[0].To = "Retrieval" }},
		{"duplicate citation", func(w *graphWire) { w.Concepts[0].Evidence = append(w.Concepts[0].Evidence, w.Concepts[0].Evidence[0]) }},
		{"duplicate concept", func(w *graphWire) { w.Concepts = append(w.Concepts, w.Concepts[0]) }},
		{"missing test", func(w *graphWire) { w.Relations[1].Test = "" }},
		{"single-source speculation", func(w *graphWire) { w.Relations[1].Evidence = w.Relations[1].Evidence[:1] }},
		{"unrelated speculative quote", func(w *graphWire) { w.Relations[1].Evidence[1] = evidenceQuote{docs[0].ID, docs[0].Summary} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, _ := json.Marshal(wire)
			var candidate graphWire
			json.Unmarshal(data, &candidate)
			tc.mutate(&candidate)
			if _, err := validateGraph(candidate, req, docs); err == nil {
				t.Fatal("invalid graph accepted")
			}
		})
	}
	if graphTermOccurs("sedimentary rocks", "sediment") || !graphTermOccurs("Sedimentary rocks influence erosion.", "sedimentary rocks") {
		t.Fatal("concept word matching failed")
	}
}

func TestGraphModelContractBudgetAndBrowserRedaction(t *testing.T) {
	d := testDB(t)
	loadFixture(t, d)
	i := testInference(t, d)
	_, wire := graphFixture()
	data, _ := json.Marshal(wire)
	content := string(data)
	calls := 0
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		name := body["response_format"].(map[string]any)["json_schema"].(map[string]any)["name"]
		tokens := float64(1800)
		response := content
		if reviewed, ok := graphEvidenceFixtureContent(body); ok {
			tokens = 700
			response = reviewed
		}
		if name == "graph_relation_review" {
			tokens = 1200
			response = `{"decisions":[{"edge_id":"edge:4","verdict":"supported","reason":"direct-evidence"}]}`
		}
		if body["max_completion_tokens"] != tokens || body["response_format"].(map[string]any)["type"] != "json_schema" || body["include_reasoning"] != false {
			t.Error("missing bounded structured model request")
		}
		encoded, _ := json.Marshal(body)
		if strings.Contains(string(encoded), "s3://") || strings.Contains(string(encoded), "paper.pdf") {
			t.Error("NAS reference sent to model")
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": response}}}})
	}))
	defer model.Close()
	i.answerURL = model.URL
	i.model = "fixture"
	req := GraphRequest{Query: "language models", IDs: []string{"arxiv:1706.03762", "arxiv:2005.11401"}}
	h := handler(d, i, "127.0.0.1:8094")
	body, _ := json.Marshal(req)
	request := httptest.NewRequest("POST", "http://127.0.0.1:8094/api/graph", strings.NewReader(string(body)))
	request.Header.Set("Origin", "http://127.0.0.1:8094")
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	if recorder.Code != 200 || calls != 3 || strings.Contains(recorder.Body.String(), "s3://") || strings.Contains(recorder.Body.String(), "nas_ref") {
		t.Fatalf("browser graph: %d %s", recorder.Code, recorder.Body.String())
	}
	var result GraphResponse
	json.Unmarshal(recorder.Body.Bytes(), &result)
	if result.ModelCalls != 3 || result.ReservedUnits <= 3000 || result.GeneratedAt == "" || len(result.RelationReview) != 1 || len(result.EvidenceReview) == 0 {
		t.Fatal("usage receipt missing")
	}
	// A valid graph prefix followed by an extra object must never be accepted.
	content += ` {}`
	if _, err := i.graph(context.Background(), req); err == nil || calls != 4 {
		t.Fatal("trailing content accepted")
	}
	bad := req
	bad.IDs = []string{"arxiv:invented"}
	if _, err := i.graph(context.Background(), bad); err == nil || calls != 4 {
		t.Fatal("invalid selection reached model")
	}
	i.dailyCalls = 4
	if _, err := i.graph(context.Background(), req); err == nil || calls != 4 {
		t.Fatal("graph bypassed shared daily budget")
	}
	restarted := testInference(t, d)
	restarted.dailyCalls = 4
	if err := restarted.reserve("graph", 1); err == nil {
		t.Fatal("graph budget did not survive restart")
	}
	for _, origin := range []string{"", "https://other.example"} {
		request = httptest.NewRequest("POST", "http://127.0.0.1:8094/api/graph", strings.NewReader(string(body)))
		request.Header.Set("Origin", origin)
		recorder = httptest.NewRecorder()
		h.ServeHTTP(recorder, request)
		if recorder.Code != 403 || calls != 4 {
			t.Fatal("cross-origin graph reached model")
		}
	}
}

func TestGraphOmitsUnsupportedModelItemsWithoutInventingLinks(t *testing.T) {
	docs, wire := graphFixture()
	req := GraphRequest{Query: "language models"}
	wire.Concepts[2].Label = "unattested concept"
	result, err := filterGraph(wire, req, docs)
	if err != nil || result.OmittedItems != 1 || len(result.UnmappedSources) != 1 || result.UnmappedSources[0] != docs[1].ID || len(result.Nodes) != 4 || len(result.Edges) != 3 {
		t.Fatalf("%+v %v", result, err)
	}
	if !strings.Contains(result.Limits, "standalone source nodes") || !strings.Contains(result.Limits, "omitted") {
		t.Fatal("partial graph did not disclose omissions")
	}
	wire.Relations = append(wire.Relations, graphRelation{From: "attention", To: "unattested concept", Label: "causes", Kind: "reported", Evidence: []evidenceQuote{{docs[0].ID, docs[0].Summary}}})
	result, err = filterGraph(wire, req, docs)
	if err != nil || result.OmittedItems != 2 || len(result.Edges) != 3 {
		t.Fatalf("unsupported relation survived: %+v %v", result, err)
	}
	for n := range wire.Concepts {
		wire.Concepts[n].Label = "invented"
	}
	if _, err = filterGraph(wire, req, docs); err == nil {
		t.Fatal("graph with no grounded concepts succeeded")
	}
}

func TestGraphRepairsOnlyRealConceptExcerpts(t *testing.T) {
	original := "We compare several methods for understanding river networks."
	summary := original + " Rock fractures guide erosion and alter sediment transport in mountain rivers."
	docs := []Document{{ID: "arxiv:2303.11404", Summary: summary, Title: "Mapping"}}
	wire := graphWire{Concepts: []graphConcept{{Label: "Rock fractures", Evidence: []evidenceQuote{{docs[0].ID, original}}}}}
	result, err := filterGraph(wire, GraphRequest{Query: "geology"}, docs)
	if err != nil || result.ExcerptAdjustments != 1 || result.OmittedItems != 0 || len(result.Edges) != 1 {
		t.Fatalf("%+v %v", result, err)
	}
	quote := result.Edges[0].Evidence[0].Quote
	if !graphTermOccurs(quote, "Rock fractures") {
		t.Fatal("replacement did not contain the exact concept")
	}
	if _, err := verifyEvidenceQuote(summary, quote); err != nil {
		t.Fatal(err)
	}
	wire.Concepts[0].Label = "unattested concept"
	if _, err := filterGraph(wire, GraphRequest{Query: "geology"}, docs); err == nil {
		t.Fatal("repair invented a concept")
	}
	wire.Concepts[0].Label = "Rock fractures"
	wire.Concepts[0].Evidence[0].Quote = "This invented statement is not a source excerpt."
	if _, err := filterGraph(wire, GraphRequest{Query: "geology"}, docs); err == nil {
		t.Fatal("repair accepted an invented quote")
	}
}

func TestGraphSharedConceptsAndHypothesisEndpointGrounding(t *testing.T) {
	common := "Rock fractures guide erosion and alter sediment transport in mountain rivers."
	intro := "We compare several methods for understanding river networks."
	a := Document{ID: "arxiv:2303.11404", Summary: intro + " " + common}
	b := Document{ID: "arxiv:2006.11310", Summary: intro + " Sediment transport records river dynamics and erosion over geological time scales."}
	wire := graphWire{Concepts: []graphConcept{{"erosion", []evidenceQuote{{a.ID, common}}}, {"Rock fractures", []evidenceQuote{{a.ID, common}}}, {"Sediment transport", []evidenceQuote{{b.ID, "Sediment transport records river dynamics and erosion over geological time scales."}}}}, Relations: []graphRelation{{From: "Rock fractures", To: "Sediment transport", Label: "could constrain", Kind: "hypothesis", Evidence: []evidenceQuote{{a.ID, intro}, {b.ID, intro}}, Rationale: "Fracture orientation might constrain transport if scales align.", Test: "Compare a matched baseline; reject if error does not improve."}}}
	result, err := filterGraph(wire, GraphRequest{Query: "geology", IncludeHypotheses: true}, []Document{a, b})
	if err != nil || result.OmittedItems != 0 || len(result.Edges) != 6 || result.Edges[5].Kind != "hypothesis" {
		t.Fatalf("%+v %v", result, err)
	}
	for _, e := range result.Edges[5].Evidence {
		source := a
		if e.ID == b.ID {
			source = b
		}
		if _, err := verifyEvidenceQuote(source.Summary, e.Quote); err != nil {
			t.Fatal(err)
		}
	}
	wire.Relations[0].Evidence[0].Quote = "This quote does not exist in the abstract."
	result, err = filterGraph(wire, GraphRequest{Query: "geology", IncludeHypotheses: true}, []Document{a, b})
	if err != nil || result.OmittedItems != 1 || len(result.Edges) != 5 {
		t.Fatal("hypothesis repair accepted invented evidence")
	}
}
