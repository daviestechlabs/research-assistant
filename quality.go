package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Expectations are deliberately separate from the model request. They are
// curated annotations, not a claim of independent human or scientific review.
//
//go:embed quality_cases.json
var qualityCases []byte

//go:embed quality_holdout.json
var qualityHoldout []byte

type qualitySource struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Fingerprint string `json:"fingerprint"`
}

type qualityCase struct {
	ID                   string      `json:"id"`
	Category             string      `json:"category"`
	Kind                 string      `json:"kind"`
	Text                 string      `json:"text"`
	Evidence             []pageQuote `json:"evidence"`
	ExpectedSupported    bool        `json:"expected_supported"`
	Rationale            string      `json:"rationale"`
	SyntheticInstruction string      `json:"synthetic_instruction,omitempty"`
}

type qualityQuery struct {
	ID       string `json:"id"`
	Query    string `json:"query"`
	TargetID string `json:"target_id"`
	Kind     string `json:"kind"`
}

type qualitySuite struct {
	Contract   string          `json:"contract"`
	Annotation string          `json:"annotation"`
	Sources    []qualitySource `json:"sources"`
	Cases      []qualityCase   `json:"cases"`
	Retrieval  []qualityQuery  `json:"retrieval"`
}

type qualityOutcome struct {
	CaseID   string            `json:"case_id"`
	Expected bool              `json:"expected_supported"`
	Accepted bool              `json:"accepted"`
	Reason   string            `json:"reason"`
	Finding  ComparisonFinding `json:"finding"`
}

type qualityRetrieval struct {
	qualityQuery
	Rank      int      `json:"rank_at_10"`
	ResultIDs []string `json:"result_ids"`
	ElapsedMS int64    `json:"elapsed_ms"`
}

type qualityCounts struct {
	SupportedCases   int `json:"supported_cases"`
	UnsupportedCases int `json:"unsupported_cases"`
	TrueAccepts      int `json:"true_accepts"`
	TrueAbstentions  int `json:"true_abstentions"`
	FalseAccepts     int `json:"false_accepts"`
	FalseAbstentions int `json:"false_abstentions"`
}

type qualityQuoteChecks struct {
	VerifiedExcerpts   int    `json:"verified_excerpts"`
	FabricatedRejected int    `json:"fabricated_rejected"`
	MisboundRejected   int    `json:"misbound_rejected"`
	Scope              string `json:"scope"`
}

type qualityReport struct {
	Status         string                `json:"status"`
	Contract       string                `json:"contract"`
	SuiteSHA       string                `json:"suite_sha256"`
	PolicySHA      string                `json:"review_policy_sha256"`
	Model          string                `json:"configured_model"`
	ModelIdentity  *ServingModelIdentity `json:"model_identity,omitempty"`
	StartedAt      string                `json:"started_at"`
	CompletedAt    string                `json:"completed_at,omitempty"`
	Sources        []qualitySource       `json:"sources"`
	Annotation     string                `json:"annotation"`
	Cases          []qualityCase         `json:"cases"`
	Outcomes       []qualityOutcome      `json:"outcomes"`
	Counts         qualityCounts         `json:"counts"`
	Quotes         qualityQuoteChecks    `json:"quote_validator"`
	Retrieval      []qualityRetrieval    `json:"keyword_retrieval"`
	RetrievalHits  int                   `json:"keyword_hits_at_10"`
	RetrievalMRR   float64               `json:"keyword_mrr_at_10"`
	SemanticStatus string                `json:"semantic_status"`
	ModelCalls     int                   `json:"model_calls"`
	ReservedUnits  int                   `json:"reserved_units"`
	Limits         []string              `json:"limits"`
}

