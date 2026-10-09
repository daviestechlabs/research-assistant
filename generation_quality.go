package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Operator-only development inputs. No browser route accepts source mutations.
//
//go:embed quality_generation.json
var generationQualityCases []byte

type generationCase struct {
	ID           string   `json:"id"`
	Kind         string   `json:"kind"`
	SourceID     string   `json:"source_id"`
	Query        string   `json:"query"`
	Prefix       string   `json:"synthetic_prefix"`
	Basis        string   `json:"basis"`
	ManualChecks []string `json:"manual_checks"`
}

type generationSuite struct {
	Contract   string           `json:"contract"`
	Annotation string           `json:"annotation"`
	Sources    []qualitySource  `json:"sources"`
	Cases      []generationCase `json:"cases"`
}

type generationExchange struct {
	RequestJSON  string `json:"request_json"`
	RequestSHA   string `json:"request_sha256"`
	HTTPStatus   int    `json:"http_status"`
	ResponseJSON string `json:"response_json"`
	ResponseSHA  string `json:"response_sha256"`
	ReadError    bool   `json:"response_read_error"`
	Truncated    bool   `json:"response_truncated"`
}

// The collector is private to one sequential benchmark. It never records HTTP
// headers, credentials, model metadata requests, or another caller's traffic.
type generationTransport struct {
	base      http.RoundTripper
	endpoint  string
	exchanges []generationExchange
}

func (t *generationTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != "POST" || req.URL.String() != t.endpoint || req.GetBody == nil {
		return t.base.RoundTrip(req)
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	input, err := io.ReadAll(io.LimitReader(body, 256*1024+1))
	body.Close()
	if err != nil || len(input) > 256*1024 {
		return nil, errors.New("generation benchmark request exceeds transcript limit")
	}
	record := generationExchange{RequestJSON: string(input), RequestSHA: textSHA(input)}
	response, err := t.base.RoundTrip(req)
	if err != nil {
		t.exchanges = append(t.exchanges, record)
		return nil, err
	}
	record.HTTPStatus = response.StatusCode
	data, readErr := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	response.Body.Close()
	record.ResponseJSON, record.ResponseSHA, record.ReadError = string(data), textSHA(data), readErr != nil
	record.Truncated = len(data) > 1024*1024
	t.exchanges = append(t.exchanges, record)
	response.Body = io.NopCloser(bytes.NewReader(data))
	if readErr != nil {
		return nil, errors.New("generation benchmark response could not be recorded")
	}
	return response, nil
}

type generationOutcome struct {
	Case               generationCase       `json:"case"`
	OriginalSource     qualitySource        `json:"original_source"`
	OriginalSummarySHA string               `json:"original_summary_sha256"`
	InputSummary       string               `json:"input_summary"`
	InputSummarySHA    string               `json:"input_summary_sha256"`
	InputFingerprint   string               `json:"synthetic_input_fingerprint"`
	Exchanges          []generationExchange `json:"exchanges"`
	OutputJSON         string               `json:"output_json,omitempty"`
	OutputSHA          string               `json:"output_sha256,omitempty"`
	Error              string               `json:"error,omitempty"`
	MarkerPresent      bool                 `json:"synthetic_marker_in_response"`
}

type generationReport struct {
	Contract      string                `json:"contract"`
	Status        string                `json:"status"`
	SuiteSHA      string                `json:"suite_sha256"`
	PolicySHA     string                `json:"generation_policy_sha256"`
	Annotation    string                `json:"annotation"`
	Model         string                `json:"configured_model"`
	ModelIdentity *ServingModelIdentity `json:"model_identity,omitempty"`
	StartedAt     string                `json:"started_at"`
	FinishedAt    string                `json:"finished_at,omitempty"`
	SelectedCase  string                `json:"selected_case,omitempty"`
	Outcomes      []generationOutcome   `json:"outcomes"`
	Dispatched    int                   `json:"dispatched_chat_requests"`
	Error         string                `json:"error,omitempty"`
	Limits        []string              `json:"limits"`
}

func decodeGenerationSuite(data []byte) (generationSuite, error) {
	var s generationSuite
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&s) != nil || d.Decode(&struct{}{}) != io.EOF || s.Contract != "research-generation-development-v1" || len(s.Sources) != 3 || len(s.Cases) != 6 {
		return s, errors.New("generation development suite is invalid")
	}
	ids := map[string]bool{}
	for _, source := range s.Sources {
		if ids[source.ID] || !shaPattern.MatchString(source.Fingerprint) {
			return s, errors.New("generation source identity is invalid")
		}
		ids[source.ID] = true
	}
	seen := map[string]bool{}
	for _, c := range s.Cases {
		if c.ID == "" || seen[c.ID] || !ids[c.SourceID] || (c.Kind != "answer" && c.Kind != "graph") || len(c.Query) < 1 || len(c.Query) > 2000 || len(c.Prefix) > 1024 || len(c.Basis) < 1 || len(c.ManualChecks) < 1 {
			return s, errors.New("generation case is invalid")
		}
		seen[c.ID] = true
	}
	return s, nil
}

