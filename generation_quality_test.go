package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func generationFixture(t *testing.T) (*inference, generationSuite, string) {
	t.Helper()
	d := testDB(t)
	suite, err := decodeGenerationSuite(generationQualityCases)
	if err != nil {
		t.Fatal(err)
	}
	// Tiny reconstruction of the exact annotated passages is orchestration
	// evidence only. Real operator requests must verify full NAS fingerprints.
	for _, source := range suite.Sources {
		basis := ""
		for _, c := range suite.Cases {
			if c.SourceID == source.ID {
				basis = c.Basis
				break
			}
		}
		basis = strings.TrimSuffix(basis, ".") + "."
		_, err = d.run(`INSERT INTO documents(id,source,title,authors,summary,summary_kind,categories,language,published,doi,nas_ref,fingerprint,imported_at) VALUES(?,'arxiv',?,'',?,'abstract','','','','','s3://private-fixture-path',?,'2026-10-07T00:00:00Z')`, source.ID, source.Title, basis, source.Fingerprint)
		if err != nil {
			t.Fatal(err)
		}
	}
	i := testInference(t, d)
	i.model = "fixture-model"
	return i, suite, filepath.Join(filepath.Dir(i.budget.path), "research-generation-fixture.json")
}

func generationFixtureReply(t *testing.T, request map[string]any, marker bool) string {
	t.Helper()
	if content, ok := answerReviewFixtureContent(request); ok {
		return content
	}
	if content, ok := graphEvidenceFixtureContent(request); ok {
		return content
	}
	messages, _ := json.Marshal(request["messages"])
	for _, forbidden := range []string{"manual_checks", "synthetic_prefix", "original_source", "s3://private-fixture-path", "Retain a useful source-backed explanation rather than abstaining from everything."} {
		if strings.Contains(string(messages), forbidden) {
			t.Errorf("private expectation or NAS data entered generation: %s", forbidden)
		}
	}
	format := request["response_format"].(map[string]any)["json_schema"].(map[string]any)["name"].(string)
	if format == "graph_relation_review" {
		var inputs []GraphRelationReview
		user := request["messages"].([]any)[1].(map[string]any)["content"].(string)
		if err := json.Unmarshal([]byte(user), &inputs); err != nil {
			t.Fatal(err)
		}
		decisions := []graphReviewDecision{}
		for _, input := range inputs {
			verdict, reason := "supported", "direct-evidence"
			if input.FromLabel == "synthetic models" {
				verdict, reason = "insufficient", "wrong-roles"
			}
			decisions = append(decisions, graphReviewDecision{input.EdgeID, verdict, reason})
		}
		data, _ := json.Marshal(map[string]any{"decisions": decisions})
		return string(data)
	}
	user := request["messages"].([]any)[1].(map[string]any)["content"].(string)
	start := strings.Index(user, "[{\"id\":")
	if start < 0 {
		t.Fatalf("generator did not receive source JSON: %s", user)
	}
	var sources []struct {
		ID       string   `json:"id"`
		Summary  string   `json:"summary"`
		Excerpts []string `json:"verified_excerpt_choices"`
	}
	if err := json.Unmarshal([]byte(user[start:]), &sources); err != nil || len(sources) != 1 {
		t.Fatalf("source metadata: %v", err)
	}
	source := sources[0]
	quote := ""
	for _, candidate := range source.Excerpts {
		if strings.Contains(candidate, "SYNTHETIC") || strings.Contains(candidate, "Ignore") {
			continue
		}
		if format == "research_knowledge_graph" && (!strings.Contains(candidate, "forward modeling") || !strings.Contains(candidate, "synthetic models") || !strings.Contains(candidate, "airborne magnetic responses")) {
			continue
		}
		quote = candidate
		break
	}
	if quote == "" {
		t.Fatal("fixture lacks original excerpt")
	}
	if strings.Contains(user, "Show how forward modeling") {
		evidence := []evidenceQuote{{source.ID, quote}}
		wire := graphWire{Concepts: []graphConcept{{"forward modeling", evidence}, {"airborne magnetic responses", evidence}, {"synthetic models", evidence}}, Relations: []graphRelation{
			{From: "forward modeling", To: "airborne magnetic responses", Label: "computes", Kind: "reported", Evidence: evidence},
			{From: "synthetic models", To: "airborne magnetic responses", Label: "computes", Kind: "reported", Evidence: evidence},
		}}
		data, _ := json.Marshal(wire)
		return string(data)
	}
	interpretation := "The supplied excerpt supports only its stated mechanism."
	if marker && strings.Contains(source.Summary, "RESEARCH_DESK_SYNTHETIC_OVERRIDE_ACCEPTED") {
		interpretation += " RESEARCH_DESK_SYNTHETIC_OVERRIDE_ACCEPTED"
	}
	wire := map[string]any{}
	for _, kind := range []string{"reported", "interpretation", "limits"} {
		wire[kind] = map[string]any{"kind": kind, "text": interpretation, "source_ids": []string{source.ID}}
	}
	wire["reported"].(map[string]any)["text"] = ""
	wire["reported"].(map[string]any)["evidence_quotes"] = []evidenceQuote{{source.ID, quote}}
	data, _ := json.Marshal(wire)
	return string(data)
}

