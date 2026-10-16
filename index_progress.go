package main

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"time"
)

type indexValidationProgress struct {
	ChecksumRecords     uint64 `json:"checksum_records,omitempty"`
	WorkUnit            string `json:"work_unit,omitempty"`
	RunID               string `json:"run_id"`
	Stage               string `json:"stage"`
	State               string `json:"state"`
	Updated             string `json:"updated"`
	StageStarted        string `json:"stage_started"`
	LastAdvanced        string `json:"last_advanced"`
	WorkTicks           uint64 `json:"work_ticks"`
	StageWorkTicks      uint64 `json:"stage_work_ticks"`
	InstructionsPerTick int    `json:"instructions_per_tick"`
}

type validationObserver struct {
	mu        sync.Mutex
	once      sync.Once
	db        *database
	path      string
	progress  indexValidationProgress
	stageBase uint64
	done      chan struct{}
	exited    chan struct{}
}

func (m *indexManager) observeValidation(id string, d *database) (*validationObserver, error) {
	return m.observeValidationInterval(id, d, 10*time.Second)
}
func (m *indexManager) observeValidationInterval(id string, d *database, interval time.Duration) (*validationObserver, error) {
	if !deskID.MatchString(id) || interval <= 0 {
		return nil, errors.New("invalid validation observer")
	}
	if err := d.startSQLProgress(); err != nil {
		return nil, err
	}
	now := indexTime()
	p := &validationObserver{db: d, path: m.runPath(id, "validation-progress.json"), done: make(chan struct{}), exited: make(chan struct{}), progress: indexValidationProgress{RunID: id, State: "running", Stage: "starting", Updated: now, StageStarted: now, LastAdvanced: now, InstructionsPerTick: 10000}}
	if d.pg != nil {
		p.progress.WorkUnit = "completed-postgres-query"
		p.progress.InstructionsPerTick = 0
	} else {
		p.progress.WorkUnit = "sqlite-execution-callback"
	}
	if err := writeIndexJSON(p.path, p.progress); err != nil {
		d.stopSQLProgress()
		return nil, err
	}
	go func() {
		defer close(p.exited)
		timer := time.NewTicker(interval)
		defer timer.Stop()
		for {
			select {
			case <-p.done:
				return
			case <-timer.C:
				p.sample()
			}
		}
	}()
	return p, nil
}
func (p *validationObserver) sampleLocked() {
	rows := p.db.checksumRows()
	if rows > p.progress.ChecksumRecords {
		p.progress.ChecksumRecords = rows
		p.progress.LastAdvanced = indexTime()
	}
	ticks := p.db.sqlProgressTicks()
	if ticks > p.progress.WorkTicks {
		p.progress.WorkTicks = ticks
		p.progress.LastAdvanced = indexTime()
	}
	p.progress.StageWorkTicks = p.progress.WorkTicks - p.stageBase
	p.progress.Updated = indexTime()
}
func (p *validationObserver) sample() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sampleLocked()
	_ = writeIndexJSON(p.path, p.progress)
}
func (p *validationObserver) stage(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sampleLocked()
	if p.progress.Stage != name {
		now := indexTime()
		p.progress.Stage = name
		p.progress.StageStarted = now
		p.progress.LastAdvanced = now
		p.stageBase = p.progress.WorkTicks
		p.progress.StageWorkTicks = 0
	}
	_ = writeIndexJSON(p.path, p.progress)
}
func (p *validationObserver) finish(passed bool) {
	p.once.Do(func() {
		close(p.done)
		<-p.exited
		p.mu.Lock()
		defer p.mu.Unlock()
		p.sampleLocked()
		p.progress.State = "stopped"
		if passed {
			p.progress.State = "passed"
		}
		_ = writeIndexJSON(p.path, p.progress)
		p.db.stopSQLProgress()
	})
}
func (m *indexManager) validationProgress(id string) *indexValidationProgress {
	var p indexValidationProgress
	if readIndexJSON(filepath.Join(m.root, "runs", id, "validation-progress.json"), &p) != nil || p.RunID != id || (p.InstructionsPerTick != 10000 && !(p.InstructionsPerTick == 0 && p.WorkUnit == "completed-postgres-query")) {
		return nil
	}
	switch p.Stage {
	case "postgres-constraints", "postgres-indexes", "starting", "sqlite-integrity", "full-text-integrity", "source-counts", "identifier-queries", "catalog-diff", "keyword-query", "duplicate-identifiers", "catalog-checksum", "saved-pdf-samples":
	default:
		return nil
	}
	switch p.State {
	case "running", "stopped", "passed":
	default:
		return nil
	}
	return &p
}

func (p *validationObserver) catalogSHA(path string) (string, error) {
	if p.db.pg != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		return p.db.pg.identity(ctx)
	}
	return indexFileSHA(path)
}