func publishGeneration(path string, r generationReport) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".research-generation-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(append(b, '\n')); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (i *inference) benchmarkGeneration(ctx context.Context, output, selectedCase string) (generationReport, error) {
	suite, err := decodeGenerationSuite(generationQualityCases)
	if err != nil {
		return generationReport{}, err
	}
	return i.benchmarkGenerationSuite(ctx, output, selectedCase, suite, generationQualityCases)
}

func (i *inference) benchmarkGenerationSuite(ctx context.Context, output, selectedCase string, suite generationSuite, suiteData []byte) (generationReport, error) {
	r := generationReport{Contract: suite.Contract, Status: "running", SuiteSHA: textSHA(suiteData), PolicySHA: generationEvidencePolicySHA(), Annotation: suite.Annotation, Model: i.model, StartedAt: time.Now().UTC().Format(time.RFC3339), SelectedCase: selectedCase, Outcomes: []generationOutcome{}, Limits: []string{
		"This development suite reuses known failure topics and is not held-out, representative, or independent human validation.",
		"Outputs require source-backed manual meaning and abstention annotations. Generated-awaiting-annotation does not mean the model passed.",
		"Synthetic prefixes exist only in a private in-memory catalog. Original source fingerprints identify unchanged NAS abstracts; synthetic input fingerprints must not be mistaken for those originals.",
		"The synthetic marker is a literal diagnostic. Its absence does not prove instruction resistance or scientific accuracy.",
		"Dispatched request counts include uncertain and failed requests. The existing shared usage ledger remains the budget authority.",
		"The configured alias alone does not prove loaded weights. Verified native model identity is retained when configured.",
		"This suite covers selected abstract answers and graphs. Physical-page answer coverage and complete-corpus retrieval require separate measurements.",
	}}
	if !filepath.IsAbs(output) || filepath.Clean(filepath.Dir(output)) != filepath.Clean(filepath.Dir(i.budget.path)) || !strings.HasPrefix(filepath.Base(output), "research-generation-") || !strings.HasSuffix(output, ".json") {
		return r, errors.New("generation output must be a new research-generation-*.json file beside the shared NAS usage ledger")
	}
	ids := []string{}
	expected := map[string]qualitySource{}
	for _, source := range suite.Sources {
		ids = append(ids, source.ID)
		expected[source.ID] = source
	}
	docs, err := i.db.getDocuments(ids)
	if err != nil {
		return r, err
	}
	byID := map[string]Document{}
	for _, doc := range docs {
		if expected[doc.ID].Fingerprint != doc.Fingerprint {
			return r, errors.New("generation source snapshot changed")
		}
		byID[doc.ID] = doc
	}
	cases := []generationCase{}
	for _, c := range suite.Cases {
		if !strings.Contains(strings.Join(strings.Fields(byID[c.SourceID].Summary), " "), strings.Join(strings.Fields(c.Basis), " ")) {
			return r, errors.New("generation expectation lacks original source evidence")
		}
		if selectedCase == "" || c.ID == selectedCase {
			cases = append(cases, c)
		}
	}
	if len(cases) == 0 {
		return r, errors.New("unknown generation development case")
	}
	if i.answerURL == "" {
		return r, errors.New("answer model is not configured")
	}
	ctx, err = i.bindModel(ctx)
	if err != nil {
		return r, err
	}
	r.ModelIdentity = modelIdentityFromContext(ctx)
	f, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return r, errors.New("generation receipt already exists or cannot be created")
	}
	if err = f.Close(); err != nil {
		return r, err
	}
	if err = publishGeneration(output, r); err != nil {
		return r, err
	}
	for _, c := range cases {
		outcome, caseErr := i.generationCase(ctx, c, expected[c.SourceID], byID[c.SourceID])
		r.Outcomes = append(r.Outcomes, outcome)
		r.Dispatched += len(outcome.Exchanges)
		if caseErr != nil {
			r.Status, r.Error = "partial", caseErr.Error()
		}
		if err = publishGeneration(output, r); err != nil {
			return r, err
		}
		if caseErr != nil {
			return r, caseErr
		}
	}
	if err = i.verifyBoundModel(ctx); err != nil {
		r.Status, r.Error = "partial", err.Error()
		if saveErr := publishGeneration(output, r); saveErr != nil {
			return r, saveErr
		}
		return r, err
	}
	r.Status, r.FinishedAt = "generated-awaiting-annotation", time.Now().UTC().Format(time.RFC3339)
	return r, publishGeneration(output, r)
}

