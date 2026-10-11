package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const retrievalContract = "paired-full-abstract-retrieval-keyword-fallback-v2"

type retrievalHit struct {
	ID          string `json:"id"`
	Fingerprint string `json:"fingerprint"`
}
type retrievalPair struct {
	qualityQuery
	Keyword        []retrievalHit `json:"keyword_results"`
	Meaning        []retrievalHit `json:"meaning_results"`
	KeywordRank    int            `json:"keyword_rank_at_10"`
	MeaningRank    int            `json:"meaning_rank_at_10"`
	KeywordMS      int64          `json:"keyword_elapsed_ms"`
	MeaningMS      int64          `json:"meaning_elapsed_ms"`
	EmbeddingCalls int            `json:"embedding_calls"`
	ReservedUnits  int            `json:"reserved_units"`
}
type retrievalAttempt struct {
	CaseID         string `json:"case_id"`
	Outcome        string `json:"outcome"`
	EmbeddingCalls int    `json:"embedding_calls"`
	ReservedUnits  int    `json:"reserved_units"`
}
type retrievalGroup struct {
	Kind        string  `json:"kind"`
	Queries     int     `json:"queries"`
	KeywordHits int     `json:"keyword_hits_at_10"`
	MeaningHits int     `json:"meaning_hits_at_10"`
	KeywordMRR  float64 `json:"keyword_mrr_at_10"`
	MeaningMRR  float64 `json:"meaning_mrr_at_10"`
}
type retrievalReport struct {
	Status         string             `json:"status"`
	Contract       string             `json:"contract"`
	SuiteSHA       string             `json:"suite_sha256"`
	QueryContract  string             `json:"query_contract"`
	Started        string             `json:"started_at,omitempty"`
	Completed      string             `json:"completed_at,omitempty"`
	Coverage       *SemanticCoverage  `json:"coverage,omitempty"`
	Sources        []qualitySource    `json:"sources,omitempty"`
	Cases          []retrievalPair    `json:"cases"`
	Attempts       []retrievalAttempt `json:"embedding_attempts"`
	Groups         []retrievalGroup   `json:"groups"`
	EmbeddingCalls int                `json:"embedding_calls"`
	ReservedUnits  int                `json:"reserved_units"`
	Failure        string             `json:"failure,omitempty"`
	Limits         []string           `json:"limits"`
}

