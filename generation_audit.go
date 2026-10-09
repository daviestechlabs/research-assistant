package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

// The approved audit omits raw requests, responses, and internal source paths.
// The generator's original transcripts stay in its separate operator receipt.
//
//go:embed quality_generation_audit.json
var generationAuditApproval []byte

type generationAuditReport struct {
	Contract        string                  `json:"contract"`
	Status          string                  `json:"status"`
	AuditedAt       string                  `json:"audited_at"`
	ReceiptSHA      string                  `json:"receipt_sha256"`
	SuiteSHA        string                  `json:"suite_sha256"`
	PolicySHA       string                  `json:"generation_policy_sha256"`
	AnnotationsSHA  string                  `json:"annotations_sha256"`
	Annotator       string                  `json:"annotator"`
	Model           string                  `json:"configured_model"`
	ModelIdentity   *ServingModelIdentity   `json:"model_identity"`
	SelectedCase    string                  `json:"selected_case"`
	GenerationCalls int                     `json:"generation_dispatched_requests"`
	AuditCalls      int                     `json:"audit_model_calls"`
	Counts          json.RawMessage         `json:"counts"`
	Unmeasured      []string                `json:"unmeasured_cases"`
	Cases           []json.RawMessage       `json:"cases"`
	Limits          []string                `json:"limits"`
	GenerationImage string                  `json:"generation_image,omitempty"`
	Audits          []generationAuditReport `json:"audits,omitempty"`
}

type generationAuditEntry struct {
	Contract string `json:"contract"`
	SHA      string `json:"receipt_sha256"`
	Image    string `json:"generation_image"`
	Filename string `json:"filename"`
}

type generationAuditSelection struct {
	generationAuditEntry
	Sources []qualitySource        `json:"sources"`
	Audits  []generationAuditEntry `json:"audits"`
}

func readGenerationAudit(i *inference) (generationAuditReport, error) {
	r := generationAuditReport{Status: "missing"}
	data := generationAuditApproval
	if file := os.Getenv("RESEARCH_GENERATION_AUDIT_MANIFEST"); file != "" {
		var err error
		data, err = os.ReadFile(file)
		if err != nil || len(data) > 1<<20 {
			return r, errors.New("generation audit approval is unavailable")
		}
	}
	if string(bytes.TrimSpace(data)) == "{}" {
		return r, nil
	}
	var approval generationAuditSelection
	if json.Unmarshal(data, &approval) != nil || len(approval.Sources) != 3 {
		return r, errors.New("generation audit approval is invalid")
	}
	if approval.Contract == "research-generation-audit-collection-v1" {
		if len(approval.Audits) < 1 || len(approval.Audits) > 8 {
			return r, errors.New("generation audit collection is invalid")
		}
		r.Contract, r.Status = approval.Contract, "measured-development-audits"
		r.Limits = []string{"Each run retains its original receipt, failures, unassessable checks, and unrun cases. Separate runs are not a single uninterrupted measurement. Repeated cases must not be added together as independent accuracy evidence."}
		seen := map[string]bool{}
		for _, entry := range approval.Audits {
			if entry.Contract != "research-generation-audit-v1" || seen[entry.Filename] || entry.Filename == "" {
				return r, errors.New("generation audit entry is invalid")
			}
			seen[entry.Filename] = true
			child, err := readApprovedGenerationAudit(i, entry, approval.Sources, true)
			if err != nil {
				return r, err
			}
			r.Audits = append(r.Audits, child)
		}
		available := false
		for _, audit := range r.Audits {
			available = available || audit.Status != "missing"
		}
		if !available {
			return generationAuditReport{Status: "missing"}, nil
		}
		return r, nil
	}
	if approval.Contract != "research-generation-audit-v1" || len(approval.Audits) != 0 {
		return r, errors.New("generation audit approval is invalid")
	}
	return readApprovedGenerationAudit(i, approval.generationAuditEntry, approval.Sources, false)
}

func readApprovedGenerationAudit(i *inference, approval generationAuditEntry, sources []qualitySource, allowPartial bool) (generationAuditReport, error) {
	r := generationAuditReport{Status: "missing"}
	if !shaPattern.MatchString(approval.SHA) {
		return r, errors.New("generation audit checksum is invalid")
	}
	if approval.Filename == "" {
		approval.Filename = "research-generation-audit.json"
	}
	if !regexp.MustCompile(`^research-generation-audit(?:-[a-z0-9-]{1,60})?\.json$`).MatchString(approval.Filename) {
		return r, errors.New("generation audit filename is invalid")
	}

	f, err := os.Open(filepath.Join(filepath.Dir(i.budget.path), approval.Filename))
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, errors.New("generation audit unavailable")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 1024*1024+1))
	if err != nil || len(b) > 1024*1024 || textSHA(b) != approval.SHA {
		return r, errors.New("generation audit does not match the approved receipt")
	}
	if json.Unmarshal(b, &r) != nil || r.Contract != approval.Contract || (r.Status != "measured-development-audit" && (!allowPartial || r.Status != "partial-development-audit")) || r.SuiteSHA != textSHA(generationQualityCases) || r.AuditCalls != 0 || len(r.Audits) != 0 {
		return r, errors.New("generation audit is invalid")
	}
	if !allowPartial {
		if len(r.Cases) != 6 || r.SelectedCase != "" {
			return r, errors.New("generation audit omits required cases")
		}
	} else {
		suite, err := decodeGenerationSuite(generationQualityCases)
		if err != nil {
			return r, err
		}
		planned := []generationCase{}
		for _, c := range suite.Cases {
			if r.SelectedCase == "" || r.SelectedCase == c.ID {
				planned = append(planned, c)
			}
		}
		if len(planned) == 0 || len(r.Cases) == 0 || len(r.Cases) > len(planned) {
			return r, errors.New("generation audit case scope is invalid")
		}
		for n, data := range r.Cases {
			var c struct {
				ID string `json:"case_id"`
			}
			if json.Unmarshal(data, &c) != nil || c.ID != planned[n].ID {
				return r, errors.New("generation audit case order is invalid")
			}
		}
		if r.Status == "measured-development-audit" && (len(r.Cases) != len(planned) || len(r.Unmeasured) != 0) {
			return r, errors.New("finished generation audit omits cases")
		}
		if len(r.Unmeasured) != len(planned)-len(r.Cases) {
			return r, errors.New("generation audit unrun scope is invalid")
		}
		for n, id := range r.Unmeasured {
			if id != planned[len(r.Cases)+n].ID {
				return r, errors.New("generation audit unrun case changed")
			}
		}
	}
	r.GenerationImage = approval.Image
	ids := []string{}
	for _, source := range sources {
		ids = append(ids, source.ID)
	}
	docs, err := i.db.getDocuments(ids)
	if err != nil {
		r.Status = "stale"
		return r, nil
	}
	for n, source := range sources {
		if docs[n].ID != source.ID || docs[n].Fingerprint != source.Fingerprint {
			r.Status = "stale"
		}
	}
	// An alias-only measurement cannot pass a later native identity gate.
	if r.Model != i.model || i.identityURL != "" || r.PolicySHA != generationEvidencePolicySHA() {
		r.Status = "stale"
	}
	return r, nil
}