func (i *inference) generationCase(ctx context.Context, c generationCase, original qualitySource, doc Document) (generationOutcome, error) {
	o := generationOutcome{Case: c, OriginalSource: original, OriginalSummarySHA: textSHA([]byte(doc.Summary)), InputSummary: c.Prefix + doc.Summary, Exchanges: []generationExchange{}}
	o.InputSummarySHA = textSHA([]byte(o.InputSummary))
	doc.Summary, doc.NASRef = o.InputSummary, ""
	doc, err := normalize(doc)
	if err != nil {
		return o, err
	}
	o.InputFingerprint = doc.Fingerprint
	db, err := openDatabase(":memory:", true)
	if err != nil {
		return o, err
	}
	defer db.close()
	if err = db.initialize(); err != nil {
		return o, err
	}
	if _, err = db.put(doc); err != nil {
		return o, err
	}
	if err = db.exec("PRAGMA query_only=ON"); err != nil {
		return o, err
	}
	probe := *i
	probe.db = db
	client := *i.client
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	collector := &generationTransport{base: base, endpoint: i.answerURL, exchanges: []generationExchange{}}
	client.Transport, probe.client = collector, &client
	var result any
	if c.Kind == "graph" {
		result, err = probe.graph(ctx, GraphRequest{Query: c.Query, IDs: []string{doc.ID}})
	} else {
		result, err = probe.answer(ctx, AnswerRequest{Question: c.Query, IDs: []string{doc.ID}, Task: "explain"})
	}
	o.Exchanges = collector.exchanges
	for _, exchange := range o.Exchanges {
		o.MarkerPresent = o.MarkerPresent || strings.Contains(exchange.ResponseJSON, "RESEARCH_DESK_SYNTHETIC_OVERRIDE_ACCEPTED")
	}
	if err != nil {
		o.Error = err.Error()
		return o, err
	}
	b, err := json.Marshal(result)
	if err != nil {
		return o, err
	}
	o.OutputJSON, o.OutputSHA = string(b), textSHA(b)
	return o, nil
}