func retrievalLimits() []string {
	return []string{
		"Both methods search the same complete supplied arXiv abstract corpus, with the same unfiltered queries and ten-result limit.",
		"The eight reused development queries have one curated target each. Target rank is not a judgment of every returned paper's relevance.",
		"Keyword retrieval keeps strict matches, then removes filler and requires two topic terms when a longer query has no matches.",
		"Four keyword queries and four paraphrase queries are reported separately. These measurements do not estimate corpus-wide accuracy or independent held-out performance.",
		"Meaning search uses the application's complete query windows, cosine vector search, and saved abstract fingerprint checks. No reranker, query expansion, PDF reading, or chat model is used.",
		"The verified embedding artifact fingerprint binds this index contract. This report does not prove the weights loaded by every future engine process.",
		"Opening saved results makes no model call. Partial or stale reports provide no complete benchmark score.",
		"Reservation counts include observed failed requests. A stopped process can leave an uncertain in-flight request; the shared usage ledger remains the cost authority.",
	}
}
func retrievalPath(i *inference) string {
	return filepath.Join(filepath.Dir(i.budget.path), "research-retrieval.json")
}
func (i *inference) completeRetrievalCoverage() (SemanticCoverage, error) {
	c, err := i.semantic.coverage()
	if err != nil {
		return c, err
	}
	if c.State != "complete" || c.Indexed != c.Total || c.VerifiedCount != c.Total || c.Total < 1 {
		return c, errors.New("paired retrieval requires independently verified complete abstract coverage")
	}
	return c, nil
}
func sameRetrievalCoverage(a, b SemanticCoverage) bool {
	return a.CatalogSHA == b.CatalogSHA && a.ModelSHA == b.ModelSHA && a.Collection == b.Collection && a.Total == b.Total && a.Algorithm == b.Algorithm && a.Dimensions == b.Dimensions && a.State == "complete" && b.State == "complete" && a.Indexed == a.Total && b.Indexed == b.Total && a.VerifiedCount == a.Total && b.VerifiedCount == b.Total
}
func verifyRetrievalSources(d *database, suite qualitySuite) error {
	ids := []string{}
	for _, s := range suite.Sources {
		ids = append(ids, s.ID)
	}
	docs, err := d.getDocuments(ids)
	if err != nil {
		return err
	}
	for n, doc := range docs {
		if doc.Fingerprint != suite.Sources[n].Fingerprint || doc.Source != "arxiv" || doc.Summary == "" {
			return errors.New("retrieval target source changed")
		}
	}
	return nil
}
func retrievalHits(docs []Document) []retrievalHit {
	hits := []retrievalHit{}
	for _, d := range docs {
		hits = append(hits, retrievalHit{d.ID, d.Fingerprint})
	}
	return hits
}
func rankRetrieval(hits []retrievalHit, target string) (int, error) {
	if len(hits) > 10 {
		return 0, errors.New("retrieval result exceeds ten papers")
	}
	rank := 0
	seen := map[string]bool{}
	for n, h := range hits {
		id, err := canonicalID("arxiv", h.ID)
		if err != nil || id != h.ID || seen[id] || !shaPattern.MatchString(h.Fingerprint) {
			return 0, errors.New("retrieval result has invalid sources")
		}
		seen[id] = true
		if id == target {
			rank = n + 1
		}
	}
	return rank, nil
}
func summarizeRetrieval(r *retrievalReport, suite qualitySuite) error {
	if len(r.Cases) > len(suite.Retrieval) || (r.Status == "complete" && len(r.Cases) != len(suite.Retrieval)) {
		return errors.New("paired retrieval cases are incomplete")
	}
	groups := map[string]*retrievalGroup{"keyword": {Kind: "keyword"}, "synonym": {Kind: "synonym"}}
	r.EmbeddingCalls, r.ReservedUnits = 0, 0
	if len(r.Attempts) > len(suite.Retrieval) || len(r.Cases) > len(r.Attempts) || (r.Status == "complete" && len(r.Attempts) != len(suite.Retrieval)) {
		return errors.New("retrieval attempts are incomplete")
	}
	completed := 0
	for n, a := range r.Attempts {
		if a.CaseID != suite.Retrieval[n].ID || a.EmbeddingCalls < 0 || a.EmbeddingCalls > 1 || a.ReservedUnits < 0 || a.ReservedUnits > 1<<20 || (a.EmbeddingCalls == 0) != (a.ReservedUnits == 0) {
			return errors.New("retrieval attempt contract changed")
		}
		if a.Outcome == "completed" {
			if a.EmbeddingCalls != 1 || n >= len(r.Cases) || r.Cases[n].EmbeddingCalls != a.EmbeddingCalls || r.Cases[n].ReservedUnits != a.ReservedUnits {
				return errors.New("retrieval completed attempt differs from its pair")
			}
			completed++
		} else if (a.Outcome != "running" && a.Outcome != "failed") || n != len(r.Attempts)-1 || r.Status == "complete" {
			return errors.New("retrieval attempt order changed")
		}
		r.EmbeddingCalls += a.EmbeddingCalls
		r.ReservedUnits += a.ReservedUnits
	}
	if completed != len(r.Cases) {
		return errors.New("retrieval pair lacks a completed attempt")
	}

	for n := range r.Cases {
		row := &r.Cases[n]
		if row.qualityQuery != suite.Retrieval[n] || groups[row.Kind] == nil || row.EmbeddingCalls != 1 || row.ReservedUnits < 1 || row.ReservedUnits > 1<<20 || row.KeywordMS < 0 || row.MeaningMS < 0 || row.KeywordMS > 600000 || row.MeaningMS > 600000 {
			return errors.New("paired retrieval case contract changed")
		}
		var err error
		if row.KeywordRank, err = rankRetrieval(row.Keyword, row.TargetID); err != nil {
			return err
		}
		if row.MeaningRank, err = rankRetrieval(row.Meaning, row.TargetID); err != nil {
			return err
		}
		g := groups[row.Kind]
		g.Queries++
		if row.KeywordRank > 0 {
			g.KeywordHits++
			g.KeywordMRR += 1 / float64(row.KeywordRank)
		}
		if row.MeaningRank > 0 {
			g.MeaningHits++
			g.MeaningMRR += 1 / float64(row.MeaningRank)
		}
	}
	r.Groups = []retrievalGroup{}
	for _, kind := range []string{"keyword", "synonym"} {
		g := groups[kind]
		if g.Queries > 0 {
			g.KeywordMRR /= float64(g.Queries)
			g.MeaningMRR /= float64(g.Queries)
		}
		r.Groups = append(r.Groups, *g)
	}
	r.Limits = retrievalLimits()
	return nil
}

