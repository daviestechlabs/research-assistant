package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func deskFixture(t *testing.T) (*researchDesk, *inference) {
	t.Helper()
	d := testDB(t)
	loadFixture(t, d)
	i := testInference(t, d)
	s, err := openResearchDesk(filepath.Join(t.TempDir(), "desk.sqlite"), i, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.close)
	return s, i
}

func sessionFixture() SessionData {
	return SessionData{Title: "Attention", Search: SearchRequest{Query: "attention"}, SelectedIDs: []string{"arxiv:1706.03762"}, Question: "Explain attention.", Task: "explain", Notes: "Keep this note across devices."}
}

func awaitDeskJob(t *testing.T, s *researchDesk, owner, id string) ResearchJob {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); {
		j, err := s.job(owner, id)
		if err != nil {
			t.Fatal(err)
		}
		if j.Status != "queued" && j.Status != "running" {
			return j
		}
		time.Sleep(time.Millisecond * 5)
	}
	t.Fatal("job did not finish")
	return ResearchJob{}
}

func TestDeskOwnershipRevisionPersistence(t *testing.T) {
	s, _ := deskFixture(t)
	x, err := s.save("owner", "", 0, sessionFixture())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.session("other", x.ID); !errors.Is(err, errDeskMissing) {
		t.Fatalf("owner bypass: %v", err)
	}
	var successes atomic.Int32
	var wg sync.WaitGroup
	for n := 0; n < 2; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.save("owner", x.ID, x.Revision, x.Data)
			if err == nil {
				successes.Add(1)
			} else if !errors.Is(err, errDeskConflict) {
				t.Errorf("save: %v", err)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatal("concurrent stale writes were not rejected")
	}
	path := s.db.path
	// A second connection verifies durable state without changing worker ownership.
	db, err := openDatabase(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.close()
	rows, err := db.rows("SELECT data,revision FROM desk_sessions WHERE owner=?", "owner")
	if err != nil || len(rows) != 1 || rows[0][1] != "2" || !strings.Contains(rows[0][0], "across devices") {
		t.Fatalf("persistence: %v %v", rows, err)
	}
	x, err = s.session("owner", x.ID)
	if err != nil || len(x.Sources) != 1 || x.Sources[0].NASRef != "" {
		t.Fatalf("session source leak: %+v %v", x, err)
	}
}

func TestDeskVerifiedCacheAndInvalidation(t *testing.T) {
	s, i := deskFixture(t)
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		if content, ok := answerReviewFixtureContent(req); ok {
			writeJSON(w, 200, map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": content}}}})
			return
		}
		content := `{"reported":{"kind":"reported","text":"","source_ids":["arxiv:1706.03762"],"evidence_quotes":[{"id":"arxiv:1706.03762","quote":"A transformer replaces recurrence with attention for machine translation."}]},"interpretation":{"kind":"interpretation","text":"Attention changes the mechanism.","source_ids":["arxiv:1706.03762"]},"limits":{"kind":"limits","text":"The abstract omits full evaluation details.","source_ids":["arxiv:1706.03762"]}}`
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": content}}}})
	}))
	defer model.Close()
	i.answerURL, i.model = model.URL, "fixture-model"
	x, err := s.save("owner", "", 0, sessionFixture())
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.submit("owner", x.ID, x.Revision, "answer")
	if err != nil {
		t.Fatal(err)
	}
	j = awaitDeskJob(t, s, "owner", j.ID)
	if j.Status != "complete" || j.Cached || calls.Load() != 2 {
		t.Fatalf("job: %+v calls=%d", j, calls.Load())
	}
	if strings.Contains(string(j.Result), "s3://") || strings.Contains(string(j.Result), "nas_ref") {
		t.Fatal("cache contains NAS reference")
	}
	before, _ := i.stats()
	j, err = s.submit("owner", x.ID, x.Revision, "answer")
	if err != nil || !j.Cached || j.Status != "complete" || calls.Load() != 2 {
		t.Fatalf("cache: %+v %v", j, err)
	}
	after, _ := i.stats()
	if before.(map[string]any)["model_calls_today"] != after.(map[string]any)["model_calls_today"] {
		t.Fatal("cache hit charged the budget")
	}
	old, _ := s.cacheKey("answer", x.Data)
	if _, err = importCatalog(i.db, "arxiv", "jsonl", strings.NewReader(`{"id":"1706.03762","title":"Changed title","abstract":"A transformer replaces recurrence with attention for machine translation."}`), 0); err != nil {
		t.Fatal(err)
	}
	changed, _ := s.cacheKey("answer", x.Data)
	if changed == old {
		t.Fatal("source fingerprint did not invalidate cache")
	}
	i.model = "changed-model"
	changedAgain, _ := s.cacheKey("answer", x.Data)
	if changedAgain == changed {
		t.Fatal("model contract did not invalidate cache")
	}
}

