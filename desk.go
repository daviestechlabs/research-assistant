package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The desk is separate from the read-only catalog and its model usage ledger.
// One deployed process owns the worker. SQLite transactions guard device races.
type researchDesk struct {
	indexing *indexManager
	db       *database
	i        *inference
	archive  *pdfArchive
	ctx      context.Context
	stop     context.CancelFunc
	wake     chan struct{}
	done     chan struct{}
	mu       sync.Mutex
	cancel   map[string]context.CancelFunc
}

type ownerKey struct{}

func requestOwner(r *http.Request) string {
	owner, _ := r.Context().Value(ownerKey{}).(string)
	return owner
}

type SessionData struct {
	Title             string        `json:"title"`
	Search            SearchRequest `json:"search"`
	SelectedIDs       []string      `json:"selected_ids"`
	Question          string        `json:"question"`
	Task              string        `json:"task"`
	Notes             string        `json:"notes"`
	IncludeHypotheses bool          `json:"include_hypotheses"`
	PDFMode           bool          `json:"pdf_mode"`
	SearchConnect     bool          `json:"search_connect,omitempty"`
}

type ResearchSession struct {
	ID       string        `json:"id"`
	Revision int           `json:"revision"`
	Updated  string        `json:"updated_at"`
	Data     SessionData   `json:"data"`
	Sources  []Document    `json:"sources,omitempty"`
	Jobs     []ResearchJob `json:"jobs,omitempty"`
}

type ResearchJob struct {
	ID              string          `json:"id"`
	SessionID       string          `json:"session_id"`
	SessionRevision int             `json:"session_revision"`
	Kind            string          `json:"kind"`
	Status          string          `json:"status"`
	Phase           string          `json:"phase"`
	Updated         string          `json:"updated_at"`
	Cached          bool            `json:"cached"`
	Result          json.RawMessage `json:"result,omitempty"`
	Error           string          `json:"error,omitempty"`
}

var deskID = regexp.MustCompile(`^[a-f0-9]{32}$`)
var errDeskMissing = errors.New("session or job was not found")
var errDeskConflict = errors.New("this session changed on another device; reopen it before saving")

func randomDeskID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func openResearchDesk(path string, i *inference, archive *pdfArchive) (*researchDesk, error) {
	db, err := openDatabase(path, true)
	if err != nil {
		return nil, err
	}
	if err = db.exec(`CREATE TABLE IF NOT EXISTS desk_sessions(
 id TEXT PRIMARY KEY, owner TEXT NOT NULL, revision INTEGER NOT NULL,
 updated TEXT NOT NULL, data TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS desk_sessions_owner ON desk_sessions(owner,updated);
CREATE TABLE IF NOT EXISTS desk_jobs(
 id TEXT PRIMARY KEY, owner TEXT NOT NULL, session_id TEXT NOT NULL,
 revision INTEGER NOT NULL, kind TEXT NOT NULL, status TEXT NOT NULL,
 phase TEXT NOT NULL, updated TEXT NOT NULL, cached INTEGER NOT NULL DEFAULT 0,
 cache_key TEXT NOT NULL, input TEXT NOT NULL, result TEXT NOT NULL DEFAULT '',
 error TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS desk_jobs_owner ON desk_jobs(owner,session_id,updated);
CREATE TABLE IF NOT EXISTS desk_cache(
 owner TEXT NOT NULL, key TEXT NOT NULL, updated TEXT NOT NULL, result TEXT NOT NULL,
 PRIMARY KEY(owner,key));
CREATE TABLE IF NOT EXISTS desk_embeddings(
 owner TEXT NOT NULL, key TEXT NOT NULL, updated TEXT NOT NULL, vector TEXT NOT NULL,
 PRIMARY KEY(owner,key));
PRAGMA user_version=1;`); err != nil {
		db.close()
		return nil, err
	}
	// A model may have completed before a process died. Never replay that call.
	_, err = db.run(`UPDATE desk_jobs SET status='interrupted',phase='Interrupted by a server restart',
 error='The server restarted. The model call may have been charged; retry explicitly.',updated=? WHERE status='running'`, deskTime())
	if err != nil {
		db.close()
		return nil, err
	}
	ctx, stop := context.WithCancel(context.Background())
	s := &researchDesk{db: db, i: i, archive: archive, ctx: ctx, stop: stop, wake: make(chan struct{}, 1), done: make(chan struct{}), cancel: map[string]context.CancelFunc{}}
	go s.worker()
	s.signal()
	return s, nil
}

