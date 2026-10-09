package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Sealed with Python json.dumps(sort_keys=True,separators=(',',':')) and
// hashlib.sha256. This is protocol evidence, not a real loaded-model receipt.
const pythonServingIdentity = `{"aliases":["fixture-model","fast-local"],"artifacts_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","contract":"dtl-vllm-serving-identity-v1","engine_config_sha256":"1111111111111111111111111111111111111111111111111111111111111111","evidence_scope":"verified-read-only-loader-inputs-and-initialized-engine","identity_sha256":"1eed0dfe8f2afeb0d529e5fa5bdcb9cdf6e4ca2f21de7bdebdebad3a1cf5d63e","launch_epoch":"dddddddddddddddddddddddddddddddd","producer_sha256":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","revision":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","serving_epoch":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","vllm_version":"0.28.0+rocm723","worker_epoch_sha256":"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"}`

const identityAnswerContent = `{"reported":{"kind":"reported","text":"","source_ids":["arxiv:1706.03762"],"evidence_quotes":[{"id":"arxiv:1706.03762","quote":"A transformer replaces recurrence with attention for machine translation."}]},"interpretation":{"kind":"interpretation","text":"Attention changes the mechanism.","source_ids":["arxiv:1706.03762"]},"limits":{"kind":"limits","text":"The abstract omits full evaluation details.","source_ids":["arxiv:1706.03762"]}}`

type identityFixture struct {
	mu               sync.Mutex
	server           *httptest.Server
	identity         ServingModelIdentity
	metadataAlias    string
	metadataOverride string
	metadataStatus   int
	headerMode       string
	calls            int
	posts            int
	metadata         int
	beforePost       func()
	afterPost        func()
	reply            func(map[string]any) string
}

func newIdentityFixture(t *testing.T, i *inference) *identityFixture {
	t.Helper()
	f := &identityFixture{reply: func(req map[string]any) string {
		if content, ok := answerReviewFixtureContent(req); ok {
			return content
		}
		return identityAnswerContent
	}}
	if err := json.Unmarshal([]byte(pythonServingIdentity), &f.identity); err != nil {
		t.Fatal(err)
	}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/dtl/model-identity" {
			if f.metadataAlias != "" && r.Header.Get(identityAliasHeader) != f.metadataAlias {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			f.mu.Lock()
			f.metadata++
			m, override, status := f.identity, f.metadataOverride, f.metadataStatus
			f.mu.Unlock()
			if status != 0 {
				w.WriteHeader(status)
			}
			if override != "" {
				_, _ = w.Write([]byte(override))
			} else {
				_ = json.NewEncoder(w).Encode(m)
			}
			return
		}
		f.mu.Lock()
		f.posts++
		before := f.beforePost
		f.mu.Unlock()
		if before != nil {
			before()
		}
		f.mu.Lock()
		m, mode, reply, after := f.identity, f.headerMode, f.reply, f.afterPost
		if r.Header.Get(expectedIdentityHeader) != m.IdentitySHA {
			f.mu.Unlock()
			w.WriteHeader(http.StatusConflict)
			return
		}
		f.calls++
		f.mu.Unlock()
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		switch mode {
		case "missing":
		case "wrong":
			w.Header().Set(responseIdentityHeader, strings.Repeat("9", 64))
		case "duplicate":
			w.Header().Add(responseIdentityHeader, m.IdentitySHA)
			w.Header().Add(responseIdentityHeader, m.IdentitySHA)
		default:
			w.Header().Set(responseIdentityHeader, m.IdentitySHA)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": reply(request)}}}})
		if after != nil {
			after()
		}
	}))
	t.Cleanup(f.server.Close)
	i.answerURL, i.model = f.server.URL+"/v1/chat/completions", "fixture-model"
	i.identityURL, i.artifactsSHA, i.producerSHA = f.server.URL+"/v1/dtl/model-identity", f.identity.ArtifactsSHA, f.identity.ProducerSHA
	return f
}