func TestDeskCancellationAndDuplicateSubmission(t *testing.T) {
	s, i := deskFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer model.Close()
	defer close(release)
	i.answerURL, i.model = model.URL, "fixture"
	x, err := s.save("owner", "", 0, sessionFixture())
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.submit("owner", x.ID, x.Revision, "answer")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("model not started")
	}
	duplicate, err := s.submit("owner", x.ID, x.Revision, "answer")
	if err != nil || duplicate.ID != j.ID {
		t.Fatalf("duplicate job: %+v %v", duplicate, err)
	}
	if _, err = s.cancelJob("other", j.ID); !errors.Is(err, errDeskMissing) {
		t.Fatal("cross-owner cancellation")
	}
	j, err = s.cancelJob("owner", j.ID)
	if err != nil || j.Status != "cancelled" {
		t.Fatalf("cancel: %+v %v", j, err)
	}
	if calls.Load() != 1 {
		t.Fatal("duplicate model call")
	}
	if _, err = s.job("other", j.ID); !errors.Is(err, errDeskMissing) {
		t.Fatal("cross-owner job read")
	}
}

func TestDeskRestartDoesNotRepeatUncertainCalls(t *testing.T) {
	d := testDB(t)
	loadFixture(t, d)
	i := testInference(t, d)
	path := filepath.Join(t.TempDir(), "desk.sqlite")
	s, err := openResearchDesk(path, i, nil)
	if err != nil {
		t.Fatal(err)
	}
	x, err := s.save("owner", "", 0, sessionFixture())
	if err != nil {
		t.Fatal(err)
	}
	s.close()
	db, err := openDatabase(path, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.run("INSERT INTO desk_jobs(id,owner,session_id,revision,kind,status,phase,updated,cache_key,input) VALUES(?,?,?,1,'answer','running','Calling model',?,'key','{}')", strings.Repeat("a", 32), "owner", x.ID, deskTime())
	db.close()
	if err != nil {
		t.Fatal(err)
	}
	s, err = openResearchDesk(path, i, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	j, err := s.job("owner", strings.Repeat("a", 32))
	if err != nil || j.Status != "interrupted" || !strings.Contains(j.Error, "charged") {
		t.Fatalf("recovery: %+v %v", j, err)
	}
	x, err = s.session("owner", x.ID)
	if err != nil || x.Data.Notes != sessionFixture().Notes {
		t.Fatal("session did not survive restart")
	}
}

func TestDeskHTTPOriginAndAnonymousChecks(t *testing.T) {
	s, i := deskFixture(t)
	h := securedPDFHandler(i.db, i, "https://research.example", &tokenVerifier{}, nil, s)
	for _, path := range []string{"/api/sessions", "/api/jobs/" + strings.Repeat("a", 32), "/desk.js"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "https://research.example"+path, nil))
		if w.Code != 401 {
			t.Fatalf("anonymous %s: %d", path, w.Code)
		}
	}
	r := httptest.NewRequest("POST", "https://research.example/api/sessions", strings.NewReader(`{}`))
	r.Header.Set("Origin", "https://other.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("foreign-origin write was accepted")
	}
}