func deskTime() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func (s *researchDesk) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *researchDesk) close() {
	s.stop()
	<-s.done
	s.db.close()
}

func (s *researchDesk) validate(data SessionData) error {
	if len(data.Title) > 120 || strings.TrimSpace(data.Title) == "" || len(data.Notes) > 8000 || len(data.Question) > 2000 || len(data.SelectedIDs) > 8 {
		return errors.New("session title, notes, question, or selection exceeds its limit")
	}
	if data.Task != "explain" && data.Task != "compare" && data.Task != "hypothesize" {
		return errors.New("invalid research task")
	}
	if data.Search.Query != "" {
		q := data.Search
		if err := q.validate(); err != nil {
			return err
		}
	}
	if len(data.SelectedIDs) > 0 {
		_, err := s.i.db.getDocuments(data.SelectedIDs)
		return err
	}
	return nil
}

func (s *researchDesk) save(owner, id string, revision int, data SessionData) (ResearchSession, error) {
	if owner == "" {
		return ResearchSession{}, errDeskMissing
	}
	if err := s.validate(data); err != nil {
		return ResearchSession{}, err
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return ResearchSession{}, err
	}
	now := deskTime()
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	if id == "" {
		if revision != 0 {
			return ResearchSession{}, errDeskConflict
		}
		rows, err := s.db.rows("SELECT count(*) FROM desk_sessions WHERE owner=?", owner)
		if err != nil {
			return ResearchSession{}, err
		}
		count, _ := strconv.Atoi(rows[0][0])
		if count >= 100 {
			return ResearchSession{}, errors.New("the desk holds at most 100 sessions")
		}
		id, err = randomDeskID()
		if err == nil {
			_, err = s.db.run("INSERT INTO desk_sessions VALUES(?,?,1,?,?)", id, owner, now, string(encoded))
		}
	} else {
		if !deskID.MatchString(id) || revision < 1 {
			return ResearchSession{}, errDeskMissing
		}
		var changed int
		changed, err = s.db.run("UPDATE desk_sessions SET revision=revision+1,updated=?,data=? WHERE id=? AND owner=? AND revision=CAST(? AS INTEGER)", now, string(encoded), id, owner, fmt.Sprint(revision))
		if err == nil && changed != 1 {
			return ResearchSession{}, errDeskConflict
		}
	}
	return ResearchSession{ID: id, Revision: revision + 1, Updated: now, Data: data}, err
}

func sessionRows(rows [][]string) ([]ResearchSession, error) {
	result := []ResearchSession{}
	for _, row := range rows {
		revision, _ := strconv.Atoi(row[1])
		x := ResearchSession{ID: row[0], Revision: revision, Updated: row[2]}
		if json.Unmarshal([]byte(row[3]), &x.Data) != nil {
			return nil, errors.New("saved session is unreadable")
		}
		result = append(result, x)
	}
	return result, nil
}

func (s *researchDesk) list(owner string) ([]ResearchSession, error) {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	rows, err := s.db.rows("SELECT id,revision,updated,data FROM desk_sessions WHERE owner=? ORDER BY updated DESC LIMIT 100", owner)
	if err != nil {
		return nil, err
	}
	return sessionRows(rows)
}

func (s *researchDesk) session(owner, id string) (ResearchSession, error) {
	if !deskID.MatchString(id) {
		return ResearchSession{}, errDeskMissing
	}
	s.db.mu.Lock()
	rows, err := s.db.rows("SELECT id,revision,updated,data FROM desk_sessions WHERE owner=? AND id=?", owner, id)
	s.db.mu.Unlock()
	if err != nil {
		return ResearchSession{}, err
	}
	if len(rows) != 1 {
		return ResearchSession{}, errDeskMissing
	}
	items, err := sessionRows(rows)
	if err != nil {
		return ResearchSession{}, err
	}
	x := items[0]
	if len(x.Data.SelectedIDs) > 0 {
		x.Sources, err = s.i.db.getDocuments(x.Data.SelectedIDs)
		if err != nil {
			return ResearchSession{}, err
		}
		x.Sources = browserDocuments(x.Sources, s.archive != nil)
	}
	x.Jobs, err = s.jobs(owner, id)
	return x, err
}