func (f *identityFixture) change(fn func(*ServingModelIdentity)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.identity)
	f.identity.IdentitySHA = servingIdentityDigest(f.identity)
}

func (f *identityFixture) counts() (int, int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.posts, f.metadata
}

func (f *identityFixture) current() ServingModelIdentity {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.identity
}

func identityUsage(t *testing.T, i *inference) int {
	t.Helper()
	value, err := i.stats()
	if err != nil {
		t.Fatal(err)
	}
	return value.(map[string]any)["model_calls_today"].(int)
}

func TestServingIdentityPythonCanonicalReceiptAndStrictBoundaries(t *testing.T) {
	m, err := decodeServingIdentity([]byte(pythonServingIdentity))
	if err != nil || m.IdentitySHA != "1eed0dfe8f2afeb0d529e5fa5bdcb9cdf6e4ca2f21de7bdebdebad3a1cf5d63e" {
		t.Fatalf("Python receipt rejected: %v", err)
	}
	for _, bad := range []string{
		pythonServingIdentity + ` {}`, strings.Replace(pythonServingIdentity, `"contract":`, `"contract":"duplicate","contract":`, 1),
		strings.Replace(pythonServingIdentity, `"contract":`, `"unexpected":"s3://private-path","contract":`, 1),
		strings.Replace(pythonServingIdentity, `"revision":"bbbb`, `"revision":"cbbb`, 1),
		`null`, `{}`, strings.Replace(pythonServingIdentity, `["fixture-model","fast-local"]`, `null`, 1),
		strings.Replace(pythonServingIdentity, `0.28.0+rocm723`, `0.28.0<script>`, 1),
	} {
		if _, err := decodeServingIdentity([]byte(bad)); err == nil {
			t.Fatal("unsafe or forged receipt accepted")
		}
	}
	changed := m
	changed.ServingEpoch = strings.Repeat("2", 32)
	changed.IdentitySHA = servingIdentityDigest(changed)
	if !changed.valid() || changed.IdentitySHA == m.IdentitySHA {
		t.Fatal("API epoch did not invalidate identity")
	}
	changed = m
	changed.WorkerEpochSHA = strings.Repeat("2", 64)
	changed.IdentitySHA = servingIdentityDigest(changed)
	if !changed.valid() || changed.IdentitySHA == m.IdentitySHA {
		t.Fatal("worker epoch did not invalidate identity")
	}
}

func TestServingIdentityRequiresNativeOriginAndBothReviewedFingerprints(t *testing.T) {
	i := &inference{answerURL: "http://model.local:8000/v1/chat/completions", model: "fixture-model"}
	if err := i.validateIdentityConfiguration(); err != nil {
		t.Fatal(err)
	}
	i.identityURL = "http://model.local:8000/v1/dtl/model-identity"
	if err := i.validateIdentityConfiguration(); err == nil {
		t.Fatal("partial configuration accepted")
	}
	i.artifactsSHA, i.producerSHA = strings.Repeat("a", 64), strings.Repeat("c", 64)
	if err := i.validateIdentityConfiguration(); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"http://elsewhere:8000/v1/dtl/model-identity", "https://model.local:8000/v1/dtl/model-identity", "http://key@model.local:8000/v1/dtl/model-identity", "http://model.local:8000/v1/dtl/model-identity?secret=x", "http://model.local:8000/v1/models", "http://model.local:8000/v1/%64tl/model-identity"} {
		i.identityURL = endpoint
		if err := i.validateIdentityConfiguration(); err == nil {
			t.Fatal("unsafe identity origin or path accepted")
		}
	}
}

