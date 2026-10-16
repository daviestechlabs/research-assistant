package main

// Indexing is an offline plane. The browser selects configured collection IDs;
// it never supplies URLs, credentials, file paths, SQL, or executable commands.
import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type indexCollection struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Source    string `json:"source"`
	Adapter   string `json:"adapter"`
	Endpoint  string `json:"endpoint"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix"`
	Key       string `json:"key"`
	AccessEnv string `json:"access_env"`
	SecretEnv string `json:"secret_env"`
	Region    string `json:"region"`
}
type indexConfig struct {
	Collections        []indexCollection `json:"collections"`
	Admins             []string          `json:"admin_subjects"`
	EmbeddingOrigin    string            `json:"embedding_origin"`
	VectorOrigin       string            `json:"vector_origin"`
	ModelSHA           string            `json:"model_sha256"`
	DocumentsPerSecond int               `json:"documents_per_second"`
	MaxObjects         int               `json:"max_objects"`
	MaxCatalogMiB      int               `json:"max_catalog_mib"`
	LinkSamples        []string          `json:"link_samples"`
}
type indexObject struct {
	Key   string `json:"key"`
	ETag  string `json:"etag"`
	Bytes int64  `json:"bytes"`
	Rows  int64  `json:"rows"`
}
type indexInventory struct {
	ObservedObjects    int64  `json:"observed_objects"`
	ObservedBytes      int64  `json:"observed_bytes"`
	MetadataCandidates int64  `json:"metadata_candidates"`
	Cursor             string `json:"cursor,omitempty"`
	ListingComplete    bool   `json:"listing_complete"`

	ObjectCount      int           `json:"object_count"`
	Collection       string        `json:"collection"`
	Objects          []indexObject `json:"objects"`
	Records          int64         `json:"records"`
	Existing         int64         `json:"existing"`
	MissingSummaries int64         `json:"missing_summaries"`
}
type indexRun struct {
	Automatic   bool            `json:"automatic,omitempty"`
	UpdateSHA   string          `json:"update_sha256,omitempty"`
	ID          string          `json:"id"`
	Kind        string          `json:"kind"`
	Collections []string        `json:"collections"`
	Meaning     bool            `json:"meaning"`
	ConfigSHA   string          `json:"config_sha256"`
	Base        indexGeneration `json:"base"`
	Created     string          `json:"created"`
}
type indexState struct {
	ValidationProgress *indexValidationProgress `json:"validation_progress,omitempty"`
	PrunedAt           string                   `json:"pruned_at,omitempty"`
	BaselineSHA        string                   `json:"baseline_sha256"`
	ReusedVectors      int64                    `json:"reused_vectors"`
	NewEmbeddings      int64                    `json:"new_embeddings"`
	ID                 string                   `json:"id"`
	Status             string                   `json:"status"`
	Phase              string                   `json:"phase"`
	Updated            string                   `json:"updated"`
	Error              string                   `json:"error,omitempty"`
	Inventory          []indexInventory         `json:"inventory"`
	Collection         int                      `json:"collection_offset"`
	Object             int                      `json:"object_offset"`
	Row                int64                    `json:"row_offset"`
	Read               int64                    `json:"records_read"`
	Duplicates         int64                    `json:"duplicate_records"`
	Missing            int64                    `json:"missing_summaries"`
	Vectors            int64                    `json:"vectors"`
	LastVectorRow      int64                    `json:"last_vector_row"`
	Validation         map[string]any           `json:"validation,omitempty"`
	Candidate          *indexGeneration         `json:"candidate,omitempty"`
}
type indexGeneration struct {
	UpdateSHA string `json:"update_sha256,omitempty"`
	Parent    string `json:"parent,omitempty"`
	ParentSHA string `json:"parent_sha256,omitempty"`
	ID        string `json:"id"`
	Catalog   string `json:"catalog"`
	SHA       string `json:"sha256"`
	Semantic  string `json:"semantic_status,omitempty"`
	ModelSHA  string `json:"model_sha256,omitempty"`
}
type indexActive struct {
	Current  indexGeneration  `json:"current"`
	Previous *indexGeneration `json:"previous,omitempty"`
}
type indexManager struct {
	reloading atomic.Bool
	admission sync.RWMutex
	root      string
	config    indexConfig
	configSHA string
	base      indexGeneration
	mu        sync.Mutex
	restart   func()
	desk      *researchDesk
}