const jobColumns = "id,session_id,revision,kind,status,phase,updated,cached,result,error"

func jobRows(rows [][]string) []ResearchJob {
	x := []ResearchJob{}
	for _, r := range rows {
		revision, _ := strconv.Atoi(r[2])
		j := ResearchJob{ID: r[0], SessionID: r[1], SessionRevision: revision, Kind: r[3], Status: r[4], Phase: r[5], Updated: r[6], Cached: r[7] == "1", Error: r[9]}
		if r[8] != "" {
			j.Result = json.RawMessage(r[8])
		}
		x = append(x, j)
	}
	return x
}

func (s *researchDesk) jobs(owner, session string) ([]ResearchJob, error) {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	columns := strings.Replace(jobColumns, ",result,", ",'',", 1)
	rows, err := s.db.rows("SELECT "+columns+" FROM desk_jobs WHERE owner=? AND session_id=? ORDER BY updated DESC LIMIT 50", owner, session)
	return jobRows(rows), err
}

func (s *researchDesk) job(owner, id string) (ResearchJob, error) {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	rows, err := s.db.rows("SELECT "+jobColumns+" FROM desk_jobs WHERE owner=? AND id=?", owner, id)
	if err != nil {
		return ResearchJob{}, err
	}
	if len(rows) != 1 {
		return ResearchJob{}, errDeskMissing
	}
	return jobRows(rows)[0], nil
}

// Persist an immutable request and source fingerprints. A session edit cannot
// change an in-flight job. The contract version covers prompts and validators.
const deskContract = "abstract-answer-v1-graph-filter-v4-desk-v1"

const deskInputContract = "research-job-input-v2-model-identity"

type researchJobInput struct {
	Contract string                `json:"contract"`
	Data     SessionData           `json:"data"`
	Model    *ServingModelIdentity `json:"model_identity,omitempty"`
}

