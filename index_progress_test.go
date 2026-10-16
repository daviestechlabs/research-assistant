package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidationProgressSamplesActualRunningSQLAndSeparatesIdleHeartbeat(t *testing.T) {
	d, err := openDatabase(":memory:", true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	id := strings.Repeat("a", 32)
	m := &indexManager{root: t.TempDir()}
	os.MkdirAll(m.runPath(id, ""), 0700)
	p, err := m.observeValidationInterval(id, d, 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer p.finish(false)
	p.stage("source-counts")
	result := make(chan error, 1)
	go func() {
		_, e := d.rows("WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<2000000) SELECT sum(x) FROM n")
		result <- e
	}()
	sampledDuringQuery := false
	timedOut := false
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
loop:
	for {
		select {
		case err = <-result:
			break loop
		case <-deadline.C:
			timedOut = true
			err = <-result
			break loop
		case <-ticker.C:
			r := m.validationProgress(id)
			if r != nil && r.WorkTicks > 0 && r.State == "running" {
				sampledDuringQuery = true
			}
		}
	}
	if err != nil || timedOut || !sampledDuringQuery {
		t.Fatalf("no live SQL progress observed: %v timeout=%v sampled=%v", err, timedOut, sampledDuringQuery)
	}
	p.sample()
	before := m.validationProgress(id)
	time.Sleep(10 * time.Millisecond)
	p.sample()
	after := m.validationProgress(id)
	if before.LastAdvanced != after.LastAdvanced || before.WorkTicks != after.WorkTicks || before.Updated == after.Updated {
		t.Fatal("idle heartbeat misreported as advancing SQL")
	}
	p.stage("catalog-diff")
	stage := m.validationProgress(id)
	if stage.StageWorkTicks != 0 || stage.WorkTicks == 0 || stage.StageStarted == before.StageStarted {
		t.Fatal("stage transition lost cumulative work or failed to reset stage count")
	}
	p.finish(true)
	p.finish(false)
	final := m.validationProgress(id)
	if final.State != "passed" || final.WorkTicks < after.WorkTicks {
		t.Fatal("final progress regressed")
	}
	if d.sqlProgressTicks() != 0 {
		t.Fatal("observer remained installed")
	}
	if _, err = d.rows("SELECT 1"); err != nil {
		t.Fatal(err)
	}
}
func TestValidationProgressFailureAndClosedConnectionCleanup(t *testing.T) {
	d, err := openDatabase(":memory:", true)
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("b", 32)
	m := &indexManager{root: t.TempDir()}
	os.MkdirAll(m.runPath(id, ""), 0700)
	p, err := m.observeValidationInterval(id, d, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.observeValidation(id, d); err == nil {
		t.Fatal("duplicate observer accepted")
	}
	p.stage("sqlite-integrity")
	if _, err = d.rows("SELECT * FROM missing_private_table"); err == nil {
		t.Fatal("missing table accepted")
	}
	d.close()
	p.sample()
	p.finish(false)
	r := m.validationProgress(id)
	if r.State != "stopped" {
		t.Fatal("failed validation reported success")
	}
	data, _ := os.ReadFile(filepath.Join(m.root, "runs", id, "validation-progress.json"))
	if strings.Contains(string(data), "private_table") || strings.Contains(string(data), m.root) {
		t.Fatal("SQL or filesystem path leaked")
	}
	if _, err = m.observeValidation(id, d); err == nil {
		t.Fatal("closed connection observed")
	}
}
func TestIntegrityProgressUsesSeparateDatabaseAndTextStages(t *testing.T) {
	d, err := openDatabase(":memory:", true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	if err = d.initialize(); err != nil {
		t.Fatal(err)
	}
	stages := []string{}
	if err = d.verifyObserved(func(s string) { stages = append(stages, s) }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(stages, ",") != "sqlite-integrity,full-text-integrity" {
		t.Fatal(stages)
	}
}