func TestServingIdentityRejectsMetadataFailureBeforeReservation(t *testing.T) {
	for _, kind := range []string{"producer", "artifacts", "alias", "oversized", "unknown-field", "unauthorized", "malformed", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			i := testInference(t, testDB(t))
			f := newIdentityFixture(t, i)
			ctx := context.Background()
			switch kind {
			case "producer":
				f.change(func(m *ServingModelIdentity) { m.ProducerSHA = strings.Repeat("8", 64) })
			case "artifacts":
				f.change(func(m *ServingModelIdentity) { m.ArtifactsSHA = strings.Repeat("8", 64) })
			case "alias":
				f.change(func(m *ServingModelIdentity) { m.Aliases = []string{"different-model"} })
			case "oversized":
				f.metadataOverride = strings.Repeat("x", 16*1024+1)
			case "unknown-field":
				f.metadataOverride = strings.Replace(pythonServingIdentity, `"contract":`, `"internal_path":"s3://private-key","contract":`, 1)
			case "unauthorized":
				f.metadataStatus = 401
				f.metadataOverride = "private-key"
			case "malformed":
				f.metadataOverride = "private-key"
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			var out map[string]any
			err := i.post(ctx, i.answerURL, "answer", map[string]string{"model": i.model}, 100, &out)
			if err == nil || strings.Contains(err.Error(), "private-key") || strings.Contains(err.Error(), "s3://") {
				t.Fatalf("identity failure not sanitized: %v", err)
			}
			calls, posts, _ := f.counts()
			if calls != 0 || posts != 0 || identityUsage(t, i) != 0 || out != nil {
				t.Fatal("invalid identity dispatched or reserved a model call")
			}
		})
	}
}

func TestServingIdentityResponseMismatchNeverProducesResultButRetainsReservation(t *testing.T) {
	for _, mode := range []string{"missing", "wrong", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			i := testInference(t, testDB(t))
			f := newIdentityFixture(t, i)
			f.headerMode = mode
			var out map[string]any
			if err := i.post(context.Background(), i.answerURL, "answer", map[string]string{"model": i.model}, 100, &out); err == nil || out != nil {
				t.Fatal("unbound response accepted")
			}
			calls, _, _ := f.counts()
			if calls != 1 || identityUsage(t, i) != 1 {
				t.Fatal("uncertain completion refunded or repeated")
			}
		})
	}
}

func TestServingIdentitySwitchBetweenPreflightAndPOSTFailsClosed(t *testing.T) {
	i := testInference(t, testDB(t))
	f := newIdentityFixture(t, i)
	f.beforePost = func() { f.change(func(m *ServingModelIdentity) { m.WorkerEpochSHA = strings.Repeat("2", 64) }) }
	var out map[string]any
	if err := i.post(context.Background(), i.answerURL, "answer", map[string]string{"model": i.model}, 100, &out); err == nil || out != nil {
		t.Fatal("identity switch at dispatch accepted")
	}
	calls, posts, _ := f.counts()
	if calls != 0 || posts != 1 || identityUsage(t, i) != 1 {
		t.Fatal("dispatch race lost durable reservation or reached inference")
	}
}