func decodeResearchJobInput(input []byte) (SessionData, *ServingModelIdentity, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(input, &fields); err != nil {
		return SessionData{}, nil, errors.New("saved research job input is invalid")
	}
	if _, wrapped := fields["contract"]; !wrapped {
		// Older queued work has no model binding. It can run only while the
		// operator keeps the legacy configuration; activation requires resubmit.
		var data SessionData
		err := json.Unmarshal(input, &data)
		return data, nil, err
	}
	var saved researchJobInput
	decoder := json.NewDecoder(strings.NewReader(string(input)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&saved); err != nil || saved.Contract != deskInputContract {
		return SessionData{}, nil, errors.New("saved research job input is invalid")
	}
	return saved.Data, saved.Model, nil
}

func (s *researchDesk) cacheKey(kind string, data SessionData) (string, error) {
	ctx := context.Background()
	if kind != "semantic-search" {
		var err error
		ctx, err = s.i.bindModel(ctx)
		if err != nil {
			return "", err
		}
	}
	return s.cacheKeyForModel(kind, data, modelIdentityFromContext(ctx))
}

func (s *researchDesk) cacheKeyForModel(kind string, data SessionData, model *ServingModelIdentity) (string, error) {
	if kind == "semantic-search" {
		q := data.Search
		if err := q.validate(); err != nil {
			return "", err
		}
		if _, err := semanticFilter(q); err != nil {
			return "", err
		}
		coverage, err := s.i.semantic.coverage()
		if err != nil {
			return "", err
		}
		b, _ := json.Marshal([]any{semanticQueryContract, q, coverage.CatalogSHA, coverage.ModelSHA, coverage.Collection, s.i.semantic.embeddingOrigin})
		return textSHA(b), nil
	}
	if len(data.SelectedIDs) == 0 {
		return "", errors.New("select sources before starting research")
	}
	docs, err := s.i.db.getDocuments(data.SelectedIDs)
	if err != nil {
		return "", err
	}
	fingerprints := []string{}
	for _, d := range docs {
		if d.SummaryKind == "missing" {
			return "", errors.New("a selected source has no supplied abstract")
		}
		fingerprints = append(fingerprints, d.ID+":"+d.Fingerprint)
	}
	// Notes, titles, and keyword queries do not change an explicit answer request.
	contract := deskContract
	if kind == "graph" || (kind == "answer" && data.Task != "compare") {
		contract += ":" + scientificEvidenceContract + ":" + textSHA([]byte(scientificEvidencePolicy))
		contract += ":" + excerptRankingContract
	}
	if kind == "answer" && data.Task != "compare" {
		contract += ":" + answerEvidenceReviewContract + ":" + textSHA([]byte(answerEvidenceReviewPrompt))
	}
	if kind == "graph" {
		contract += ":" + graphEvidenceContract + ":" + textSHA([]byte(graphEvidencePrompt))
		contract += ":" + textSHA([]byte(graphConceptPolicy))
		contract += ":" + textSHA([]byte(graphQuestionPolicy))
		contract += ":" + graphRelationContract + ":" + textSHA([]byte(graphRelationReviewPrompt))
	}
	if kind == "pdf-answer" {
		contract += ":" + readingAnswerContract
	}
	if kind != "graph" && data.Task == "compare" {
		contract += ":" + comparisonContract
	}
	values := []any{contract, kind, s.i.answerURL, s.i.model, data.Question, data.Task, data.IncludeHypotheses, fingerprints}
	if model != nil {
		if err := s.i.checkServingIdentity(*model); err != nil {
			return "", err
		}
		values = append(values, servingIdentityContract, model.IdentitySHA)
	} else if s.i.identityURL != "" {
		return "", errors.New("research job has no verified model binding; submit a new job")
	}
	x, _ := json.Marshal(values)
	h := sha256.Sum256(x)
	return hex.EncodeToString(h[:]), nil
}

func (s *researchDesk) submit(owner, sessionID string, revision int, kind string) (ResearchJob, error) {
	return s.submitWithContext(context.Background(), owner, sessionID, revision, kind)
}

func (s *researchDesk) submitWithContext(ctx context.Context, owner, sessionID string, revision int, kind string) (ResearchJob, error) {
	if kind != "answer" && kind != "graph" && kind != "pdf-answer" && kind != "semantic-search" {
		return ResearchJob{}, errors.New("unsupported research job")
	}
	x, err := s.session(owner, sessionID)
	if err != nil {
		return ResearchJob{}, err
	}
	if x.Revision != revision {
		return ResearchJob{}, errDeskConflict
	}
	if kind != "semantic-search" && strings.TrimSpace(x.Data.Question) == "" {
		return ResearchJob{}, errors.New("save a research question before starting the job")
	}
	if (kind == "answer" || kind == "pdf-answer") && x.Data.Task == "compare" && len(x.Data.SelectedIDs) < 2 {
		return ResearchJob{}, errors.New("select at least two sources for a comparison")
	}
	if kind == "pdf-answer" {
		if s.archive == nil || !x.Data.PDFMode || len(x.Data.SelectedIDs) > 2 {
			return ResearchJob{}, errors.New("enable PDF reading and select one or two saved papers")
		}
		for _, id := range x.Data.SelectedIDs {
			if !strings.HasPrefix(id, "arxiv:") && s.i.db.books[id].MediaType != "application/pdf" {
				return ResearchJob{}, errors.New("PDF reading supports saved papers and private PDF books")
			}
		}
	}
	if kind != "semantic-search" {
		ctx, err = s.i.bindModel(ctx)
		if err != nil {
			return ResearchJob{}, err
		}
	}
	model := modelIdentityFromContext(ctx)
	key, err := s.cacheKeyForModel(kind, x.Data, model)
	if err != nil {
		return ResearchJob{}, err
	}
	input, _ := json.Marshal(researchJobInput{Contract: deskInputContract, Data: x.Data, Model: model})
	readCache := kind != "pdf-answer"
	if kind == "semantic-search" {
		coverage, err := s.i.semantic.coverage()
		if err != nil {
			return ResearchJob{}, err
		}
		readCache = coverage.State == "complete"
	}
	id, err := randomDeskID()
	if err != nil {
		return ResearchJob{}, err
	}
	now := deskTime()
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	if err = s.db.exec("BEGIN IMMEDIATE"); err != nil {
		return ResearchJob{}, err
	}
	defer s.db.exec("ROLLBACK")
	// Recheck revision inside the write transaction, after source validation.
	rows, err := s.db.rows("SELECT revision FROM desk_sessions WHERE id=? AND owner=?", sessionID, owner)
	if err != nil || len(rows) != 1 || rows[0][0] != fmt.Sprint(revision) {
		return ResearchJob{}, errDeskConflict
	}
	// Repeated device clicks attach to an existing identical job.
	rows, err = s.db.rows("SELECT "+jobColumns+" FROM desk_jobs WHERE owner=? AND session_id=? AND cache_key=? AND status IN ('queued','running') ORDER BY updated DESC LIMIT 1", owner, sessionID, key)
	if err != nil {
		return ResearchJob{}, err
	}
	if len(rows) == 1 {
		return jobRows(rows)[0], nil
	}
	rows, err = s.db.rows("SELECT count(*) FROM desk_jobs WHERE status IN ('queued','running')")
	if err != nil {
		return ResearchJob{}, err
	}
	count, _ := strconv.Atoi(rows[0][0])
	if count >= 8 {
		return ResearchJob{}, errors.New("research queue is full; wait for a job to finish")
	}
	j := ResearchJob{ID: id, SessionID: sessionID, SessionRevision: revision, Kind: kind, Status: "queued", Phase: "Waiting for the research worker", Updated: now}
	rows = nil
	if readCache {
		rows, err = s.db.rows("SELECT result FROM desk_cache WHERE owner=? AND key=? AND updated>=?", owner, key, time.Now().UTC().Add(-30*24*time.Hour).Format(time.RFC3339Nano))
		if err != nil {
			return ResearchJob{}, err
		}
	}
	result := ""
	if len(rows) == 1 && kind != "semantic-search" && validateResultModel(rows[0][0], model) != nil {
		// An unbound or mismatched payload is a cache miss, never a response.
		if _, err = s.db.run("DELETE FROM desk_cache WHERE owner=? AND key=?", owner, key); err != nil {
			return ResearchJob{}, err
		}
		rows = nil
	}
	if len(rows) == 1 {
		if kind == "semantic-search" {
			if err = s.validateSemanticCache(rows[0][0], x.Data.Search); err != nil {
				return ResearchJob{}, err
			}
		} else if err = s.i.verifyBoundModel(ctx); err != nil {
			return ResearchJob{}, err
		}
		j.Cached, j.Status, j.Phase = true, "complete", "Reused a verified result; no new model call"
		result = rows[0][0]
		j.Result = json.RawMessage(result)
	}
	_, err = s.db.run("INSERT INTO desk_jobs(id,owner,session_id,revision,kind,status,phase,updated,cached,cache_key,input,result) VALUES(?,?,?,CAST(? AS INTEGER),?,?,?,?,CAST(? AS INTEGER),?,?,?)", id, owner, sessionID, fmt.Sprint(revision), kind, j.Status, j.Phase, now, fmt.Sprint(boolInt(j.Cached)), key, string(input), result)
	if err != nil {
		return ResearchJob{}, err
	}
	// Keep recent results bounded without removing active work.
	_, err = s.db.run("DELETE FROM desk_jobs WHERE owner=? AND status NOT IN ('queued','running') AND id NOT IN (SELECT id FROM desk_jobs WHERE owner=? ORDER BY updated DESC LIMIT 200)", owner, owner)
	if err != nil {
		return ResearchJob{}, err
	}
	if err = s.db.exec("COMMIT"); err == nil && !j.Cached {
		s.signal()
	}
	return j, err
}

func boolInt(x bool) int {
	if x {
		return 1
	}
	return 0
}

func (s *researchDesk) cancelJob(owner, id string) (ResearchJob, error) {
	s.mu.Lock()
	s.db.mu.Lock()
	_, err := s.db.run("UPDATE desk_jobs SET status='cancelled',phase='Cancelled; any started model call remains charged',updated=? WHERE id=? AND owner=? AND status IN ('queued','running')", deskTime(), id, owner)
	s.db.mu.Unlock()
	// Do not cancel another owner's job even if its opaque ID is known.
	j, readErr := s.job(owner, id)
	if readErr == nil && j.Status == "cancelled" {
		if cancel := s.cancel[id]; cancel != nil {
			cancel()
		}
	}
	s.mu.Unlock()
	if err != nil {
		return ResearchJob{}, err
	}
	return j, readErr
}

func (s *researchDesk) worker() {
	defer close(s.done)
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.wake:
		}
		for s.ctx.Err() == nil {
			if !s.workOne() {
				break
			}
		}
	}
}