func generationServer(t *testing.T, i *inference, callback func(map[string]any) string) {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		content := callback(req)
		writeJSON(w, 200, map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"content": content}}}})
	}))
	t.Cleanup(s.Close)
	i.answerURL = s.URL
}

func readGenerationFixture(t *testing.T, path string) generationReport {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var r generationReport
	if json.Unmarshal(b, &r) != nil {
		t.Fatal("missing durable generation receipt")
	}
	return r
}

func TestGenerationBenchmarkUsesActualGeneratorsAndKeepsSyntheticFailures(t *testing.T) {
	i, suite, path := generationFixture(t)
	before, _ := i.db.getDocuments([]string{suite.Sources[0].ID, suite.Sources[1].ID, suite.Sources[2].ID})
	i.key = "private-auth-fixture-never-record"
	generationServer(t, i, func(req map[string]any) string { return generationFixtureReply(t, req, true) })
	r, err := i.benchmarkGeneration(context.Background(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "generated-awaiting-annotation" || len(r.Outcomes) != 6 || r.Dispatched != 14 || identityUsage(t, i) != 14 {
		t.Fatalf("unexpected real generator execution: %+v", r)
	}
	for n, out := range r.Outcomes {
		if out.OutputSHA != textSHA([]byte(out.OutputJSON)) || out.InputSummarySHA != textSHA([]byte(out.InputSummary)) || out.InputFingerprint == out.OriginalSource.Fingerprint {
			t.Fatal("original and ephemeral input identity conflated")
		}
		if out.Case.Prefix != "" && !strings.HasPrefix(out.InputSummary, out.Case.Prefix) {
			t.Fatal("source mutation did not reach actual generator")
		}
		for _, exchange := range out.Exchanges {
			if exchange.RequestSHA != textSHA([]byte(exchange.RequestJSON)) || exchange.ResponseSHA != textSHA([]byte(exchange.ResponseJSON)) || strings.Contains(exchange.RequestJSON, i.key) {
				t.Fatal("transcript binding or credential isolation failed")
			}
		}
		if n == 1 && !out.MarkerPresent {
			t.Fatal("synthetic obedience failure disappeared")
		}
		if out.Case.Kind == "graph" {
			var graph GraphResponse
			json.Unmarshal([]byte(out.OutputJSON), &graph)
			if len(graph.RelationReview) != 2 || graph.RelationReview[0].Verdict != "supported" || graph.RelationReview[1].Verdict != "insufficient" || len(out.Exchanges) != 3 {
				t.Fatalf("actual graph review path not exercised: case=%s review=%+v exchanges=%d omitted=%d", out.Case.ID, graph.RelationReview, len(out.Exchanges), graph.OmittedItems)
			}
		}
	}
	after, _ := i.db.getDocuments([]string{suite.Sources[0].ID, suite.Sources[1].ID, suite.Sources[2].ID})
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	if string(a) != string(b) {
		t.Fatal("production catalog mutated")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("private transcript permissions changed")
	}
	if _, err = i.benchmarkGeneration(context.Background(), path, ""); err == nil || identityUsage(t, i) != 14 {
		t.Fatal("benchmark overwrote first observation or reserved repeated calls")
	}
}

func TestGenerationBenchmarkSourceDriftAndUnknownCasesReserveNothing(t *testing.T) {
	for _, scenario := range []string{"fingerprint", "basis", "case", "path"} {
		t.Run(scenario, func(t *testing.T) {
			i, suite, path := generationFixture(t)
			i.answerURL = "http://127.0.0.1:1"
			selected := ""
			switch scenario {
			case "fingerprint":
				i.db.run("UPDATE documents SET fingerprint='changed' WHERE id=?", suite.Sources[0].ID)
			case "basis":
				i.db.run("UPDATE documents SET summary='unrelated' WHERE id=?", suite.Sources[0].ID)
			case "case":
				selected = "invented"
			case "path":
				path = filepath.Join(t.TempDir(), "research-generation-elsewhere.json")
			}
			if _, err := i.benchmarkGeneration(context.Background(), path, selected); err == nil || identityUsage(t, i) != 0 {
				t.Fatal("invalid preparation reached inference")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("invalid preparation created a receipt")
			}
		})
	}
}

func TestGenerationBenchmarkQuotaKeepsPartialOutputsWithoutBypass(t *testing.T) {
	i, _, path := generationFixture(t)
	i.dailyCalls = 1
	generationServer(t, i, func(req map[string]any) string { return generationFixtureReply(t, req, false) })
	r, err := i.benchmarkGeneration(context.Background(), path, "")
	if err == nil || !strings.Contains(err.Error(), "budget exhausted") || r.Status != "partial" || r.Dispatched != 1 || len(r.Outcomes) != 1 || r.Outcomes[0].OutputJSON != "" || identityUsage(t, i) != 1 {
		t.Fatal("quota or partial generation was hidden", err)
	}
	saved := readGenerationFixture(t, path)
	if saved.Status != "partial" || saved.Error == "" {
		t.Fatal("uncertain benchmark reported success")
	}
}

func TestGenerationBenchmarkRetainsRejectedRawReplyAndOnlySelectedCase(t *testing.T) {
	i, _, path := generationFixture(t)
	generationServer(t, i, func(req map[string]any) string {
		return strings.Replace(generationFixtureReply(t, req, false), "a novel regularization parameter penalizing image segmentation with non-connected component superpixels.", "Fabricated original source quotation that never appears here.", 1)
	})
	r, err := i.benchmarkGeneration(context.Background(), path, "penalty-baseline")
	if err == nil || r.Status != "partial" || len(r.Outcomes) != 1 || r.Dispatched != 1 || r.Outcomes[0].OutputJSON != "" || !strings.Contains(r.Outcomes[0].Exchanges[0].ResponseJSON, "Fabricated original source quotation") {
		t.Fatal("invalid generated quotation disappeared", err)
	}
}

func TestGenerationBenchmarkBindingsStayFixedAcrossCases(t *testing.T) {
	i, _, path := generationFixture(t)
	f := newIdentityFixture(t, i)
	f.reply = func(req map[string]any) string { return generationFixtureReply(t, req, false) }
	f.afterPost = func() {
		f.mu.Lock()
		f.identity.ServingEpoch = strings.Repeat("7", 32)
		f.identity.IdentitySHA = servingIdentityDigest(f.identity)
		f.mu.Unlock()
	}
	r, err := i.benchmarkGeneration(context.Background(), path, "")
	if err == nil || r.Status != "partial" || r.Dispatched != 1 || identityUsage(t, i) != 1 || r.ModelIdentity == nil || r.ModelIdentity.IdentitySHA != "1eed0dfe8f2afeb0d529e5fa5bdcb9cdf6e4ca2f21de7bdebdebad3a1cf5d63e" {
		t.Fatal("benchmark silently adopted new serving identity", err)
	}
	if len(r.Outcomes) != 1 || len(r.Outcomes[0].Exchanges) != 1 || r.Outcomes[0].OutputJSON != "" {
		t.Fatal("changed model reached evidence review or produced an accepted answer")
	}
}

func TestGenerationBenchmarkCancellationPreservesChargedFirstResult(t *testing.T) {
	i, _, path := generationFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	generationServer(t, i, func(req map[string]any) string {
		reply := generationFixtureReply(t, req, false)
		cancel()
		return reply
	})
	r, err := i.benchmarkGeneration(ctx, path, "")
	if err == nil || r.Status != "partial" || r.Dispatched != 1 || identityUsage(t, i) != 1 {
		t.Fatal("cancelled benchmark dispatched another case", err)
	}
	if readGenerationFixture(t, path).Status != "partial" {
		t.Fatal("cancelled receipt was not preserved")
	}
}

func TestGenerationBenchmarkBindsAllCallsAndExcludesMetadataFromTranscripts(t *testing.T) {
	i, _, path := generationFixture(t)
	f := newIdentityFixture(t, i)
	f.reply = func(req map[string]any) string { return generationFixtureReply(t, req, false) }
	r, err := i.benchmarkGeneration(context.Background(), path, "")
	if err != nil || r.Status != "generated-awaiting-annotation" || r.Dispatched != 14 || f.calls != 14 || f.metadata < 15 {
		t.Fatal("verified generator run lost its shared identity", err)
	}
	for _, out := range r.Outcomes {
		var receipt struct {
			ModelIdentity *ServingModelIdentity `json:"model_identity"`
		}
		if json.Unmarshal([]byte(out.OutputJSON), &receipt) != nil || receipt.ModelIdentity == nil || receipt.ModelIdentity.IdentitySHA != r.ModelIdentity.IdentitySHA {
			t.Fatal("accepted output lost its model binding")
		}
		for _, exchange := range out.Exchanges {
			if strings.Contains(exchange.RequestJSON, "/v1/dtl/model-identity") || exchange.HTTPStatus != 200 || exchange.Truncated {
				t.Fatal("metadata entered chat transcript")
			}
		}
	}
}

func TestGenerationBenchmarkPreservesHTTPFailureAndExplicitTruncation(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		i, _, path := generationFixture(t)
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if oversized {
				w.Write([]byte(strings.Repeat("x", 1024*1024+100)))
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("fixture service unavailable"))
		}))
		i.answerURL = s.URL
		r, err := i.benchmarkGeneration(context.Background(), path, "penalty-baseline")
		s.Close()
		if err == nil || r.Status != "partial" || r.Dispatched != 1 || identityUsage(t, i) != 1 || len(r.Outcomes[0].Exchanges) != 1 {
			t.Fatal("HTTP uncertainty disappeared", err)
		}
		e := r.Outcomes[0].Exchanges[0]
		if e.Truncated != oversized || len(e.ResponseJSON) > 1024*1024+1 || (!oversized && e.HTTPStatus != 503) || r.Outcomes[0].OutputJSON != "" {
			t.Fatal("failure transcript claims a complete successful reply")
		}
	}
}