func textSHA(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func loadQualitySuite() (qualitySuite, error) {
	return decodeQualitySuite(qualityCases, 16, 4, 8)
}

func decodeQualitySuite(data []byte, cases, sources, queries int) (qualitySuite, error) {
	var s qualitySuite
	err := json.Unmarshal(data, &s)
	if err != nil || s.Contract == "" || len(s.Cases) != cases || len(s.Sources) != sources || len(s.Retrieval) != queries {
		return s, errors.New("quality suite is invalid")
	}
	return s, nil
}

func qualityPath(i *inference) string {
	return filepath.Join(filepath.Dir(i.budget.path), "research-quality.json")
}

// Verify every real excerpt against the current NAS metadata before making a
// model request. Adversarial instructions are separate synthetic quotations;
// this function never alters the catalog or a real source fingerprint.
func prepareQuality(d *database, suite qualitySuite) ([]ComparisonFinding, qualityQuoteChecks, error) {
	ids := []string{}
	for _, source := range suite.Sources {
		ids = append(ids, source.ID)
	}
	docs, err := d.getDocuments(ids)
	if err != nil {
		return nil, qualityQuoteChecks{}, err
	}
	byID := map[string]Document{}
	for _, doc := range docs {
		byID[doc.ID] = doc
	}
	for _, source := range suite.Sources {
		if byID[source.ID].Fingerprint != source.Fingerprint {
			return nil, qualityQuoteChecks{}, errors.New("quality source snapshot changed; update the curated expectations")
		}
	}
	checks := qualityQuoteChecks{Scope: "Exact excerpt validator checks, not model-generated quotation accuracy."}
	findings := []ComparisonFinding{}
	seen := map[string]bool{}
	for _, c := range suite.Cases {
		if seen[c.ID] || c.ID == "" || len(c.Evidence) == 0 {
			return nil, checks, errors.New("quality case is invalid")
		}
		seen[c.ID] = true
		finding := ComparisonFinding{Kind: c.Kind, Text: c.Text, Citations: []ComparisonCitation{}}
		for _, q := range c.Evidence {
			doc := byID[q.ID]
			if doc.ID == "" || q.Page != 0 {
				return nil, checks, errors.New("quality citation is not a selected abstract")
			}
			if _, err := verifyEvidenceQuote(doc.Summary, q.Quote); err != nil {
				return nil, checks, err
			}
			checks.VerifiedExcerpts++
			finding.Citations = append(finding.Citations, ComparisonCitation{pageQuote: q, Scope: "abstract", SourceFingerprint: doc.Fingerprint})
		}
		if c.SyntheticInstruction != "" {
			finding.Citations = append(finding.Citations, ComparisonCitation{pageQuote: pageQuote{ID: "synthetic:adversarial-source", Quote: c.SyntheticInstruction}, Scope: "synthetic-adversarial-excerpt", SourceFingerprint: textSHA([]byte(c.SyntheticInstruction))})
		}
		findings = append(findings, finding)
	}
	for _, source := range suite.Sources {
		if _, err := verifyEvidenceQuote(byID[source.ID].Summary, "This fabricated benchmark excerpt reports a guaranteed result of 987654321 percent."); err == nil {
			return nil, checks, errors.New("quality validator accepted a fabricated excerpt")
		}
		checks.FabricatedRejected++
	}
	// Deliberately bind an authentic geology excerpt to a different paper.
	for _, c := range suite.Cases {
		q := c.Evidence[0]
		for _, source := range suite.Sources {
			if source.ID == q.ID {
				continue
			}
			if _, err := verifyEvidenceQuote(byID[source.ID].Summary, q.Quote); err == nil {
				return nil, checks, errors.New("quality misbound excerpt is ambiguous")
			}
			checks.MisboundRejected++
			break
		}
	}
	return findings, checks, nil
}

func summarizeQuality(r *qualityReport) error {
	if len(r.Outcomes) != len(r.Cases) {
		return errors.New("quality report is incomplete")
	}
	r.Counts = qualityCounts{}
	seen := map[string]bool{}
	for n, out := range r.Outcomes {
		if out.CaseID != r.Cases[n].ID || out.Expected != r.Cases[n].ExpectedSupported || seen[out.CaseID] {
			return errors.New("quality report has misbound case outcomes")
		}
		seen[out.CaseID] = true
		reasonOK := false
		for _, reason := range comparisonReviewReasons {
			if reason == out.Reason {
				reasonOK = true
			}
		}
		if !reasonOK || out.Accepted != (out.Reason == "direct-evidence") {
			return errors.New("quality verdict and reason disagree")
		}
		if out.Expected {
			r.Counts.SupportedCases++
			if out.Accepted {
				r.Counts.TrueAccepts++
			} else {
				r.Counts.FalseAbstentions++
			}
		} else {
			r.Counts.UnsupportedCases++
			if out.Accepted {
				r.Counts.FalseAccepts++
			} else {
				r.Counts.TrueAbstentions++
			}
		}
	}
	r.RetrievalHits, r.RetrievalMRR = 0, 0
	for _, row := range r.Retrieval {
		if row.Rank > 0 && row.Rank <= 10 {
			r.RetrievalHits++
			r.RetrievalMRR += 1 / float64(row.Rank)
		}
	}
	if len(r.Retrieval) > 0 {
		r.RetrievalMRR /= float64(len(r.Retrieval))
	}
	return nil
}

// Same-directory rename and fsync keep a complete receipt on NAS. An interrupted
// benchmark leaves its explicit running receipt instead of fabricated success.
func publishQuality(path string, r any) error {
	if !filepath.IsAbs(path) {
		return errors.New("quality output must be an absolute NAS path")
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".research-quality-")
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

func (i *inference) benchmarkQuality(ctx context.Context, path string) (qualityReport, error) {
	suite, err := loadQualitySuite()
	if err != nil {
		return qualityReport{}, err
	}
	return i.benchmarkQualitySuite(ctx, path, suite, qualityCases, false)
}

func (i *inference) benchmarkQualityHoldout(ctx context.Context, path string) (qualityReport, error) {
	suite, err := decodeQualitySuite(qualityHoldout, 4, 2, 0)
	if err != nil {
		return qualityReport{}, err
	}
	if filepath.Clean(path) != filepath.Join(filepath.Dir(i.budget.path), "research-quality-holdout.json") {
		return qualityReport{}, errors.New("held-out receipt must use the shared NAS budget directory")
	}
	if _, _, err = prepareQuality(i.db, suite); err != nil {
		return qualityReport{}, err
	}
	// Keep the first measurement, including interrupted runs. Once observed,
	// these cases must not silently become fresh holdouts on a later policy.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return qualityReport{}, errors.New("held-out measurement already exists or cannot be reserved")
	}
	if err = f.Close(); err != nil {
		return qualityReport{}, err
	}
	return i.benchmarkQualitySuite(ctx, path, suite, qualityHoldout, true)
}

func (i *inference) benchmarkQualitySuite(ctx context.Context, path string, suite qualitySuite, suiteData []byte, holdout bool) (qualityReport, error) {
	findings, quotes, err := prepareQuality(i.db, suite)
	if err != nil {
		return qualityReport{}, err
	}
	ctx, err = i.bindModel(ctx)
	if err != nil {
		return qualityReport{}, err
	}
	r := qualityReport{Status: "running", Contract: suite.Contract, SuiteSHA: textSHA(suiteData), PolicySHA: textSHA([]byte(evidenceReviewPrompt)), Model: i.model, StartedAt: time.Now().UTC().Format(time.RFC3339), Sources: suite.Sources, Annotation: suite.Annotation, Cases: suite.Cases, Outcomes: []qualityOutcome{}, Quotes: quotes, Retrieval: []qualityRetrieval{}, SemanticStatus: "Not measured; the complete abstract vector index is still required.", Limits: []string{
		"Sixteen fixed candidate claims test the deployed comparison reviewer, not end-to-end generated answers or graphs.",
		"Expectations were authored by Codex and checked against four saved abstracts; they are not independent human gold labels.",
		"The same local model generates and reviews comparisons. Agreement with these labels does not establish scientific accuracy.",
		"The review policy was revised after failures on this suite. These reused cases are development checks, not held-out evidence of generalization.",
		"Quote checks exercise the deterministic validator; they do not measure generated quotation accuracy.",
		"Retrieval measures one curated target per query at ten results, not all relevant papers or corpus-wide recall.",
		"The configured model alias is recorded. Exact backend weight identity remains a separate verification requirement.",
		"The source-instruction case contains an explicitly synthetic mutation; production source text is unchanged.",
	}}
	r.ModelIdentity = modelIdentityFromContext(ctx)
	if r.ModelIdentity != nil {
		r.Limits[6] = "This run binds every review call to the recorded verified loader-input identity and serving epoch. It does not hash GPU memory."
	}
	if holdout {
		r.Limits[0] = "Four new fixed claims test the current comparison evidence reviewer, not end-to-end generated answers or graphs."
		r.Limits[1] = "Expectations were authored by Codex and checked against two saved abstracts; they are not independent human gold labels."
		r.Limits[3] = "Cases and the current review policy were sealed before this first measurement. The outcome cannot support future prompt tuning while retaining a held-out claim."
		r.Limits[5] = "This held-out reviewer check does not measure retrieval."
		r.Limits[7] = "These four held-out cases contain real saved excerpts without synthetic source mutation."
	}
	if err = publishQuality(path, r); err != nil {
		return r, err
	}
	for _, q := range suite.Retrieval {
		result, err := i.db.search(SearchRequest{Query: q.Query, Source: "arxiv", Limit: 10})
		if err != nil {
			return r, err
		}
		row := qualityRetrieval{qualityQuery: q, ResultIDs: []string{}, ElapsedMS: result.ElapsedMS}
		for n, doc := range result.Documents {
			row.ResultIDs = append(row.ResultIDs, doc.ID)
			if doc.ID == q.TargetID {
				row.Rank = n + 1
			}
		}
		r.Retrieval = append(r.Retrieval, row)
	}
	// Four bounded serial requests use the real shared reservation ledger. Never
	// send expected labels or rationale to the model.
	for start := 0; start < len(findings); start += 4 {
		result := AnswerResponse{Comparison: &EvidenceComparison{Sections: []ComparisonSection{}}}
		for n := start; n < start+4 && n < len(findings); n++ {
			result.Comparison.Sections = append(result.Comparison.Sections, ComparisonSection{Category: suite.Cases[n].Category, Status: "supported", Findings: []ComparisonFinding{findings[n]}})
		}
		// Category/index pairs must be unique, as in production comparison output.
		merged := []ComparisonSection{}
		for _, section := range result.Comparison.Sections {
			found := false
			for n := range merged {
				if merged[n].Category == section.Category {
					merged[n].Findings = append(merged[n].Findings, section.Findings...)
					found = true
					break
				}
			}
			if !found {
				merged = append(merged, section)
			}
		}
		result.Comparison.Sections = merged
		if err = i.reviewComparison(ctx, &result); err != nil {
			return r, err
		}
		r.ModelCalls += result.ModelCalls
		r.ReservedUnits += result.ReservedUnits
		indices := map[string]int{}
		for n := start; n < start+4 && n < len(findings); n++ {
			c := suite.Cases[n]
			index := indices[c.Category]
			indices[c.Category]++
			for _, review := range result.Comparison.Review {
				if review.Category == c.Category && review.Index == index {
					r.Outcomes = append(r.Outcomes, qualityOutcome{c.ID, c.ExpectedSupported, review.Verdict == "supported", review.Reason, findings[n]})
				}
			}
		}
		if err = publishQuality(path, r); err != nil {
			return r, err
		}
	}
	if err = summarizeQuality(&r); err != nil {
		return r, err
	}
	if err := i.verifyBoundModel(ctx); err != nil {
		return r, err
	}
	r.Status, r.CompletedAt = "complete", time.Now().UTC().Format(time.RFC3339)
	return r, publishQuality(path, r)
}

func readQuality(i *inference) (qualityReport, error) {
	return readQualitySuite(i, qualityPath(i), qualityCases, 16, 4, 8)
}

func readQualitySuite(i *inference, path string, suiteData []byte, cases, sources, queries int) (qualityReport, error) {
	r := qualityReport{Status: "missing"}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, errors.New("quality receipt unavailable")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 1024*1024+1))
	if err != nil || len(b) > 1024*1024 {
		return r, errors.New("quality receipt exceeds bounds")
	}
	decoder := json.NewDecoder(strings.NewReader(string(b)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&r) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return r, errors.New("quality receipt invalid")
	}
	suite, err := decodeQualitySuite(suiteData, cases, sources, queries)
	if err != nil {
		return r, err
	}
	expected, _ := json.Marshal(suite.Cases)
	observed, _ := json.Marshal(r.Cases)
	if r.Contract != suite.Contract || r.SuiteSHA != textSHA(suiteData) || r.PolicySHA != textSHA([]byte(evidenceReviewPrompt)) || r.Model != i.model || string(expected) != string(observed) {
		r.Status = "stale"
		return r, nil
	}
	if i.identityURL != "" && (r.ModelIdentity == nil || i.checkServingIdentity(*r.ModelIdentity) != nil) {
		// Alias-only historical measurements do not establish the reviewed
		// model's quality. Keep their saved receipts, but require a fresh run.
		r.Status = "stale"
		return r, nil
	}
	findings, quotes, err := prepareQuality(i.db, suite)
	if err != nil {
		r.Status = "stale"
		return r, nil
	}
	if r.Status == "complete" {
		if len(r.Retrieval) != len(suite.Retrieval) || len(r.Outcomes) != len(findings) {
			return r, errors.New("quality measurement is incomplete")
		}
		for n := range r.Retrieval {
			row := &r.Retrieval[n]
			if row.qualityQuery != suite.Retrieval[n] || len(row.ResultIDs) > 10 {
				return r, errors.New("quality retrieval query changed")
			}
			row.Rank = 0
			for index, id := range row.ResultIDs {
				if id == row.TargetID {
					row.Rank = index + 1
					break
				}
			}
		}
		for n, out := range r.Outcomes {
			actual, _ := json.Marshal(out.Finding)
			expected, _ := json.Marshal(findings[n])
			if string(actual) != string(expected) {
				return r, errors.New("quality finding evidence changed")
			}
		}
		r.Quotes = quotes
		if err = summarizeQuality(&r); err != nil {
			return r, err
		}
	}
	return r, nil
}

func qualityHandler(i *inference) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		r, err := readQuality(i)
		if err != nil {
			fail(w, err)
			return
		}
		holdout, err := readQualitySuite(i, filepath.Join(filepath.Dir(i.budget.path), "research-quality-holdout.json"), qualityHoldout, 4, 2, 0)
		if err != nil {
			fail(w, err)
			return
		}
		generated, err := readGeneratedQuality(i)
		if err != nil {
			fail(w, err)
			return
		}
		generation, err := readGenerationAudit(i)
		if err != nil {
			fail(w, err)
			return
		}
		retrieval, err := readRetrieval(i)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, struct {
			qualityReport
			Holdout    qualityReport          `json:"holdout"`
			Generated  generatedQualityReport `json:"generated"`
			Generation generationAuditReport  `json:"generation"`
			Retrieval  retrievalReport        `json:"paired_retrieval"`
		}{r, holdout, generated, generation, retrieval})
	}
}