func (s *researchDesk) workOne() bool {
	s.mu.Lock()
	s.db.mu.Lock()
	rows, err := s.db.rows("SELECT id,owner,kind,cache_key,input FROM desk_jobs WHERE status='queued' ORDER BY updated LIMIT 1")
	if err != nil || len(rows) == 0 {
		s.db.mu.Unlock()
		s.mu.Unlock()
		return false
	}
	r := rows[0]
	id, owner, kind, key := r[0], r[1], r[2], r[3]
	n, err := s.db.run("UPDATE desk_jobs SET status='running',phase='Checking selected sources',updated=? WHERE id=? AND status='queued'", deskTime(), id)
	deadline := 180 * time.Second
	if kind == "graph" {
		deadline = 420 * time.Second
	}
	if kind == "pdf-answer" {
		deadline = 420 * time.Second
	}
	if s.i.limitOwner == "litellm" && s.i.model == "smart-local" {
		deadline = 600 * time.Second
	}
	ctx, cancel := context.WithTimeout(s.ctx, deadline)
	s.cancel[id] = cancel
	s.db.mu.Unlock()
	s.mu.Unlock()
	defer func() { cancel(); s.mu.Lock(); delete(s.cancel, id); s.mu.Unlock() }()
	if err != nil || n != 1 {
		return true
	}
	data, model, inputErr := decodeResearchJobInput([]byte(r[4]))
	err = inputErr
	if err == nil && kind != "semantic-search" {
		if model != nil {
			ctx = s.i.contextWithModel(ctx, *model)
		}
		err = s.i.verifyBoundModel(ctx)
	} else if err == nil && model != nil {
		err = errors.New("semantic job has an invalid chat model binding")
	}
	var currentKey string
	if err == nil {
		currentKey, err = s.cacheKeyForModel(kind, data, model)
	}
	if err == nil && currentKey != key {
		err = errors.New("selected sources or the model contract changed; submit a new job")
	}
	var result any
	cached := false
	if err == nil && kind != "pdf-answer" && kind != "semantic-search" {
		s.db.mu.Lock()
		_, err = s.db.run("UPDATE desk_jobs SET phase='Reading selected abstracts with the local model',updated=? WHERE id=? AND status='running'", deskTime(), id)
		s.db.mu.Unlock()
	}
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	if err == nil {
		if kind == "semantic-search" {
			progress := func(phase string) {
				s.db.mu.Lock()
				_, _ = s.db.run("UPDATE desk_jobs SET phase=?,updated=? WHERE id=? AND status='running'", phase, deskTime(), id)
				s.db.mu.Unlock()
			}
			result, err = s.semanticSearchJob(ctx, owner, data.Search, progress)
		} else if kind == "pdf-answer" {
			progress := func(phase string) {
				s.db.mu.Lock()
				_, _ = s.db.run("UPDATE desk_jobs SET phase=?,updated=? WHERE id=? AND status='running'", phase, deskTime(), id)
				s.db.mu.Unlock()
			}
			result, key, cached, err = s.pdfAnswerJob(ctx, owner, key, data, progress)
		} else if kind == "graph" {
			var v GraphResponse
			progress := func(phase string) {
				s.db.mu.Lock()
				_, _ = s.db.run("UPDATE desk_jobs SET phase=?,updated=? WHERE id=? AND status='running'", phase, deskTime(), id)
				s.db.mu.Unlock()
			}
			v, err = s.i.graphWithProgress(ctx, GraphRequest{Query: data.Question, IDs: data.SelectedIDs, IncludeHypotheses: data.IncludeHypotheses}, progress)
			v.Sources = browserDocuments(v.Sources, s.archive != nil)
			result = v
		} else {
			var v AnswerResponse
			progress := func(phase string) {
				s.db.mu.Lock()
				_, _ = s.db.run("UPDATE desk_jobs SET phase=?,updated=? WHERE id=? AND status='running'", phase, deskTime(), id)
				s.db.mu.Unlock()
			}
			v, err = s.i.answerWithProgress(ctx, AnswerRequest{Question: data.Question, IDs: data.SelectedIDs, Task: data.Task}, progress)
			v.Sources = browserDocuments(v.Sources, s.archive != nil)
			result = v
		}
	}
	status, phase, failure, encoded := "complete", "Evidence checked and result saved", "", []byte{}
	if cached {
		phase = "Reused verified page-cited reading; no new model call"
	}
	if err == nil && kind != "semantic-search" {
		err = s.i.verifyBoundModel(ctx)
	}
	if err == nil {
		encoded, err = json.Marshal(result)
	}
	if err == nil && kind != "semantic-search" {
		err = validateResultModel(string(encoded), model)
	}
	if len(encoded) > 1<<20 {
		err = errors.New("research result exceeds its storage limit")
	}
	if err != nil {
		status, phase, failure = "failed", "Research did not complete", err.Error()
		encoded = nil
		if s.ctx.Err() != nil {
			status, phase, failure = "interrupted", "Interrupted by a server restart", "The server stopped. Retry explicitly; any started model call remains charged."
		}
	}
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	if s.db.exec("BEGIN IMMEDIATE") != nil {
		return true
	}
	defer s.db.exec("ROLLBACK")
	n, err = s.db.run("UPDATE desk_jobs SET status=?,phase=?,updated=?,result=?,error=?,cached=CAST(? AS INTEGER) WHERE id=? AND status='running'", status, phase, deskTime(), string(encoded), failure, fmt.Sprint(boolInt(cached)), id)
	if err != nil {
		return true
	}
	cacheable := true
	if kind == "semantic-search" && status == "complete" {
		v, ok := result.(SearchResponse)
		cacheable = ok && v.Coverage != nil && v.Coverage.State == "complete"
	}
	if n == 1 && status == "complete" && cacheable {
		if _, err = s.db.run("INSERT OR REPLACE INTO desk_cache VALUES(?,?,?,?)", owner, key, deskTime(), string(encoded)); err != nil {
			return true
		}
		if _, err = s.db.run("DELETE FROM desk_cache WHERE owner=? AND key NOT IN (SELECT key FROM desk_cache WHERE owner=? ORDER BY updated DESC LIMIT 64)", owner, owner); err != nil {
			return true
		}
	}
	_ = s.db.exec("COMMIT")
	return true
}