func TestDeskModelBindingPersistsAndInvalidatesCacheForActualEpochs(t *testing.T) {
	s, i := deskFixture(t)
	f := newIdentityFixture(t, i)
	x, err := s.save("owner", "", 0, sessionFixture())
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 2; n++ {
		j, err := s.submit("owner", x.ID, x.Revision, "answer")
		if err != nil {
			t.Fatal(err)
		}
		j = awaitDeskJob(t, s, "owner", j.ID)
		if j.Status != "complete" || j.Cached != (n == 1) || validateResultModel(string(j.Result), ptrIdentity(f.current())) != nil {
			t.Fatalf("bound job/cache: %+v", j)
		}
	}
	if identityUsage(t, i) != 2 {
		t.Fatal("metadata or cache hit charged inference usage")
	}
	copy, err := openDatabase(s.db.path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer copy.close()
	rows, err := copy.rows("SELECT input,result FROM desk_jobs WHERE owner=?", "owner")
	if err != nil || len(rows) != 2 {
		t.Fatal("bound input or result did not persist")
	}
	for _, row := range rows {
		_, m, err := decodeResearchJobInput([]byte(row[0]))
		if err != nil || m == nil || m.IdentitySHA != f.current().IdentitySHA {
			t.Fatal("queued binding did not persist")
		}
		if strings.Contains(row[0]+row[1], f.server.URL) || strings.Contains(row[0]+row[1], "nas_ref") {
			t.Fatal("private endpoint or NAS reference in stored receipt")
		}
	}
	for _, change := range []func(*ServingModelIdentity){
		func(m *ServingModelIdentity) { m.ServingEpoch = strings.Repeat("2", 32) },
		func(m *ServingModelIdentity) { m.WorkerEpochSHA = strings.Repeat("3", 64) },
	} {
		f.change(change)
		j, err := s.submit("owner", x.ID, x.Revision, "answer")
		if err != nil {
			t.Fatal(err)
		}
		j = awaitDeskJob(t, s, "owner", j.ID)
		if j.Status != "complete" || j.Cached {
			t.Fatal("old epoch satisfied cache lookup")
		}
	}
	if identityUsage(t, i) != 6 {
		t.Fatal("restart cache invalidation did not make one generation and one review per epoch")
	}
}

func ptrIdentity(m ServingModelIdentity) *ServingModelIdentity { return &m }

func TestDeskQueuedBindingSurvivesResearchRestartAndRejectsModelRestartOrDowngrade(t *testing.T) {
	for _, kind := range []string{"stable", "model-restart", "configuration-removed", "legacy-activation"} {
		t.Run(kind, func(t *testing.T) {
			s, i := deskFixture(t)
			f := newIdentityFixture(t, i)
			s.stop()
			<-s.done // Stop this worker before persisting a queued job.
			if kind == "legacy-activation" {
				i.identityURL, i.artifactsSHA, i.producerSHA = "", "", ""
			}
			x, err := s.save("owner", "", 0, sessionFixture())
			if err != nil {
				t.Fatal(err)
			}
			j, err := s.submit("owner", x.ID, x.Revision, "answer")
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "model-restart":
				f.change(func(m *ServingModelIdentity) { m.ServingEpoch = strings.Repeat("2", 32) })
			case "configuration-removed":
				i.identityURL, i.artifactsSHA, i.producerSHA = "", "", ""
			case "legacy-activation":
				i.identityURL, i.artifactsSHA, i.producerSHA = f.server.URL+"/v1/dtl/model-identity", f.current().ArtifactsSHA, f.current().ProducerSHA
			}
			reopened, err := openResearchDesk(s.db.path, i, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.close()
			j = awaitDeskJob(t, reopened, "owner", j.ID)
			calls, _, _ := f.counts()
			if kind == "stable" {
				if j.Status != "complete" || calls != 2 {
					t.Fatalf("stable queued binding lost on app restart: %+v", j)
				}
				repeat, err := reopened.submit("owner", x.ID, x.Revision, "answer")
				if err != nil || !repeat.Cached || identityUsage(t, i) != 2 {
					t.Fatal("stable model lost cache after research restart")
				}
			} else if j.Status != "failed" || calls != 0 || identityUsage(t, i) != 0 || len(j.Result) != 0 {
				t.Fatalf("queued stale or downgraded binding ran: %+v", j)
			}
		})
	}
}

func TestDeskMismatchedCachePayloadIsMissAndFailedResponseCannotCache(t *testing.T) {
	s, i := deskFixture(t)
	f := newIdentityFixture(t, i)
	x, err := s.save("owner", "", 0, sessionFixture())
	if err != nil {
		t.Fatal(err)
	}
	key, err := s.cacheKey("answer", x.Data)
	if err != nil {
		t.Fatal(err)
	}
	s.db.mu.Lock()
	_, err = s.db.run("INSERT INTO desk_cache VALUES(?,?,?,?)", "owner", key, deskTime(), `{"answer":"old unbound fixture result"}`)
	s.db.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	f.headerMode = "missing"
	j, err := s.submit("owner", x.ID, x.Revision, "answer")
	if err != nil {
		t.Fatal(err)
	}
	j = awaitDeskJob(t, s, "owner", j.ID)
	if j.Cached || j.Status != "failed" || len(j.Result) != 0 {
		t.Fatal("mismatched result returned or uncertain response saved")
	}
	s.db.mu.Lock()
	rows, err := s.db.rows("SELECT count(*) FROM desk_cache WHERE owner=?", "owner")
	s.db.mu.Unlock()
	if err != nil || rows[0][0] != "0" {
		t.Fatal("failed identity verification entered cache")
	}
	if identityUsage(t, i) != 1 {
		t.Fatal("failed model response reservation lost")
	}
}

func TestBoundGraphAndComparisonKeepOneIdentityAcrossGenerationAndReview(t *testing.T) {
	for _, kind := range []string{"graph", "comparison"} {
		for _, switchModel := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: "-stable", true: "-switch"}[switchModel], func(t *testing.T) {
				s, i := deskFixture(t)
				f := newIdentityFixture(t, i)
				data := sessionFixture()
				data.SelectedIDs = []string{"arxiv:1706.03762", "arxiv:2005.11401"}
				jobKind := "graph"
				if kind == "graph" {
					_, wire := graphFixture()
					encoded, _ := json.Marshal(wire)
					f.reply = func(req map[string]any) string {
						if content, ok := graphEvidenceFixtureContent(req); ok {
							return content
						}
						name := req["response_format"].(map[string]any)["json_schema"].(map[string]any)["name"]
						if name == "graph_relation_review" {
							return `{"decisions":[{"edge_id":"edge:4","verdict":"supported","reason":"direct-evidence"}]}`
						}
						return string(encoded)
					}
				} else {
					p := comparisonFixture(t)
					content := encodeComparison(t, comparisonFixtureWire(p))
					f.reply = func(req map[string]any) string { return comparisonModelReply(t, req, content) }
					data.Task = "compare"
					jobKind = "answer"
				}
				if switchModel {
					f.afterPost = func() { f.change(func(m *ServingModelIdentity) { m.WorkerEpochSHA = strings.Repeat("2", 64) }) }
				}
				x, err := s.save("owner", "", 0, data)
				if err != nil {
					t.Fatal(err)
				}
				j, err := s.submit("owner", x.ID, x.Revision, jobKind)
				if err != nil {
					t.Fatal(err)
				}
				j = awaitDeskJob(t, s, "owner", j.ID)
				calls, _, _ := f.counts()
				if switchModel {
					if j.Status != "failed" || calls != 1 || len(j.Result) != 0 || identityUsage(t, i) != 1 {
						t.Fatalf("mixed-model result or unnecessary review reservation: %+v calls=%d", j, calls)
					}
				} else {
					wantCalls := 2
					if kind == "graph" {
						wantCalls = 3
					}
					if j.Status != "complete" || calls != wantCalls || validateResultModel(string(j.Result), ptrIdentity(f.current())) != nil {
						t.Fatalf("same identity not retained: %+v", j)
					}
					repeat, err := s.submit("owner", x.ID, x.Revision, jobKind)
					if err != nil || !repeat.Cached || identityUsage(t, i) != wantCalls {
						t.Fatal("bound reviewed result not cached")
					}
				}
			})
		}
	}
}

