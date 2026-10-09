package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// Operator proof uses a fresh explicit desk, the real NAS catalog, and its
// existing usage ledger. It never opens a second worker on the live desk.
func checkResearchJobs(ctx context.Context, path string, i *inference, archive *pdfArchive, data SessionData) (any, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) == filepath.Clean(deskPath(i)) {
		return nil, errors.New("check-jobs requires an absolute, separate, new desk database path")
	}
	if !data.Search.Meaning && ((data.Task != "compare" && !(data.PDFMode && data.Task == "explain")) || len(data.SelectedIDs) < 1 || (data.Task == "compare" && len(data.SelectedIDs) < 2) || (data.PDFMode && archive == nil)) {
		return nil, errors.New("check-jobs requires a comparison and configured storage for PDF reading")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, errors.New("the verification desk must not already exist")
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	s, err := openResearchDesk(path, i, archive)
	if err != nil {
		return nil, err
	}
	defer s.close()
	data.Title, data.Notes = "Operator comparison verification", "Separate verification desk; the production worker stays unchanged."
	if data.Search.Meaning {
		data.Title = "Operator semantic verification"
	}
	const owner = "operator-verification"
	x, err := s.save(owner, "", 0, data)
	if err != nil {
		return nil, err
	}
	kind := "answer"
	if data.PDFMode {
		kind = "pdf-answer"
	}
	if data.Search.Meaning {
		kind = "semantic-search"
	}
	jobs := []ResearchJob{}
	phases := []string{}
	for n := 0; n < 2; n++ {
		job, err := s.submit(owner, x.ID, x.Revision, kind)
		if err != nil {
			return nil, err
		}
		for {
			job, err = s.job(owner, job.ID)
			if err != nil {
				return nil, err
			}
			if len(phases) == 0 || phases[len(phases)-1] != job.Phase {
				phases = append(phases, job.Phase)
			}
			if job.Status != "queued" && job.Status != "running" {
				break
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
		if job.Status != "complete" {
			return nil, errors.New("comparison verification failed: " + job.Error)
		}
		var semanticResult SearchResponse
		if data.Search.Meaning {
			if json.Unmarshal(job.Result, &semanticResult) != nil || semanticResult.Coverage == nil {
				return nil, errors.New("semantic result lost its coverage")
			}
			if n == 1 && !job.Cached && (!semanticResult.EmbeddingCached || semanticResult.ModelCalls != 0) {
				return nil, errors.New("semantic repeat did not reuse its query embedding")
			}
		}
		if !data.Search.Meaning && job.Cached != (n == 1) {
			return nil, errors.New("comparison cache verification failed")
		}
		jobs = append(jobs, job)
	}
	// Reopen through a separate connection, without starting another worker.
	copy, err := openDatabase(path, false)
	if err != nil {
		return nil, err
	}
	defer copy.close()
	rows, err := copy.rows("PRAGMA integrity_check")
	if err != nil || len(rows) != 1 || rows[0][0] != "ok" {
		return nil, errors.New("verification desk integrity failed")
	}
	rows, err = copy.rows("SELECT result FROM desk_jobs WHERE owner=? AND id=? AND status='complete'", owner, jobs[1].ID)
	if err != nil || len(rows) != 1 {
		return nil, errors.New("comparison did not persist")
	}
	var saved AnswerResponse
	if data.Search.Meaning {
		var result SearchResponse
		if json.Unmarshal([]byte(rows[0][0]), &result) != nil || result.Coverage == nil || len(result.Documents) == 0 {
			return nil, errors.New("persisted semantic result lost its sources or coverage")
		}
	} else if data.PDFMode && data.Task == "explain" {
		var reading PDFAnswerResponse
		if json.Unmarshal([]byte(rows[0][0]), &reading) != nil || len(reading.Papers) != len(data.SelectedIDs) || len(reading.Statements) == 0 {
			return nil, errors.New("persisted reading lost its pages and statements")
		}
	} else if json.Unmarshal([]byte(rows[0][0]), &saved) != nil || saved.Comparison == nil || len(saved.Comparison.Sections) != 5 {
		return nil, errors.New("persisted comparison lost its evidence sections")
	}
	return map[string]any{"scope": "isolated operator desk using the application worker and shared catalog usage ledger", "desk_path": path, "session_id": x.ID, "jobs": jobs, "observed_phases": phases, "integrity": "ok", "separate_connection_restored": true, "repeat_cached": jobs[1].Cached, "repeat_embedding_reused": data.Search.Meaning && !jobs[1].Cached, "model_calls_for_repeat": 0, "result": jobs[0].Result}, nil
}