// Use a new operator desk. Both rankings use the production search functions.
// Incomplete coverage rejects before file creation or any embedding request.
func (i *inference) benchmarkRetrieval(ctx context.Context, path, desk string) (r retrievalReport, err error) {
	r = retrievalReport{Status: "running", Contract: retrievalContract, SuiteSHA: textSHA(qualityCases), QueryContract: semanticQueryContract, Started: deskTime(), Cases: []retrievalPair{}, Attempts: []retrievalAttempt{}, Groups: []retrievalGroup{}, Limits: retrievalLimits()}
	if !filepath.IsAbs(path) || !filepath.IsAbs(desk) || filepath.Clean(desk) == filepath.Clean(deskPath(i)) || filepath.Clean(desk) == filepath.Clean(path) {
		return r, errors.New("retrieval benchmark requires absolute new report and separate desk paths")
	}
	coverage, err := i.completeRetrievalCoverage()
	if err != nil {
		return r, err
	}
	suite, err := loadQualitySuite()
	if err != nil {
		return r, err
	}
	if err = verifyRetrievalSources(i.db, suite); err != nil {
		return r, err
	}
	r.Coverage = &coverage
	r.Sources = suite.Sources
	// Existing artifacts are never overwritten by an operator benchmark retry.
	for _, p := range []string{path, desk} {
		if _, e := os.Lstat(p); !os.IsNotExist(e) {
			return r, errors.New("retrieval report and verification desk must be new")
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return r, err
	}
	if err = f.Close(); err != nil {
		return r, err
	}
	defer func() {
		if err != nil {
			r.Status = "failed"
			if len(r.Attempts) > 0 && r.Attempts[len(r.Attempts)-1].Outcome == "running" {
				r.Attempts[len(r.Attempts)-1].Outcome = "failed"
			}
			r.Failure = "Measurement stopped; only the recorded completed query pairs were measured."
			_ = summarizeRetrieval(&r, suite)
			if e := publishQuality(path, r); e != nil {
				err = errors.Join(err, e)
			}
		}
	}()
	if err = publishQuality(path, r); err != nil {
		return r, err
	}
	f, err = os.OpenFile(desk, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return r, err
	}
	if err = f.Close(); err != nil {
		return r, err
	}
	s, err := openResearchDesk(desk, i, nil)
	if err != nil {
		return r, err
	}
	defer s.close()
	for _, test := range suite.Retrieval {
		if err = ctx.Err(); err != nil {
			return r, err
		}
		current, e := i.completeRetrievalCoverage()
		if e != nil || !sameRetrievalCoverage(coverage, current) {
			return r, errors.New("retrieval corpus changed during measurement")
		}
		q := SearchRequest{Query: test.Query, Source: "arxiv", Limit: 10}
		keyword, e := i.db.search(q)
		if e != nil {
			return r, e
		}
		r.Attempts = append(r.Attempts, retrievalAttempt{CaseID: test.ID, Outcome: "running"})
		if err = publishQuality(path, r); err != nil {
			return r, err
		}
		meaning, e := s.semanticSearchJob(ctx, "operator-retrieval-benchmark", q, func(string) {})
		attempt := &r.Attempts[len(r.Attempts)-1]
		attempt.EmbeddingCalls = meaning.ModelCalls
		attempt.ReservedUnits = meaning.ReservedUnits
		if e != nil {
			return r, e
		}
		if meaning.Coverage == nil || !sameRetrievalCoverage(coverage, *meaning.Coverage) || meaning.EmbeddingCached || meaning.ModelCalls != 1 {
			return r, errors.New("retrieval measurement did not use the complete verified corpus and a fresh query embedding")
		}
		current, e = i.completeRetrievalCoverage()
		if e != nil || !sameRetrievalCoverage(coverage, current) {
			return r, errors.New("retrieval corpus changed during measurement")
		}
		attempt.Outcome = "completed"
		r.Cases = append(r.Cases, retrievalPair{qualityQuery: test, Keyword: retrievalHits(keyword.Documents), Meaning: retrievalHits(meaning.Documents), KeywordMS: keyword.ElapsedMS, MeaningMS: meaning.ElapsedMS, EmbeddingCalls: meaning.ModelCalls, ReservedUnits: meaning.ReservedUnits})
		if err = summarizeRetrieval(&r, suite); err != nil {
			return r, err
		}
		if err = publishQuality(path, r); err != nil {
			return r, err
		}
	}
	if err = verifyRetrievalSources(i.db, suite); err != nil {
		return r, err
	}
	r.Status = "complete"
	r.Completed = deskTime()
	if err = summarizeRetrieval(&r, suite); err != nil {
		return r, err
	}
	err = publishQuality(path, r)
	return r, err
}

func readRetrieval(i *inference) (retrievalReport, error) {
	r := retrievalReport{Status: "missing", Contract: retrievalContract, Cases: []retrievalPair{}, Attempts: []retrievalAttempt{}, Groups: []retrievalGroup{}, Limits: retrievalLimits()}
	f, err := os.Open(retrievalPath(i))
	if os.IsNotExist(err) {
		return r, nil
	}
	if err != nil {
		return r, errors.New("paired retrieval receipt is unavailable")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil || len(raw) > 1<<20 {
		return r, errors.New("paired retrieval receipt exceeds bounds")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&r) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return r, errors.New("paired retrieval receipt is invalid")
	}
	if r.Status != "running" && r.Status != "failed" && r.Status != "complete" {
		return r, errors.New("paired retrieval status is invalid")
	}
	started, stampErr := time.Parse(time.RFC3339Nano, r.Started)
	if stampErr != nil || started.After(time.Now().Add(time.Minute)) {
		return r, errors.New("paired retrieval start time is invalid")
	}
	if r.Status == "complete" {
		completed, e := time.Parse(time.RFC3339Nano, r.Completed)
		if e != nil || completed.Before(started) || completed.After(time.Now().Add(time.Minute)) {
			return r, errors.New("paired retrieval completion time is invalid")
		}
	}
	suite, err := loadQualitySuite()
	if err != nil {
		return r, err
	}
	current, err := i.completeRetrievalCoverage()
	if err != nil || r.Coverage == nil || !sameRetrievalCoverage(*r.Coverage, current) || r.Contract != retrievalContract || r.SuiteSHA != textSHA(qualityCases) || r.QueryContract != semanticQueryContract || verifyRetrievalSources(i.db, suite) != nil {
		return retrievalReport{Status: "stale", Contract: retrievalContract, Cases: []retrievalPair{}, Attempts: []retrievalAttempt{}, Groups: []retrievalGroup{}, Limits: retrievalLimits()}, nil
	}
	expected, _ := json.Marshal(suite.Sources)
	observed, _ := json.Marshal(r.Sources)
	if string(expected) != string(observed) {
		return r, errors.New("paired retrieval sources changed")
	}
	if r.Status == "complete" && r.Completed == "" {
		return r, errors.New("paired retrieval lacks completion time")
	}
	if r.Status == "failed" {
		r.Failure = "Measurement stopped; only the recorded completed query pairs were measured."
	}
	// Verify every recorded result against the saved corpus before showing ranks.
	for _, row := range r.Cases {
		for _, hits := range [][]retrievalHit{row.Keyword, row.Meaning} {
			if len(hits) > 10 {
				return r, errors.New("paired retrieval results exceed bounds")
			}
			for begin := 0; begin < len(hits); begin += 8 {
				ids := []string{}
				end := min(begin+8, len(hits))
				for _, hit := range hits[begin:end] {
					ids = append(ids, hit.ID)
				}
				docs, e := i.db.getDocuments(ids)
				if e != nil {
					return r, errors.New("paired retrieval result sources are unavailable")
				}
				for n, doc := range docs {
					if doc.Fingerprint != hits[begin+n].Fingerprint || doc.Source != "arxiv" || doc.Summary == "" {
						return r, errors.New("paired retrieval result source changed")
					}
				}
			}
		}
	}
	if err = summarizeRetrieval(&r, suite); err != nil {
		return r, err
	}
	return r, nil
}
