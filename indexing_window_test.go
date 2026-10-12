package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestOfflineAbstractWindowsKeepOnlineQueryBound(t *testing.T) {
	for _, count := range []int{843, 4097} {
		text := strings.Repeat("α", count)
		offsets := make([][]int, count)
		for n := range offsets {
			offsets[n] = []int{n, n + 1}
		}
		sha := textSHA([]byte(text))
		if _, err := exactQueryWindows(text, sha, 128, 2, offsets); err == nil {
			t.Fatal("Long source accepted as an online search question")
		}
		windows, err := exactAbstractWindows(text, sha, 128, 2, offsets)
		if err != nil || len(windows) <= 8 {
			t.Fatalf("complete abstract: %d windows, %v", len(windows), err)
		}
		covered := 0
		for _, w := range windows {
			if len([]rune(w.text)) > 112 || w.weight < 1 {
				t.Fatal("window algorithm changed")
			}
			covered += w.weight
		}
		if covered != count || windows[len(windows)-1].text != string([]rune(text)[(len(windows)-1)*96:]) {
			t.Fatal("abstract tail lost or overlap counted twice")
		}
		offsets[len(offsets)-1][1] = count + 1
		if _, err := exactAbstractWindows(text, sha, 128, 2, offsets); err == nil {
			t.Fatal("invalid source offsets accepted")
		}
	}
	text := "source"
	offsets := make([][]int, 131073)
	if _, err := exactAbstractWindows(text, textSHA([]byte(text)), 128, 2, offsets); err == nil {
		t.Fatal("unbounded tokenizer output accepted")
	}
	if _, err := exactAbstractWindows(text, "wrong", 128, 2, [][]int{{0, 6}}); err == nil {
		t.Fatal("source identity changed")
	}
}

func TestMeaningResumeRequiresPreviouslyReconciledCounts(t *testing.T) {
	for _, value := range []any{nil, false, "true"} {
		m := &indexManager{}
		state := &indexState{Validation: map[string]any{"counts_reconciled": value}}
		if err := m.indexMeaning(context.Background(), indexRun{}, state); err == nil || err.Error() != "meaning indexing requires reconciled catalog counts" {
			t.Fatal("Unverified counters accepted", err)
		}
	}
}

func TestWorkerShutdownPreservesCheckpointPause(t *testing.T) {
	transport := errors.New("semantic service request failed or exceeded its deadline")
	if got := indexWorkerOutcome(context.Background(), transport); got != transport {
		t.Fatal("Real transport failure hidden")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(indexWorkerOutcome(ctx, transport), errIndexPause) {
		t.Fatal("Shutdown transport cancellation recorded as a failed run")
	}
	if !errors.Is(indexWorkerOutcome(ctx, errIndexCancel), errIndexCancel) {
		t.Fatal("User cancellation changed")
	}
	if indexWorkerOutcome(ctx, nil) != nil {
		t.Fatal("Completed work changed into a pause")
	}
}
