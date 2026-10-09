package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Only a source-checked operator receipt approved with this release is shown.
// This approval contains hashes, not NAS object paths or cached book text.
//
//go:embed quality_generated.json
var generatedQualityApproval []byte

type generatedQualityReport struct {
	Contract   string            `json:"contract"`
	Status     string            `json:"status"`
	AuditedAt  string            `json:"audited_at"`
	SuiteSHA   string            `json:"suite_sha256"`
	Annotation string            `json:"annotation"`
	ModelCalls int               `json:"model_calls"`
	Counts     json.RawMessage   `json:"counts"`
	Cases      []json.RawMessage `json:"cases"`
	Limits     []string          `json:"limits"`
}

func readGeneratedQuality(i *inference) (generatedQualityReport, error) {
	r := generatedQualityReport{Status: "missing"}
	var approval struct {
		Contract string          `json:"contract"`
		SHA      string          `json:"receipt_sha256"`
		Sources  []qualitySource `json:"sources"`
	}
	if json.Unmarshal(generatedQualityApproval, &approval) != nil || approval.Contract != "generated-output-audit-v1" || !shaPattern.MatchString(approval.SHA) || len(approval.Sources) < 1 || len(approval.Sources) > 8 {
		return r, errors.New("generated output audit approval is invalid")
	}
	f, err := os.Open(filepath.Join(filepath.Dir(i.budget.path), "research-generated-quality.json"))
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, errors.New("generated output audit unavailable")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 1024*1024+1))
	if err != nil || len(b) > 1024*1024 || textSHA(b) != approval.SHA {
		return r, errors.New("generated output audit does not match the approved receipt")
	}
	decoder := json.NewDecoder(strings.NewReader(string(b)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&r) != nil || decoder.Decode(&struct{}{}) != io.EOF || r.Contract != approval.Contract || r.Status != "complete" || r.ModelCalls != 0 || len(r.Cases) != 8 {
		return r, errors.New("generated output audit is invalid")
	}
	ids := []string{}
	for _, source := range approval.Sources {
		ids = append(ids, source.ID)
	}
	docs, err := i.db.getDocuments(ids)
	if err != nil {
		r.Status = "stale"
		return r, nil
	}
	for n, source := range approval.Sources {
		if docs[n].ID != source.ID || docs[n].Fingerprint != source.Fingerprint {
			r.Status = "stale"
			break
		}
	}
	return r, nil
}
