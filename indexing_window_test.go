package main

import (
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