func deskFailure(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, errDeskMissing) {
		status = http.StatusNotFound
	}
	if errors.Is(err, errDeskConflict) {
		status = http.StatusConflict
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func (s *researchDesk) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/sessions", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.list(requestOwner(r))
		if err != nil {
			deskFailure(w, err)
			return
		}
		writeJSON(w, 200, v)
	})
	mux.HandleFunc("GET /api/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.session(requestOwner(r), r.PathValue("id"))
		if err != nil {
			deskFailure(w, err)
			return
		}
		writeJSON(w, 200, v)
	})
	mux.HandleFunc("POST /api/sessions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID       string      `json:"id"`
			Revision int         `json:"revision"`
			Data     SessionData `json:"data"`
		}
		if err := decode(w, r, &req); err != nil {
			deskFailure(w, err)
			return
		}
		v, err := s.save(requestOwner(r), req.ID, req.Revision, req.Data)
		if err != nil {
			deskFailure(w, err)
			return
		}
		writeJSON(w, 200, v)
	})
	mux.HandleFunc("POST /api/jobs", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			SessionID string `json:"session_id"`
			Revision  int    `json:"revision"`
			Kind      string `json:"kind"`
		}
		if err := decode(w, r, &req); err != nil {
			deskFailure(w, err)
			return
		}
		v, err := s.submitWithContext(r.Context(), requestOwner(r), req.SessionID, req.Revision, req.Kind)
		if err != nil {
			deskFailure(w, err)
			return
		}
		writeJSON(w, 202, v)
	})
	mux.HandleFunc("GET /api/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.job(requestOwner(r), r.PathValue("id"))
		if err != nil {
			deskFailure(w, err)
			return
		}
		writeJSON(w, 200, v)
	})
	mux.HandleFunc("POST /api/jobs/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.cancelJob(requestOwner(r), r.PathValue("id"))
		if err != nil {
			deskFailure(w, err)
			return
		}
		writeJSON(w, 200, v)
	})
}

func deskPath(i *inference) string {
	return filepath.Join(filepath.Dir(i.budget.path), "research-desk.sqlite")
}
