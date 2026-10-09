package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const semanticAlgorithm = "bge-m3-cls-128-overlap16-weighted-mean-v1"
const semanticQueryContract = "complete-query-bgem3-cosine-fingerprints-v1"
const semanticDimensions = 1024

type SemanticCoverage struct {
	Algorithm     string         `json:"algorithm"`
	CatalogSHA    string         `json:"catalog_sha256"`
	ModelSHA      string         `json:"model_sha256"`
	Collection    string         `json:"collection"`
	Dimensions    int            `json:"dimensions"`
	Engine        map[string]any `json:"engine"`
	Indexed       int            `json:"indexed"`
	Total         int            `json:"total"`
	State         string         `json:"state"`
	Updated       string         `json:"updated_at"`
	VerifiedCount int            `json:"verified_vector_count,omitempty"`
	Stale         bool           `json:"stale,omitempty"`
}

type semanticClient struct {
	embeddingOrigin, milvusOrigin, statusPath string
	catalogSHA, modelSHA, collection          string
	diskPath                                  string
	diskInfo                                  os.FileInfo
	total                                     int
	client                                    *http.Client
}

var shaPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var semanticCategory = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,63}$`)

func semanticEngine() map[string]any {
	return map[string]any{"served_model_name": "bge-m3", "static_seq_len": 128, "pooling": "cls", "normalize_embeddings": true, "static_loader": true}
}

func semanticCollection(catalog, model string) string {
	b, _ := json.Marshal(map[string]any{"algorithm": semanticAlgorithm, "catalog_sha256": catalog, "model_sha256": model, "dimensions": semanticDimensions, "engine": semanticEngine()})
	return "research_arxiv_bgem3_" + textSHA(b)[:12]
}

func serviceOrigin(value string) (string, error) {
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", errors.New("semantic services require fixed HTTP origins")
	}
	return strings.TrimSuffix(value, "/"), nil
}

func newSemantic(i *inference) (*semanticClient, error) {
	embedding, milvus, status := os.Getenv("RESEARCH_EMBEDDING_ORIGIN"), os.Getenv("RESEARCH_VECTOR_ORIGIN"), os.Getenv("RESEARCH_SEMANTIC_STATUS")
	catalog, model := os.Getenv("RESEARCH_CATALOG_SHA256"), os.Getenv("RESEARCH_EMBEDDING_SHA256")
	if embedding == "" && milvus == "" && status == "" && catalog == "" && model == "" {
		return nil, nil
	}
	if !shaPattern.MatchString(catalog) || !shaPattern.MatchString(model) || !filepath.IsAbs(status) || i.db.path == ":memory:" {
		return nil, errors.New("semantic search requires a pinned catalog, embedding identity, and NAS status path")
	}
	s := &semanticClient{catalogSHA: catalog, modelSHA: model, statusPath: status, diskPath: i.db.path, collection: semanticCollection(catalog, model), client: i.client}
	var err error
	if s.embeddingOrigin, err = serviceOrigin(embedding); err != nil {
		return nil, err
	}
	if s.milvusOrigin, err = serviceOrigin(milvus); err != nil {
		return nil, err
	}
	// Hash the actual NAS catalog before any RAM copy. A read transaction keeps
	// the rollback-mode file stable; no source bytes are written to local disk.
	i.db.mu.Lock()
	defer i.db.mu.Unlock()
	if err = i.db.exec("BEGIN"); err != nil {
		return nil, err
	}
	defer i.db.exec("ROLLBACK")
	rows, err := i.db.rows("SELECT documents,missing_summaries FROM catalog_stats WHERE source='arxiv'")
	if err != nil || len(rows) != 1 || rows[0][1] != "0" {
		return nil, errors.New("semantic catalog lacks complete supplied abstracts")
	}
	if _, err = fmt.Sscan(rows[0][0], &s.total); err != nil || s.total < 1 {
		return nil, errors.New("semantic catalog count is invalid")
	}
	f, err := os.Open(s.diskPath)
	if err != nil {
		return nil, errors.New("semantic catalog snapshot unavailable")
	}
	defer f.Close()
	s.diskInfo, err = f.Stat()
	if err != nil || !s.diskInfo.Mode().IsRegular() || s.diskInfo.Size() > 16<<30 {
		return nil, errors.New("semantic catalog snapshot exceeds bounds")
	}
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return nil, errors.New("semantic catalog fingerprint read failed")
	}
	after, err := os.Stat(s.diskPath)
	if err != nil || !sameFileSnapshot(s.diskInfo, after) || hex.EncodeToString(h.Sum(nil)) != catalog {
		return nil, errors.New("semantic catalog fingerprint does not match the index")
	}
	return s, nil
}

func sameFileSnapshot(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime() == b.ModTime()
}

func (s *semanticClient) coverage() (SemanticCoverage, error) {
	var status SemanticCoverage
	if s == nil {
		return status, errors.New("meaning-based search is not configured")
	}
	if s.diskPath != "" {
		info, err := os.Stat(s.diskPath)
		if err != nil || !sameFileSnapshot(info, s.diskInfo) {
			return status, errors.New("catalog snapshot changed; rebuild or reopen the verified index")
		}
	}
	f, err := os.Open(s.statusPath)
	if err != nil {
		return status, errors.New("semantic index coverage is unavailable")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(b) > 65536 || json.Unmarshal(b, &status) != nil {
		return status, errors.New("semantic coverage receipt is invalid")
	}
	expected, _ := json.Marshal(semanticEngine())
	observed, _ := json.Marshal(status.Engine)
	if status.Algorithm != semanticAlgorithm || status.CatalogSHA != s.catalogSHA || status.ModelSHA != s.modelSHA || status.Collection != s.collection || status.Dimensions != semanticDimensions || string(expected) != string(observed) || status.Total != s.total || status.Indexed < 0 || status.Indexed > status.Total {
		return status, errors.New("semantic index identity or coverage changed")
	}
	switch status.State {
	case "indexing", "limited", "interrupted", "failed", "complete":
	default:
		return status, errors.New("semantic index state is invalid")
	}
	if status.State == "complete" && (status.Indexed != status.Total || status.VerifiedCount != status.Total) {
		return status, errors.New("complete semantic coverage is unverified")
	}
	updated, err := time.Parse(time.RFC3339, status.Updated)
	if err != nil || updated.After(time.Now().Add(time.Minute)) {
		return status, errors.New("semantic coverage time is invalid")
	}
	status.Stale = status.State == "indexing" && time.Since(updated) > 2*time.Minute
	return status, nil
}

// Service addresses and credentials never come from a browser request. This
// client deliberately does not forward the chat model's authorization key.
func (s *semanticClient) call(ctx context.Context, origin, path string, body any, out any) error {
	var data []byte
	var err error
	method := "GET"
	if body != nil {
		data, err = json.Marshal(body)
		method = "POST"
	}
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, origin+path, bytes.NewReader(data))
	if err != nil {
		return errors.New("semantic service request is invalid")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Request-Timeout", "30")
	r, err := s.client.Do(req)
	if err != nil {
		return errors.New("semantic service request failed or exceeded its deadline")
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return fmt.Errorf("semantic service returned HTTP %d", r.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20+1))
	if err != nil || len(b) > 1<<20 {
		return errors.New("semantic service response exceeds bounds")
	}
	if json.Unmarshal(b, out) != nil {
		return errors.New("semantic service response is invalid")
	}
	return nil
}

func (s *semanticClient) health(ctx context.Context) error {
	var actual map[string]any
	if err := s.call(ctx, s.embeddingOrigin, "/health", nil, &actual); err != nil {
		return err
	}
	for key, value := range semanticEngine() {
		a, _ := json.Marshal(actual[key])
		b, _ := json.Marshal(value)
		if string(a) != string(b) {
			return errors.New("embedding engine changed its exact indexing contract")
		}
	}
	return nil
}

type queryWindow struct {
	text   string
	weight int
}

func exactQueryWindows(query string, inputSHA string, maxTokens, special int, offsets [][]int) ([]queryWindow, error) {
	chars := []rune(query)
	if inputSHA != textSHA([]byte(query)) || maxTokens != 128 || special != 2 || len(offsets) == 0 || len(offsets) > 4096 {
		return nil, errors.New("query tokenizer identity or bounds changed")
	}
	prevStart, prevEnd := 0, 0
	for _, pair := range offsets {
		if len(pair) != 2 || pair[0] < prevStart || pair[1] < prevEnd || pair[0] < 0 || pair[1] < pair[0] || pair[1] > len(chars) {
			return nil, errors.New("query token offsets are invalid")
		}
		prevStart, prevEnd = pair[0], pair[1]
	}
	windows := []queryWindow{}
	covered := 0
	for start := 0; start < len(offsets); start += 96 {
		end := min(start+112, len(offsets))
		text := string(chars[offsets[start][0]:offsets[end-1][1]])
		if text == "" {
			return nil, errors.New("query has an empty token window")
		}
		windows = append(windows, queryWindow{text, end - covered})
		covered = end
		if end == len(offsets) {
			break
		}
	}
	if len(windows) > 8 || covered != len(offsets) {
		return nil, errors.New("complete query exceeds bounded embedding windows")
	}
	return windows, nil
}

func validSemanticVector(v []float64) bool {
	if len(v) != semanticDimensions {
		return false
	}
	norm := 0.0
	for _, x := range v {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return false
		}
		norm += x * x
	}
	return math.Abs(norm-1) < .002
}

func (i *inference) embedQuery(ctx context.Context, query string) ([]float64, int, error) {
	s := i.semantic
	select {
	case i.gate <- struct{}{}:
		defer func() { <-i.gate }()
	default:
		return nil, 0, errors.New("model is busy; retry later")
	}
	var tokens struct {
		SHA     string  `json:"input_sha256"`
		Max     int     `json:"max_input_tokens"`
		Special int     `json:"special_tokens"`
		Offsets [][]int `json:"offsets"`
	}
	if err := s.call(ctx, s.embeddingOrigin, "/tokenize", map[string]any{"model": "bge-m3", "input": query}, &tokens); err != nil {
		return nil, 0, err
	}
	windows, err := exactQueryWindows(query, tokens.SHA, tokens.Max, tokens.Special, tokens.Offsets)
	if err != nil {
		return nil, 0, err
	}
	texts := []string{}
	for _, w := range windows {
		texts = append(texts, w.text)
	}
	body := map[string]any{"model": "bge-m3", "input": texts, "truncate": false}
	serialized, _ := json.Marshal(body)
	units := len(serialized)
	if err = i.reserve("abstract-embedding", units); err != nil {
		return nil, 0, err
	}
	var out struct {
		Model    string `json:"model"`
		Contract struct {
			Truncated *bool  `json:"truncated"`
			Pool      string `json:"pooling"`
			Max       int    `json:"max_input_tokens"`
			Counts    []int  `json:"input_tokens"`
		} `json:"input_contract"`
		Data []struct {
			Index  int       `json:"index"`
			Vector []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err = s.call(ctx, s.embeddingOrigin, "/embeddings", body, &out); err != nil {
		return nil, units, err
	}
	if out.Model != "bge-m3" || out.Contract.Truncated == nil || *out.Contract.Truncated || out.Contract.Pool != "cls" || out.Contract.Max != 128 || len(out.Contract.Counts) != len(windows) || len(out.Data) != len(windows) {
		return nil, units, errors.New("query embedding was truncated or changed its contract")
	}
	mean := make([]float64, semanticDimensions)
	seen := map[int]bool{}
	total := 0
	for _, w := range windows {
		total += w.weight
	}
	for _, row := range out.Data {
		if row.Index < 0 || row.Index >= len(windows) || seen[row.Index] || !validSemanticVector(row.Vector) || out.Contract.Counts[row.Index] < 1 || out.Contract.Counts[row.Index] > 128 {
			return nil, units, errors.New("query embedding dimensions or normalization changed")
		}
		seen[row.Index] = true
		for d, x := range row.Vector {
			mean[d] += x * float64(windows[row.Index].weight) / float64(total)
		}
	}
	norm := 0.0
	for _, x := range mean {
		norm += x * x
	}
	norm = math.Sqrt(norm)
	if norm < 1e-8 || math.IsInf(norm, 0) || math.IsNaN(norm) {
		return nil, units, errors.New("query embedding has no usable direction")
	}
	for n := range mean {
		mean[n] /= norm
	}
	return mean, units, nil
}

func semanticFilter(q SearchRequest) (string, error) {
	if q.Source == "gutenberg" || q.Language != "" {
		return "", errors.New("meaning-based search supports supplied arXiv abstracts")
	}
	parts := []string{}
	quote := func(v string) string { b, _ := json.Marshal(v); return string(b) }
	if q.From != "" {
		parts = append(parts, "published >= "+quote(q.From))
	}
	if q.To != "" {
		parts = append(parts, "published <= "+quote(q.To))
	}
	if q.Category != "" {
		if !semanticCategory.MatchString(q.Category) {
			return "", errors.New("meaning search requires one exact arXiv category")
		}
		parts = append(parts, "categories like "+quote("% "+q.Category+" %"))
	}
	return strings.Join(parts, " and "), nil
}

func (s *researchDesk) validateSemanticCache(encoded string, q SearchRequest) error {
	var result SearchResponse
	limit := q.Limit
	if limit == 0 {
		limit = 10
	}
	if json.Unmarshal([]byte(encoded), &result) != nil || result.Coverage == nil || result.Coverage.State != "complete" || result.Query != strings.TrimSpace(q.Query) || len(result.Documents) > limit {
		return errors.New("saved semantic result failed verification")
	}
	coverage, err := s.i.semantic.coverage()
	if err != nil {
		return err
	}
	if coverage.State != "complete" || result.Coverage.Collection != coverage.Collection || result.Coverage.CatalogSHA != coverage.CatalogSHA || result.Coverage.ModelSHA != coverage.ModelSHA || result.Coverage.VerifiedCount != coverage.Total {
		return errors.New("saved semantic result uses an older index contract")
	}
	ids := []string{}
	for _, d := range result.Documents {
		ids = append(ids, d.ID)
	}
	for begin := 0; begin < len(ids); begin += 8 {
		docs, err := s.i.db.getDocuments(ids[begin:min(begin+8, len(ids))])
		if err != nil {
			return err
		}
		for n, doc := range docs {
			if doc.Fingerprint != result.Documents[begin+n].Fingerprint {
				return errors.New("saved semantic source changed; refresh the verified index")
			}
		}
	}
	return nil
}

func (s *researchDesk) semanticSearchJob(ctx context.Context, owner string, q SearchRequest, progress func(string)) (SearchResponse, error) {
	start := time.Now()
	result := SearchResponse{Documents: []Document{}, Method: "abstract-vector-cosine", Query: q.Query, EvidenceLimit: "Meaning search ranks abstract similarity; a high score does not establish a paper's claim or a scientific relationship."}
	if err := q.validate(); err != nil {
		return result, err
	}
	result.Query = q.Query
	filter, err := semanticFilter(q)
	if err != nil {
		return result, err
	}
	if _, err := canonicalID("arxiv", q.Query); err == nil || doiID.MatchString(q.Query) {
		lookup := q
		lookup.Source = "arxiv"
		result, err = s.i.db.search(lookup)
		result.Query = q.Query
		result.Documents = browserDocuments(result.Documents, s.archive != nil)
		return result, err
	}
	coverage, err := s.i.semantic.coverage()
	if err != nil {
		return result, err
	}
	result.Coverage = &coverage
	if coverage.Indexed == 0 {
		return result, errors.New("the semantic index has no verified abstracts yet")
	}
	progress("Checking embedding identity and index coverage")
	if err = s.i.semantic.health(ctx); err != nil {
		return result, err
	}
	b, _ := json.Marshal([]any{semanticQueryContract, semanticAlgorithm, s.i.semantic.modelSHA, s.i.semantic.embeddingOrigin, q.Query})
	key := textSHA(b)
	s.db.mu.Lock()
	rows, err := s.db.rows("SELECT vector FROM desk_embeddings WHERE owner=? AND key=?", owner, key)
	s.db.mu.Unlock()
	if err != nil {
		return result, err
	}
	var vector []float64
	if len(rows) == 1 {
		if json.Unmarshal([]byte(rows[0][0]), &vector) != nil || !validSemanticVector(vector) {
			return result, errors.New("saved query embedding failed verification")
		}
		result.EmbeddingCached = true
		progress("Reusing a verified query embedding; no model call")
	} else {
		progress("Embedding the complete abstract-search query")
		var units int
		vector, units, err = s.i.embedQuery(ctx, q.Query)
		if units > 0 {
			result.ModelCalls, result.ReservedUnits = 1, units
		}
		if err != nil {
			return result, err
		}
		result.ModelCalls, result.ReservedUnits = 1, units
		encoded, _ := json.Marshal(vector)
		s.db.mu.Lock()
		_, err = s.db.run("INSERT OR REPLACE INTO desk_embeddings VALUES(?,?,?,?)", owner, key, deskTime(), string(encoded))
		if err == nil {
			_, err = s.db.run("DELETE FROM desk_embeddings WHERE owner=? AND key NOT IN (SELECT key FROM desk_embeddings WHERE owner=? ORDER BY updated DESC LIMIT 64)", owner, owner)
		}
		s.db.mu.Unlock()
		if err != nil {
			return result, err
		}
	}
	progress("Searching verified abstract vectors")
	var hits struct {
		Code *int `json:"code"`
		Data []struct {
			ID          string  `json:"id"`
			Fingerprint string  `json:"fingerprint"`
			Score       float64 `json:"distance"`
		} `json:"data"`
	}
	body := map[string]any{"collectionName": coverage.Collection, "data": [][]float64{vector}, "annsField": "embedding", "filter": filter, "limit": q.Limit, "outputFields": []string{"id", "fingerprint"}, "searchParams": map[string]any{"metricType": "COSINE", "params": map[string]any{"nprobe": 32}}, "consistencyLevel": "Strong"}
	if err = s.i.semantic.call(ctx, s.i.semantic.milvusOrigin, "/v2/vectordb/entities/search", body, &hits); err != nil {
		return result, err
	}
	if hits.Code == nil || *hits.Code != 0 || hits.Data == nil || len(hits.Data) > q.Limit {
		return result, errors.New("vector service rejected the bounded abstract search")
	}
	ids := []string{}
	seen := map[string]bool{}
	for _, hit := range hits.Data {
		id, e := canonicalID("arxiv", hit.ID)
		if e != nil || id != hit.ID || seen[id] || !shaPattern.MatchString(hit.Fingerprint) || math.IsNaN(hit.Score) || math.IsInf(hit.Score, 0) || hit.Score < -1.002 || hit.Score > 1.002 {
			return result, errors.New("vector search has invalid identifiers or scores")
		}
		seen[id] = true
		ids = append(ids, id)
	}
	progress("Verifying every result against its saved abstract")
	for begin := 0; begin < len(ids); begin += 8 {
		docs, e := s.i.db.getDocuments(ids[begin:min(begin+8, len(ids))])
		if e != nil {
			return result, e
		}
		for n, doc := range docs {
			hit := hits.Data[begin+n]
			if doc.Fingerprint != hit.Fingerprint || doc.Summary == "" || doc.Source != "arxiv" {
				return result, errors.New("vector result does not match the saved abstract fingerprint")
			}
			if (q.From != "" && doc.Published < q.From) || (q.To != "" && doc.Published > q.To) || (q.Category != "" && !strings.Contains(" "+doc.Categories+" ", " "+q.Category+" ")) {
				return result, errors.New("vector result does not match the requested source filters")
			}
			doc.Score = hit.Score
			result.Documents = append(result.Documents, doc)
		}
	}
	// Indexing may advance during a query. Preserve the conservative coverage
	// observed when this search started, rather than claim a later full corpus.
	if _, err = s.i.semantic.coverage(); err != nil {
		return result, err
	}
	result.Documents = browserDocuments(result.Documents, s.archive != nil)
	result.ElapsedMS = time.Since(start).Milliseconds()
	return result, nil
}