var collectionID = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)
var envID = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,100}$`)

func readIndexJSON(path string, dst any) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, 8<<20+1))
	if e != nil || len(b) > 8<<20 {
		return errors.New("index receipt exceeds its limit")
	}
	return json.Unmarshal(b, dst)
}
func writeIndexJSON(path string, src any) error {
	b, e := json.Marshal(src)
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".index-*")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e == nil {
		e = closeErr
	}
	if e != nil {
		return e
	}
	if e = os.Rename(name, path); e != nil {
		return e
	}
	dir, e := os.Open(filepath.Dir(path))
	if e != nil {
		return e
	}
	defer dir.Close()
	return dir.Sync()
}
func indexLock(path string) (*os.File, error) {
	f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, errors.New("another indexing worker or control operation is active")
	}
	return f, nil
}
func indexUnlock(f *os.File) { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }
func loadIndexConfig(path string) (indexConfig, string, error) {
	var c indexConfig
	if e := readIndexJSON(path, &c); e != nil {
		return c, "", errors.New("indexing configuration is unavailable")
	}
	if len(c.Collections) < 1 || len(c.Collections) > 32 || len(c.Admins) < 1 || len(c.Admins) > 32 {
		return c, "", errors.New("indexing needs configured collections and explicit administrators")
	}
	if c.MaxObjects == 0 {
		c.MaxObjects = 1000
	}
	if c.MaxCatalogMiB == 0 {
		c.MaxCatalogMiB = 16384
	}
	if c.DocumentsPerSecond == 0 {
		c.DocumentsPerSecond = 5
	}
	if c.MaxObjects < 1 || c.MaxObjects > 10000 || c.MaxCatalogMiB < 64 || c.MaxCatalogMiB > 65536 || c.DocumentsPerSecond < 1 || c.DocumentsPerSecond > 30 || len(c.LinkSamples) > 8 {
		return c, "", errors.New("invalid offline indexing limits")
	}
	seen := map[string]bool{}
	for _, v := range c.Collections {
		if !collectionID.MatchString(v.ID) || seen[v.ID] || len(v.Label) > 120 || (v.Source != "arxiv" && v.Source != "gutenberg" && !(v.Source == "inventory" && v.Adapter == "inventory")) || !envID.MatchString(v.AccessEnv) || !envID.MatchString(v.SecretEnv) {
			return c, "", errors.New("invalid indexing collection")
		}
		seen[v.ID] = true
		if _, e := serviceOrigin(v.Endpoint); e != nil {
			return c, "", errors.New("indexing storage requires a fixed origin")
		}
		if v.Bucket == "" || strings.ContainsAny(v.Bucket, "/\\?#") || !safeIndexKey(v.Key) && v.Key != "" || !safeIndexKey(strings.TrimSuffix(v.Prefix, "/")) && v.Prefix != "" || v.Key == "" && v.Prefix == "" && v.Adapter != "inventory" {
			return c, "", errors.New("indexing needs a fixed metadata key or prefix")
		}
		if v.Adapter != "jsonl" && v.Adapter != "arxiv-parquet" && v.Adapter != "gutenberg-manifest" && v.Adapter != "inventory" || v.Adapter == "arxiv-parquet" && v.Source != "arxiv" || v.Adapter == "gutenberg-manifest" && v.Source != "gutenberg" {
			return c, "", errors.New("unsupported metadata adapter")
		}
	}
	if c.EmbeddingOrigin != "" || c.VectorOrigin != "" || c.ModelSHA != "" {
		if _, e := serviceOrigin(c.EmbeddingOrigin); e != nil {
			return c, "", e
		}
		if _, e := serviceOrigin(c.VectorOrigin); e != nil {
			return c, "", e
		}
		if !shaPattern.MatchString(c.ModelSHA) {
			return c, "", errors.New("embedding model identity is required")
		}
	}
	b, _ := json.Marshal(c)
	return c, textSHA(b), nil
}
func safeIndexKey(k string) bool {
	return k != "" && !strings.HasPrefix(k, "/") && filepath.Clean(k) == k && !strings.ContainsAny(k, "\\?#\r\n") && !strings.Contains(k, "..")
}
func newIndexManager(base string, desk *researchDesk) (*indexManager, error) {
	root, config := os.Getenv("RESEARCH_INDEX_ROOT"), os.Getenv("RESEARCH_INDEX_CONFIG")
	if root == "" && config == "" {
		return nil, nil
	}
	if !filepath.IsAbs(root) || !filepath.IsAbs(config) {
		return nil, errors.New("indexing requires absolute configuration and NAS state paths")
	}
	c, sha, e := loadIndexConfig(config)
	if e != nil {
		return nil, e
	}
	if e = os.MkdirAll(filepath.Join(root, "runs"), 0700); e != nil {
		return nil, e
	}
	m := &indexManager{root: root, config: c, configSHA: sha, desk: desk, base: indexGeneration{ID: "original", Catalog: base, SHA: os.Getenv("RESEARCH_CATALOG_SHA256"), Semantic: os.Getenv("RESEARCH_SEMANTIC_STATUS"), ModelSHA: os.Getenv("RESEARCH_EMBEDDING_SHA256")}}
	var active indexActive
	if e = readIndexJSON(filepath.Join(root, "active.json"), &active); e == nil {
		m.base = active.Current
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	return m, nil
}

// Run before opening the serving catalog. Usage and session paths stay independent.
func resolveIndexCatalog(base string) (string, error) {
	root := os.Getenv("RESEARCH_INDEX_ROOT")
	if root == "" {
		return base, nil
	}
	var a indexActive
	e := readIndexJSON(filepath.Join(root, "active.json"), &a)
	if os.IsNotExist(e) {
		return base, nil
	}
	if e != nil {
		return "", e
	}
	if e = validGeneration(root, a.Current, base); e != nil {
		return "", e
	}
	if a.Current.Semantic == "" {
		for _, key := range []string{"RESEARCH_EMBEDDING_ORIGIN", "RESEARCH_VECTOR_ORIGIN", "RESEARCH_SEMANTIC_STATUS", "RESEARCH_CATALOG_SHA256", "RESEARCH_EMBEDDING_SHA256"} {
			os.Unsetenv(key)
		}
	} else {
		c, _, e := loadIndexConfig(os.Getenv("RESEARCH_INDEX_CONFIG"))
		if e != nil {
			return "", e
		}
		os.Setenv("RESEARCH_EMBEDDING_ORIGIN", c.EmbeddingOrigin)
		os.Setenv("RESEARCH_VECTOR_ORIGIN", c.VectorOrigin)
		os.Setenv("RESEARCH_SEMANTIC_STATUS", a.Current.Semantic)
		os.Setenv("RESEARCH_CATALOG_SHA256", a.Current.SHA)
		os.Setenv("RESEARCH_EMBEDDING_SHA256", a.Current.ModelSHA)
	}
	return a.Current.Catalog, nil
}
func validGeneration(root string, g indexGeneration, original string) error {
	if !shaPattern.MatchString(g.SHA) {
		return errors.New("generation checksum is missing")
	}
	if g.ID == "original" {
		if g.Catalog != original {
			return errors.New("original catalog changed")
		}
	} else if !deskID.MatchString(g.ID) || g.Catalog != filepath.Join(root, "runs", g.ID, "catalog.sqlite") || g.Semantic != "" && g.Semantic != filepath.Join(root, "runs", g.ID, "semantic.json") {
		return errors.New("invalid generation path")
	}
	sha, e := indexFileSHA(g.Catalog)
	if e != nil || sha != g.SHA {
		return errors.New("generation catalog failed checksum verification")
	}
	if g.ID != "original" && g.Semantic != "" {
		var c SemanticCoverage
		if readIndexJSON(g.Semantic, &c) != nil || c.State != "complete" || c.Total < 1 || c.Indexed != c.Total || c.VerifiedCount != c.Total || c.CatalogSHA != g.SHA || c.ModelSHA != g.ModelSHA || c.Algorithm != semanticAlgorithm || c.Dimensions != semanticDimensions || c.Collection != semanticCollection(g.SHA, g.ModelSHA) {
			return errors.New("candidate meaning coverage is not complete and verified")
		}
		expected, _ := json.Marshal(semanticEngine())
		observed, _ := json.Marshal(c.Engine)
		if string(expected) != string(observed) {
			return errors.New("candidate embedding contract changed")
		}
	}
	return nil
}
func (m *indexManager) runPath(id, name string) string {
	return filepath.Join(m.root, "runs", id, name)
}
func (m *indexManager) authorized(r *http.Request) bool {
	for _, s := range m.config.Admins {
		if s == requestOwner(r) {
			return true
		}
	}
	return false
}
func (m *indexManager) states() ([]indexState, error) {
	entries, e := os.ReadDir(filepath.Join(m.root, "runs"))
	if e != nil {
		return nil, e
	}
	var out []indexState
	for _, v := range entries {
		if v.IsDir() && deskID.MatchString(v.Name()) {
			var s indexState
			if e = readIndexJSON(m.runPath(v.Name(), "state.json"), &s); e != nil {
				return nil, errors.New("an indexing receipt is unreadable")
			}
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated > out[j].Updated })
	return out, nil
}
func (m *indexManager) submit(kind string, ids []string, meaning bool) (indexState, error) {
	return m.submitRun(kind, ids, meaning, false)
}
func (m *indexManager) submitRun(kind string, ids []string, meaning, automatic bool) (indexState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var state indexState
	if m.reloading.Load() {
		return state, errors.New("catalog reload is in progress")
	}
	lock, e := indexLock(filepath.Join(m.root, "control.lock"))
	if e != nil {
		return state, e
	}
	defer indexUnlock(lock)
	update := kind == "arxiv-update"
	if automatic && !update {
		return state, errors.New("automatic updates require the arXiv update workflow")
	}
	if update && os.Getenv("RESEARCH_ARXIV_UPDATES_ENABLED") != "true" {
		return state, errors.New("the indexing worker must enable arXiv updates after the current rebuild finishes")
	}
	if update && (syncRoot() == "" || len(ids) != 0 || !meaning) {
		return state, errors.New("arXiv updates require configured harvesting and meaning search")
	}
	if !update && kind != "inventory" && kind != "rebuild" || !update && len(ids) < 1 || len(ids) > 32 {
		return state, errors.New("choose inventory or rebuild and configured collections")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		found := false
		for _, c := range m.config.Collections {
			if c.ID == id {
				found = true
				if kind == "rebuild" && c.Adapter == "inventory" {
					return state, errors.New("inventory-only collections need a reviewed metadata adapter before rebuilding")
				}
			}
		}
		if !found || seen[id] {
			return state, errors.New("unknown or duplicate collection")
		}
		seen[id] = true
	}
	if meaning && m.config.ModelSHA == "" {
		return state, errors.New("offline meaning indexing is not configured")
	}
	if update {
		var coverage SemanticCoverage
		if readIndexJSON(m.base.Semantic, &coverage) != nil || coverage.State != "complete" || coverage.VerifiedCount != coverage.Total || coverage.Indexed != coverage.Total || coverage.Total < 1 || coverage.CatalogSHA != m.base.SHA || coverage.ModelSHA != m.config.ModelSHA || coverage.Algorithm != semanticAlgorithm || coverage.Dimensions != semanticDimensions || coverage.Collection != semanticCollection(m.base.SHA, m.config.ModelSHA) {
			return state, errors.New("finish and activate the complete meaning catalog before merging arXiv updates")
		}
		expected, _ := json.Marshal(semanticEngine())
		actual, _ := json.Marshal(coverage.Engine)
		if string(expected) != string(actual) {
			return state, errors.New("active embedding contract changed")
		}
	}
	if kind == "rebuild" {
		sources := map[string]bool{}
		for _, c := range m.config.Collections {
			if seen[c.ID] {
				sources[c.Source] = true
			}
		}
		for _, c := range m.config.Collections {
			if c.Adapter != "inventory" && sources[c.Source] && !seen[c.ID] {
				return state, errors.New("select every configured collection for a source before replacing it")
			}
		}
	}
	states, e := m.states()
	if e != nil {
		return state, e
	}
	retained := 0
	for _, state := range states {
		if state.PrunedAt == "" {
			retained++
		}
	}
	if retained >= 100 {
		return state, errors.New("index history needs operator retention maintenance")
	}
	for _, s := range states {
		if s.Status != "complete" && s.Status != "ready" && s.Status != "cancelled" {
			return state, errors.New("finish or cancel the existing indexing run first")
		}
	}
	id, e := randomDeskID()
	if e != nil {
		return state, e
	}
	staged, e := os.MkdirTemp(filepath.Join(m.root, "runs"), ".queued-")
	if e != nil {
		return state, e
	}
	defer os.RemoveAll(staged)
	updateSHA := ""
	if update {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if e = freezeArxivUpdates(ctx, filepath.Join(staged, "updates.sqlite")); e != nil {
			return state, e
		}
		if updateSHA, e = arxivMetadataDigest(ctx, filepath.Join(staged, "updates.sqlite")); e != nil {
			return state, e
		}
		if automatic && updateSHA == m.base.UpdateSHA {
			return state, errArxivUnchanged
		}
	}
	run := indexRun{ID: id, Kind: kind, Collections: ids, Meaning: meaning, ConfigSHA: m.configSHA, Base: m.base, Created: indexTime(), Automatic: automatic, UpdateSHA: updateSHA}
	state = indexState{ID: id, Status: "queued", Phase: "inventory", Updated: indexTime()}
	if e = writeIndexJSON(filepath.Join(staged, "request.json"), run); e != nil {
		return state, e
	}
	if e = writeIndexJSON(filepath.Join(staged, "control.json"), map[string]string{"desired": "running"}); e != nil {
		return state, e
	}
	if e = writeIndexJSON(filepath.Join(staged, "state.json"), state); e != nil {
		return state, e
	}
	e = os.Rename(staged, m.runPath(id, ""))
	if e == nil {
		dir, openErr := os.Open(filepath.Join(m.root, "runs"))
		if openErr != nil {
			return state, openErr
		}
		e = dir.Sync()
		dir.Close()
	}
	return state, e
}
func indexTime() string { return time.Now().UTC().Format(time.RFC3339Nano) }
func (m *indexManager) control(id, action string) error {
	if !deskID.MatchString(id) {
		return errors.New("invalid indexing run")
	}
	var s indexState
	if e := readIndexJSON(m.runPath(id, "state.json"), &s); e != nil {
		return e
	}
	if s.Status == "ready" || s.Status == "complete" || s.Status == "cancelled" {
		return errors.New("this run has finished")
	}
	desired := map[string]string{"pause": "paused", "resume": "running", "cancel": "cancelled"}[action]
	if desired == "" {
		return errors.New("invalid indexing action")
	}
	return writeIndexJSON(m.runPath(id, "control.json"), map[string]string{"desired": desired})
}
func (m *indexManager) activate(id string, rollback bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	lock, e := indexLock(filepath.Join(m.root, "control.lock"))
	if e != nil {
		return e
	}
	defer indexUnlock(lock)
	if m.reloading.Load() {
		return errors.New("catalog reload is already in progress")
	}
	if m.restart == nil {
		return errors.New("this server cannot reload its catalog")
	}
	// A switch never interrupts a saved research job.
	if m.desk != nil {
		m.desk.db.mu.Lock()
		rows, e := m.desk.db.rows("SELECT count(*) FROM desk_jobs WHERE status IN ('queued','running')")
		m.desk.db.mu.Unlock()
		if e != nil || len(rows) != 1 || rows[0][0] != "0" {
			return errIndexJobsBusy
		}
	}
	var a indexActive
	e = readIndexJSON(filepath.Join(m.root, "active.json"), &a)
	if os.IsNotExist(e) {
		a.Current = m.base
	} else if e != nil {
		return e
	}
	var candidate indexGeneration
	if rollback {
		if a.Previous == nil {
			return errors.New("no previous generation is available")
		}
		candidate = *a.Previous
	} else {
		if !deskID.MatchString(id) {
			return errors.New("invalid indexing run")
		}
		var s indexState
		if e = readIndexJSON(m.runPath(id, "state.json"), &s); e != nil {
			return e
		}
		if s.Status != "ready" || s.Candidate == nil {
			return errors.New("only a validated candidate can be activated")
		}
		candidate = *s.Candidate
	}
	if candidate.ID == a.Current.ID {
		return errors.New("this generation is already active")
	}
	original := os.Getenv("RESEARCH_INDEX_ORIGINAL_CATALOG")
	if postgresEnabled() {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
		defer cancel()
		if e = postgresPrepareActivation(ctx, candidate); e != nil {
			return e
		}
	}
	if e = validGeneration(m.root, candidate, original); e != nil {
		return e
	}
	if a.Current.SHA == "" {
		a.Current.SHA, e = indexFileSHA(a.Current.Catalog)
		if e != nil {
			return e
		}
	}
	if e = validGeneration(m.root, a.Current, original); e != nil {
		return e
	}
	if !rollback && (candidate.Parent != a.Current.ID || candidate.ParentSHA != a.Current.SHA) {
		return errors.New("candidate used an older active generation; rebuild from the current catalog")
	}
	previous := a.Current
	if e = m.rememberSwitch(a); e != nil {
		return e
	}
	a.Previous = &previous
	a.Current = candidate
	if e = writeIndexJSON(filepath.Join(m.root, "active.json"), a); e != nil {
		return e
	}
	// Activated generations remain usable through the rollback receipt. Mark
	// their indexing runs complete so unattended updates do not mistake them
	// for candidates awaiting a review.
	for _, generation := range []indexGeneration{previous, candidate} {
		if generation.ID == "original" {
			continue
		}
		var state indexState
		if readIndexJSON(m.runPath(generation.ID, "state.json"), &state) == nil && state.Status == "ready" {
			state.Status = "complete"
			_ = m.saveState(&state)
		}
	}
	m.reloading.Store(true)
	m.restart()
	return nil
}
func (m *indexManager) routes(mux *http.ServeMux) {
	if m == nil {
		return
	}
	wrap := func(fn http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if m.reloading.Load() {
				http.Error(w, "catalog reload is in progress", 503)
				return
			}
			if !m.authorized(r) {
				http.Error(w, "index administration requires an approved administrator", 403)
				return
			}
			fn(w, r)
		}
	}
	mux.HandleFunc("GET /api/library/indexing", wrap(func(w http.ResponseWriter, r *http.Request) {
		states, e := m.states()
		if e != nil {
			fail(w, e)
			return
		}
		collections := []map[string]any{}
		for _, c := range m.config.Collections {
			collections = append(collections, map[string]any{"id": c.ID, "label": c.Label, "source": c.Source, "adapter": c.Adapter})
		}
		// Retired generations retain small audit receipts, not visible catalog copies.
		visible := states[:0]
		for _, state := range states {
			if state.PrunedAt == "" {
				visible = append(visible, state)
			}
		}
		states = visible
		// The public response intentionally excludes all storage keys and paths.
		for n := range states {
			states[n].ValidationProgress = m.validationProgress(states[n].ID)
			for j := range states[n].Inventory {
				states[n].Inventory[j].ObjectCount = len(states[n].Inventory[j].Objects)
				states[n].Inventory[j].Objects = nil
				states[n].Inventory[j].Cursor = ""
			}
			if states[n].Candidate != nil {
				g := *states[n].Candidate
				g.Catalog = ""
				g.Semantic = ""
				states[n].Candidate = &g
			}
		}
		var a indexActive
		if readIndexJSON(filepath.Join(m.root, "active.json"), &a) != nil {
			a.Current = m.base
		}
		writeJSON(w, 200, map[string]any{"collections": collections, "runs": states, "active": a.Current.ID, "rollback_available": a.Previous != nil, "meaning_available": m.config.ModelSHA != "", "worker_heartbeat": indexHeartbeat(m.root), "arxiv_sync": syncPublicStatus(), "arxiv_updates_enabled": os.Getenv("RESEARCH_ARXIV_UPDATES_ENABLED") == "true", "arxiv_auto_merge": m.autoMergePublicStatus(), "retention": m.retentionPublicStatus(), "metadata_backend": func() string {
			if postgresEnabled() {
				return "postgres"
			}
			return "sqlite"
		}()})
	}))
	mux.HandleFunc("POST /api/library/indexing", wrap(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Kind        string   `json:"kind"`
			Collections []string `json:"collections"`
			Meaning     bool     `json:"meaning"`
		}
		if e := decode(w, r, &req); e != nil {
			fail(w, e)
			return
		}
		s, e := m.submit(req.Kind, req.Collections, req.Meaning)
		if e != nil {
			fail(w, e)
			return
		}
		writeJSON(w, 202, s)
	}))
	mux.HandleFunc("POST /api/library/indexing/{id}/{action}", wrap(func(w http.ResponseWriter, r *http.Request) {
		id, action := r.PathValue("id"), r.PathValue("action")
		var e error
		if action == "activate" || action == "rollback" {
			e = m.activate(id, action == "rollback")
		} else {
			e = m.control(id, action)
		}
		if e != nil {
			fail(w, e)
			return
		}
		writeJSON(w, 202, map[string]string{"action": action, "status": "accepted"})
	}))
}
func indexHeartbeat(root string) string {
	var v map[string]string
	if readIndexJSON(filepath.Join(root, "worker.json"), &v) != nil {
		return ""
	}
	return v["updated"]
}
func indexFileSHA(path string) (string, error) {
	if postgresEnabled() {
		if id, ok := postgresPathID(path); ok {
			d, e := openPostgresCatalog(path, id)
			if e != nil {
				return "", e
			}
			defer d.close()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
			defer cancel()
			return d.pg.identity(ctx)
		}
	}
	return indexRawFileSHA(path)
}
func indexRawFileSHA(path string) (string, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", e
	}
	defer f.Close()
	return streamSHA(f)
}

// Defined here to keep file hashing streamed, including multi-gigabyte catalogs.
func streamSHA(r io.Reader) (string, error) { return indexStreamSHA(r) }