func TestBoundPDFReadingRetainsIdentityAndPhysicalPageReceipt(t *testing.T) {
	i := testInference(t, testDB(t))
	f := newIdentityFixture(t, i)
	p := readingFixture()
	f.reply = func(map[string]any) string { return readingContent(2, p.Sources[0].Pages[1].Choices[0]) }
	result, err := i.readingAnswer(context.Background(), SessionData{Question: "How are river channels linked to erosion?", Task: "explain"}, p)
	if err != nil || result.ModelIdentity == nil || result.ModelIdentity.IdentitySHA != f.current().IdentitySHA || result.Statements[0].Citations[0].Page != 2 {
		t.Fatalf("PDF identity or physical citation lost: %+v %v", result, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = i.readingAnswer(ctx, SessionData{Question: "How are river channels linked to erosion?", Task: "explain"}, p)
	if !errors.Is(err, context.Canceled) || identityUsage(t, i) != 1 {
		t.Fatal("cancelled metadata lookup reserved a reading call")
	}
}

func TestModelIdentityContextSnapshotsCannotMixOrMutateParallelRequests(t *testing.T) {
	i := testInference(t, testDB(t))
	f := newIdentityFixture(t, i)
	old, err := i.bindModel(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	original := modelIdentityFromContext(old).IdentitySHA
	copy := modelIdentityFromContext(old)
	copy.Aliases[0] = "changed-outside-context"
	if !modelIdentityFromContext(old).valid() {
		t.Fatal("returned receipt mutated the private context")
	}
	f.change(func(m *ServingModelIdentity) { m.ServingEpoch = strings.Repeat("2", 32) })
	current, err := i.bindModel(context.Background())
	if err != nil || modelIdentityFromContext(current).IdentitySHA == original {
		t.Fatal("new context did not receive the current epoch")
	}
	var group sync.WaitGroup
	group.Add(2)
	go func() {
		defer group.Done()
		var out map[string]any
		if err := i.post(old, i.answerURL, "answer", map[string]string{"model": i.model}, 100, &out); err == nil || out != nil {
			t.Error("old context silently adopted the new identity")
		}
	}()
	go func() {
		defer group.Done()
		var out map[string]any
		if err := i.post(current, i.answerURL, "answer", map[string]string{"model": i.model}, 100, &out); err != nil || out == nil {
			t.Error("current context failed", err)
		}
	}()
	group.Wait()
	if modelIdentityFromContext(old).IdentitySHA != original || identityUsage(t, i) != 1 {
		t.Fatal("parallel contexts mixed identity or reservations")
	}
}

func TestConfiguredIdentityOperatorReadsMetadataWithoutCatalogOrInference(t *testing.T) {
	i := testInference(t, testDB(t))
	f := newIdentityFixture(t, i)
	for name, value := range map[string]string{
		"RESEARCH_CHAT_URL": i.answerURL, "RESEARCH_CHAT_MODEL": i.model,
		"RESEARCH_MODEL_IDENTITY_URL": i.identityURL, "RESEARCH_MODEL_ARTIFACTS_SHA256": i.artifactsSHA,
		"RESEARCH_MODEL_PRODUCER_SHA256": i.producerSHA,
	} {
		t.Setenv(name, value)
	}
	m, err := configuredServingIdentity(context.Background())
	if err != nil || m.IdentitySHA != f.current().IdentitySHA {
		t.Fatal("operator could not verify native receipt", err)
	}
	calls, posts, metadata := f.counts()
	if calls != 0 || posts != 0 || metadata != 1 || identityUsage(t, i) != 0 {
		t.Fatal("metadata command dispatched inference or touched its budget")
	}
	if err := run([]string{"model-identity", "--db", "/does/not/exist"}); err == nil {
		t.Fatal("metadata command accepts file or endpoint overrides")
	}
}

func TestQualityRunUsesOneBindingAndAliasOnlyReceiptIsStaleAfterActivation(t *testing.T) {
	for _, switchModel := range []bool{false, true} {
		t.Run(map[bool]string{false: "stable", true: "switch"}[switchModel], func(t *testing.T) {
			i, _ := qualityFixture(t)
			f := newIdentityFixture(t, i)
			f.reply = func(body map[string]any) string {
				messages := body["messages"].([]any)
				var inputs []comparisonReviewInput
				if err := json.Unmarshal([]byte(messages[1].(map[string]any)["content"].(string)), &inputs); err != nil {
					t.Error(err)
				}
				decisions := []ComparisonReview{}
				for _, input := range inputs {
					decisions = append(decisions, ComparisonReview{input.Category, input.Index, "supported", "direct-evidence"})
				}
				data, _ := json.Marshal(map[string]any{"decisions": decisions})
				return string(data)
			}
			if switchModel {
				f.afterPost = func() { f.change(func(m *ServingModelIdentity) { m.WorkerEpochSHA = strings.Repeat("2", 64) }) }
			}
			r, err := i.benchmarkQuality(context.Background(), qualityPath(i))
			if switchModel {
				if err == nil || r.Status == "complete" || identityUsage(t, i) != 1 {
					t.Fatal("mixed-model benchmark completed or reserved another review")
				}
				return
			}
			if err != nil || r.ModelIdentity == nil || r.ModelCalls != 4 || identityUsage(t, i) != 4 {
				t.Fatal("benchmark model binding lost", err)
			}
			_, _, before := f.counts()
			got, err := readQuality(i)
			_, _, after := f.counts()
			if err != nil || got.Status != "complete" || after != before {
				t.Fatal("bound receipt read made a model or metadata request", err)
			}
			r.ModelIdentity = nil
			if err := publishQuality(qualityPath(i), r); err != nil {
				t.Fatal(err)
			}
			got, err = readQuality(i)
			if err != nil || got.Status != "stale" {
				t.Fatal("alias-only measurement accepted for reviewed weights", err)
			}
		})
	}
}

func TestServingIdentityGatewayRequiresPrivateApprovedOriginAndAlias(t *testing.T) {
	t.Setenv("RESEARCH_LITELLM_ORIGIN", "http://litellm.models.svc.cluster.local:4000")
	i := &inference{answerURL: "http://litellm.models.svc.cluster.local:4000/v1/chat/completions", model: "fast-local", limitOwner: "litellm", key: "fixture-key", identityURL: "http://litellm.models.svc.cluster.local:4000/v1/dtl/model-identity", artifactsSHA: strings.Repeat("a", 64), producerSHA: strings.Repeat("c", 64)}
	if err := i.validateIdentityConfiguration(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*inference){
		func(i *inference) { i.key = "" },
		func(i *inference) { i.model = "planner" },
		func(i *inference) {
			i.answerURL = "http://other-gateway:4000/v1/chat/completions"
			i.identityURL = "http://other-gateway:4000/v1/dtl/model-identity"
		},
		func(i *inference) { i.identityURL += "?model=fast-local" },
	} {
		bad := *i
		change(&bad)
		if err := bad.validateIdentityConfiguration(); err == nil {
			t.Fatal("unsafe gateway identity configuration accepted")
		}
	}
}

func TestGatewayIdentityAliasKeepsEpochBoundDeskCache(t *testing.T) {
	s, i := deskFixture(t)
	f := newIdentityFixture(t, i)
	i.model, i.limitOwner, i.key = "fast-local", "litellm", "fixture-key"
	f.metadataAlias = "fast-local"
	x, err := s.save("owner", "", 0, sessionFixture())
	if err != nil {
		t.Fatal(err)
	}
	submit := func(cached bool) {
		t.Helper()
		j, err := s.submit("owner", x.ID, x.Revision, "answer")
		if err != nil {
			t.Fatal(err)
		}
		j = awaitDeskJob(t, s, "owner", j.ID)
		if j.Status != "complete" || j.Cached != cached || validateResultModel(string(j.Result), ptrIdentity(f.current())) != nil {
			t.Fatalf("gateway bound job/cache: %+v", j)
		}
	}
	submit(false)
	submit(true)
	if identityUsage(t, i) != 2 {
		t.Fatal("gateway metadata or matching cache hit charged inference")
	}
	f.change(func(m *ServingModelIdentity) { m.WorkerEpochSHA = strings.Repeat("2", 64) })
	submit(false)
	if identityUsage(t, i) != 4 {
		t.Fatal("new worker epoch reused old gateway cache")
	}
	f.change(func(m *ServingModelIdentity) { m.ProducerSHA = strings.Repeat("8", 64) })
	if _, err := s.submit("owner", x.ID, x.Revision, "answer"); err == nil {
		t.Fatal("changed native producer accepted through gateway")
	}
	if identityUsage(t, i) != 4 {
		t.Fatal("rejected gateway identity charged inference")
	}
}
