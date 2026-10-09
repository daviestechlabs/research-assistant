package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
)

func indexEmbedParts(ctx context.Context, s *semanticClient, parts []string) ([]float64, error) {
	var windows []queryWindow
	for _, part := range parts {
		var tokens struct {
			SHA     string  `json:"input_sha256"`
			Max     int     `json:"max_input_tokens"`
			Special int     `json:"special_tokens"`
			Offsets [][]int `json:"offsets"`
		}
		if e := s.call(ctx, s.embeddingOrigin, "/tokenize", map[string]any{"model": "bge-m3", "input": part}, &tokens); e != nil {
			return nil, e
		}
		w, e := exactQueryWindows(part, tokens.SHA, tokens.Max, tokens.Special, tokens.Offsets)
		if e != nil {
			return nil, e
		}
		windows = append(windows, w...)
	}
	if len(windows) == 0 {
		return nil, errors.New("abstract has no embedding windows")
	}
	mean := make([]float64, semanticDimensions)
	total := 0
	for _, w := range windows {
		total += w.weight
	}
	for start := 0; start < len(windows); start += 16 {
		end := start + 16
		if end > len(windows) {
			end = len(windows)
		}
		batch := windows[start:end]
		texts := []string{}
		for _, w := range batch {
			texts = append(texts, w.text)
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
		if e := s.call(ctx, s.embeddingOrigin, "/embeddings", map[string]any{"model": "bge-m3", "input": texts, "truncate": false}, &out); e != nil {
			return nil, e
		}
		if out.Model != "bge-m3" || out.Contract.Truncated == nil || *out.Contract.Truncated || out.Contract.Pool != "cls" || out.Contract.Max != 128 || len(out.Contract.Counts) != len(batch) || len(out.Data) != len(batch) {
			return nil, errors.New("abstract embedding changed or truncated its contract")
		}
		seen := map[int]bool{}
		for _, r := range out.Data {
			if r.Index < 0 || r.Index >= len(batch) || seen[r.Index] || !validSemanticVector(r.Vector) || out.Contract.Counts[r.Index] < 1 || out.Contract.Counts[r.Index] > 128 {
				return nil, errors.New("abstract vector failed its exact contract")
			}
			seen[r.Index] = true
			for d, x := range r.Vector {
				mean[d] += x * float64(batch[r.Index].weight) / float64(total)
			}
		}
	}
	norm := 0.0
	for _, x := range mean {
		norm += x * x
	}
	norm = math.Sqrt(norm)
	if norm < 1e-8 || math.IsNaN(norm) || math.IsInf(norm, 0) {
		return nil, errors.New("abstract embedding has no direction")
	}
	for d := range mean {
		mean[d] /= norm
	}
	return mean, nil
}

// Reuse a vector only when its model, algorithm, source snapshot, document ID,
// and content fingerprint match. Missing or changed records get new embeddings.
func indexReusableVectors(ctx context.Context, s *semanticClient, base indexGeneration, batch [][]string) (map[string][]float64, error) {
	out := map[string][]float64{}
	if base.Semantic == "" || base.ModelSHA != s.modelSHA || !shaPattern.MatchString(base.SHA) {
		return out, nil
	}
	var coverage SemanticCoverage
	if readIndexJSON(base.Semantic, &coverage) != nil {
		return out, nil
	}
	if coverage.Algorithm != semanticAlgorithm || coverage.CatalogSHA != base.SHA || coverage.ModelSHA != s.modelSHA || coverage.Dimensions != semanticDimensions || coverage.Collection != semanticCollection(base.SHA, s.modelSHA) {
		return out, nil
	}
	expected, _ := json.Marshal(semanticEngine())
	actual, _ := json.Marshal(coverage.Engine)
	if string(actual) != string(expected) {
		return out, nil
	}
	ids := []string{}
	fingerprints := map[string]string{}
	for _, r := range batch {
		ids = append(ids, r[1])
		fingerprints[r[1]] = r[3]
	}
	encoded, _ := json.Marshal(ids)
	var response struct {
		Code int `json:"code"`
		Data []struct {
			ID          string    `json:"id"`
			Fingerprint string    `json:"fingerprint"`
			Vector      []float64 `json:"embedding"`
		} `json:"data"`
	}
	e := s.call(ctx, s.milvusOrigin, "/v2/vectordb/entities/query", map[string]any{"collectionName": coverage.Collection, "filter": "id in " + string(encoded), "outputFields": []string{"id", "fingerprint", "embedding"}, "consistencyLevel": "Strong", "limit": len(batch)}, &response)
	if e != nil {
		return nil, e
	}
	if response.Code != 0 {
		return nil, errors.New("source vector query failed")
	}
	for _, r := range response.Data {
		if fingerprints[r.ID] == r.Fingerprint && validSemanticVector(r.Vector) {
			if _, duplicate := out[r.ID]; duplicate {
				return nil, errors.New("source vector IDs are duplicated")
			}
			out[r.ID] = r.Vector
		}
	}
	return out, nil
}
