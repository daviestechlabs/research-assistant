package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var errIndexPause = errors.New("indexing paused at a durable checkpoint")
var errIndexCancel = errors.New("indexing cancelled")

func (m *indexManager) checkControl(ctx context.Context, id string) error {
	if ctx.Err() != nil {
		return errIndexPause
	}
	var c map[string]string
	if e := readIndexJSON(m.runPath(id, "control.json"), &c); e != nil {
		return e
	}
	switch c["desired"] {
	case "running":
		return nil
	case "paused":
		return errIndexPause
	case "cancelled":
		return errIndexCancel
	}
	return errors.New("invalid indexing control receipt")
}
func (m *indexManager) saveState(s *indexState) error {
	s.Updated = indexTime()
	return writeIndexJSON(m.runPath(s.ID, "state.json"), s)
}
func indexingWorker() error {
	original := os.Getenv("RESEARCH_INDEX_ORIGINAL_CATALOG")
	if !filepath.IsAbs(original) {
		return errors.New("worker needs an explicit original catalog")
	}
	m, e := newIndexManager(original, nil)
	if e != nil {
		return e
	}
	if m == nil {
		return errors.New("indexing worker is not configured")
	}
	lock, e := indexLock(filepath.Join(m.root, "worker.lock"))
	if e != nil {
		return e
	}
	defer indexUnlock(lock)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// This worker receives no chat-model key and never invokes a chat model.
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			writeIndexJSON(filepath.Join(m.root, "worker.json"), map[string]string{"updated": indexTime()})
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	for ctx.Err() == nil {
		states, e := m.states()
		if e != nil {
			return e
		}
		for _, s := range states {
			if s.Status == "complete" || s.Status == "ready" || s.Status == "cancelled" {
				continue
			}
			controlErr := m.checkControl(ctx, s.ID)
			if errors.Is(controlErr, errIndexCancel) {
				s.Status = "cancelled"
				m.saveState(&s)
				continue
			}
			if controlErr != nil {
				if s.Status != "failed" {
					s.Status = "paused"
					m.saveState(&s)
				}
				continue
			}
			if s.Status == "failed" {
				c, _ := os.Stat(m.runPath(s.ID, "control.json"))
				v, _ := os.Stat(m.runPath(s.ID, "state.json"))
				if c == nil || v == nil || !c.ModTime().After(v.ModTime()) {
					continue
				}
			}
			var run indexRun
			if e = readIndexJSON(m.runPath(s.ID, "request.json"), &run); e == nil {
				e = m.execute(ctx, run, &s)
			}
			e = indexWorkerOutcome(ctx, e)
			if e != nil {
				s.Status = "failed"
				s.Error = "Indexing failed. Check the worker receipt and configured source snapshot."
				if errors.Is(e, errIndexPause) {
					s.Status = "paused"
					s.Error = ""
				}
				if errors.Is(e, errIndexCancel) {
					s.Status = "cancelled"
					s.Error = ""
				}
				m.saveState(&s)
				fmt.Fprintln(os.Stderr, "index run", s.ID, "phase", s.Phase, "failed:", indexSafeError(e))
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
	return nil
}

// A cancelled service request during worker shutdown is a resumable pause.
// Service adapters deliberately hide transport details, so use the parent context.
func indexWorkerOutcome(ctx context.Context, err error) error {
	if err != nil && ctx.Err() != nil && !errors.Is(err, errIndexCancel) {
		return errIndexPause
	}
	return err
}

func indexSafeError(e error) string {
	// Adapter and SQLite errors can include private object paths or source text.
	// Keep user-facing messages and worker logs free of those values.
	if errors.Is(e, errIndexPause) || errors.Is(e, errIndexCancel) {
		return e.Error()
	}
	return "operation failed; source or validation contract did not pass"
}
func (m *indexManager) selected(run indexRun) ([]indexCollection, error) {
	if run.ConfigSHA != m.configSHA {
		return nil, errors.New("index configuration changed; start a new run")
	}
	var out []indexCollection
	for _, id := range run.Collections {
		found := false
		for _, c := range m.config.Collections {
			if c.ID == id {
				out = append(out, c)
				found = true
			}
		}
		if !found {
			return nil, errors.New("collection disappeared")
		}
	}
	return out, nil
}
func (m *indexManager) execute(ctx context.Context, run indexRun, s *indexState) error {
	if run.Kind == "arxiv-update" {
		return m.executeArxivUpdate(ctx, run, s)
	}
	collections, e := m.selected(run)
	if e != nil {
		return e
	}
	s.Status = "running"
	s.Error = ""
	if e = m.saveState(s); e != nil {
		return e
	}
	if s.Phase == "inventory" {
		base, e := openDatabase(run.Base.Catalog, false)
		if e != nil {
			return e
		}
		defer base.close()
		for ci, c := range collections {
			if ci < len(s.Inventory) && s.Inventory[ci].ListingComplete {
				continue
			}
			if e = m.checkControl(ctx, run.ID); e != nil {
				return e
			}
			a, e := collectionStore(c)
			if e != nil {
				return e
			}
			if ci >= len(s.Inventory) {
				s.Inventory = append(s.Inventory, indexInventory{Collection: c.ID})
			}
			if c.Adapter == "inventory" {
				if e = m.inventoryHolding(ctx, a, c, s, ci); e != nil {
					return e
				}
				continue
			}
			objects, e := discoverIndexObjects(ctx, a, c, m.config.MaxObjects)
			if e != nil {
				return e
			}
			inv := indexInventory{Collection: c.ID, Objects: objects}
			rows, e := base.rows("SELECT documents,missing_summaries FROM catalog_stats WHERE source=?", c.Source)
			if e != nil {
				return e
			}
			if len(rows) == 1 {
				inv.Existing, _ = strconv.ParseInt(rows[0][0], 10, 64)
				inv.MissingSummaries, _ = strconv.ParseInt(rows[0][1], 10, 64)
			}
			for n := range inv.Objects {
				if e = m.checkControl(ctx, run.ID); e != nil {
					return e
				}
				count, e := indexObjectRows(ctx, a, c, inv.Objects[n])
				if e != nil {
					return e
				}
				inv.Objects[n].Rows = count
				inv.Records += count
				inv.ObservedObjects++
				inv.ObservedBytes += inv.Objects[n].Bytes
			}
			inv.ListingComplete = true
			s.Inventory[ci] = inv
			if e = m.saveState(s); e != nil {
				return e
			}
		}
	}
	if len(s.Inventory) != len(collections) {
		return errors.New("inventory is incomplete; start a new run")
	}
	if run.Kind == "inventory" {
		s.Status = "complete"
		s.Phase = "inventory"
		return m.saveState(s)
	}
	path := m.runPath(run.ID, "catalog.sqlite")
	if s.Phase == "inventory" || s.Phase == "keyword" {
		if e = m.prepareCandidate(ctx, run, s, path, collections); e != nil {
			return e
		}
		d, e := openDatabase(path, false)
		if e != nil {
			return e
		}
		// The database checkpoint is authoritative if the JSON status lagged a commit.
		rows, e := d.rows("SELECT state FROM indexing_checkpoint WHERE id=1")
		if e != nil {
			d.close()
			return e
		}
		if len(rows) == 1 {
			if e = json.Unmarshal([]byte(rows[0][0]), s); e != nil {
				d.close()
				return e
			}
			s.Status = "running"
		}
		for s.Collection < len(collections) {
			c := collections[s.Collection]
			a, e := collectionStore(c)
			if e != nil {
				d.close()
				return e
			}
			inv := s.Inventory[s.Collection]
			for s.Object < len(inv.Objects) {
				o := inv.Objects[s.Object]
				fresh, e := indexHead(ctx, a, o.Key)
				if e != nil || fresh.ETag != o.ETag || fresh.Bytes != o.Bytes {
					d.close()
					return errors.New("source snapshot changed; start a new run")
				}
				e = streamIndexObject(ctx, a, c, o, s.Row, func(batch []Document, cursor int64) error {
					if e := m.checkControl(ctx, run.ID); e != nil {
						return e
					}
					before := *s
					if e := d.exec("BEGIN IMMEDIATE"); e != nil {
						return e
					}
					committed := false
					defer func() {
						if !committed {
							d.exec("ROLLBACK")
							*s = before
						}
					}()
					for _, raw := range batch {
						doc, e := normalize(raw)
						if e != nil {
							return e
						}
						existing, e := d.rows("SELECT fingerprint FROM documents WHERE id=?", doc.ID)
						if e != nil {
							return e
						}
						if len(existing) > 0 {
							if existing[0][0] != doc.Fingerprint {
								return errors.New("duplicate ID has conflicting metadata")
							}
							s.Duplicates++
						} else {
							if _, e = d.put(doc); e != nil {
								return e
							}
						}
						s.Read++
						if doc.Summary == "" {
							s.Missing++
						}
					}
					s.Row = cursor
					s.Phase = "keyword"
					s.Updated = indexTime()
					encoded, _ := json.Marshal(s)
					if _, e := d.run("INSERT OR REPLACE INTO indexing_checkpoint VALUES(1,?)", string(encoded)); e != nil {
						return e
					}
					if e := d.exec("COMMIT"); e != nil {
						return e
					}
					committed = true
					return m.saveState(s)
				})
				if e != nil {
					d.close()
					return e
				}
				s.Object++
				s.Row = 0
				if e = m.checkpoint(d, s); e != nil {
					d.close()
					return e
				}
			}
			s.Collection++
			s.Object = 0
			if e = m.checkpoint(d, s); e != nil {
				d.close()
				return e
			}
		}
		s.Phase = "validate-keywords"
		if e = m.checkpoint(d, s); e != nil {
			d.close()
			return e
		}
		d.close()
	}
	if e = m.checkControl(ctx, run.ID); e != nil {
		return e
	}
	if s.Phase == "validate-keywords" {
		d, e := openDatabase(path, false)
		if e != nil {
			return e
		}
		if e = d.verify(); e != nil {
			d.close()
			return e
		}
		if e = d.exec("DROP TABLE IF EXISTS indexing_checkpoint"); e != nil {
			d.close()
			return e
		}
		rows, e := d.rows("SELECT count(*),sum(summary_kind='missing') FROM documents")
		if e != nil || len(rows) != 1 || rows[0][0] == "0" {
			d.close()
			return errors.New("empty candidate")
		}
		counts, e := d.rows("SELECT source,count(*),sum(summary_kind='missing') FROM documents GROUP BY source ORDER BY source")
		if e != nil {
			d.close()
			return e
		}
		stats, e := d.rows("SELECT source,documents,missing_summaries FROM catalog_stats WHERE documents>0 ORDER BY source")
		if e != nil {
			d.close()
			return e
		}
		a, _ := json.Marshal(counts)
		b, _ := json.Marshal(stats)
		if string(a) != string(b) {
			d.close()
			return errors.New("catalog count reconciliation failed")
		}
		var expected int64
		for _, v := range s.Inventory {
			expected += v.Records
		}
		if expected != s.Read {
			d.close()
			return errors.New("import did not consume the entire snapshot")
		}
		for _, c := range collections {
			r, e := d.rows("SELECT id FROM documents WHERE source=? ORDER BY rowid LIMIT 1", c.Source)
			if e != nil || len(r) != 1 {
				d.close()
				return errors.New("collection has no indexed records")
			}
			found, e := d.search(SearchRequest{Query: r[0][0], Source: c.Source, Limit: 5})
			if e != nil || len(found.Documents) == 0 || found.Documents[0].ID != r[0][0] {
				d.close()
				return errors.New("saved ID query failed")
			}
		}
		if _, e = d.run("ATTACH DATABASE ? AS baseline", run.Base.Catalog); e != nil {
			d.close()
			return e
		}
		changes, e := d.rows(`SELECT
   (SELECT count(*) FROM documents n LEFT JOIN baseline.documents b ON n.id=b.id WHERE b.id IS NULL),
   (SELECT count(*) FROM baseline.documents b LEFT JOIN documents n ON n.id=b.id WHERE n.id IS NULL),
   (SELECT count(*) FROM documents n JOIN baseline.documents b ON n.id=b.id WHERE n.fingerprint!=b.fingerprint)`)
		if e != nil || len(changes) != 1 {
			d.close()
			return errors.New("candidate change accounting failed")
		}
		if e = d.exec("DETACH DATABASE baseline"); e != nil {
			d.close()
			return e
		}
		doi, e := d.rows("SELECT count(*) FROM (SELECT doi FROM documents WHERE doi!='' GROUP BY doi HAVING count(*)>1)")
		if e != nil {
			d.close()
			return e
		}
		keywordRows, e := d.rows("SELECT summary FROM documents WHERE summary!='' LIMIT 1")
		if e != nil {
			d.close()
			return e
		}
		if len(keywordRows) > 0 {
			words := strings.Fields(keywordRows[0][0])
			probe := ""
			for _, word := range words {
				word = strings.Trim(word, ".,;:()[]{}\"'")
				if len(word) > 3 && len(word) < 32 && !strings.ContainsAny(word, "\\/\"*") {
					probe = word
					break
				}
			}
			if probe != "" {
				found, e := d.search(SearchRequest{Query: probe, Limit: 1})
				if e != nil || len(found.Documents) == 0 {
					d.close()
					return errors.New("abstract keyword query failed")
				}
			}
		}
		s.Validation = map[string]any{"sqlite_integrity": true, "fts_integrity": true, "counts_reconciled": true, "all_snapshot_rows_read": true, "source_counts": counts, "id_queries": true, "keyword_query": true, "duplicate_dois": doi[0][0], "saved_link_scope": "configured samples only; metadata does not prove every saved file exists", "chat_model_calls": 0, "added_records": changes[0][0], "removed_records": changes[0][1], "changed_records": changes[0][2]}
		d.close()
		sha, e := indexFileSHA(path)
		if e != nil {
			return e
		}
		s.Candidate = &indexGeneration{ID: run.ID, Catalog: path, SHA: sha, Parent: run.Base.ID, ParentSHA: s.BaselineSHA}
		// Saved-link checks read only the deployment's explicitly configured samples.
		if len(m.config.LinkSamples) > 0 {
			archive, e := newPDFArchive()
			if e != nil || archive == nil {
				return errors.New("saved-link validation is unavailable")
			}
			for _, id := range m.config.LinkSamples {
				if e = m.checkControl(ctx, run.ID); e != nil {
					return e
				}
				if _, e = archive.readSourcePDF(ctx, id); e != nil {
					return errors.New("a saved document sample failed validation")
				}
			}
			s.Validation["saved_links_verified"] = len(m.config.LinkSamples)
		}
		if run.Meaning {
			s.Phase = "meaning"
		} else {
			s.Phase = "validated"
		}
		if e = m.saveState(s); e != nil {
			return e
		}
	}
	if s.Phase == "meaning" {
		if e = m.indexMeaning(ctx, run, s); e != nil {
			return e
		}
		s.Phase = "validated"
		if e = m.saveState(s); e != nil {
			return e
		}
	}
	if s.Phase != "validated" {
		return errors.New("invalid candidate phase")
	}
	sha, e := indexFileSHA(path)
	if e != nil || s.Candidate == nil || sha != s.Candidate.SHA {
		return errors.New("validated candidate changed")
	}
	s.Status = "ready"
	return m.saveState(s)
}
func (m *indexManager) checkpoint(d *database, s *indexState) error {
	encoded, _ := json.Marshal(s)
	if _, e := d.run("INSERT OR REPLACE INTO indexing_checkpoint VALUES(1,?)", string(encoded)); e != nil {
		return e
	}
	return m.saveState(s)
}
func (m *indexManager) prepareCandidate(ctx context.Context, run indexRun, s *indexState, path string, collections []indexCollection) error {
	if _, e := os.Stat(path); e == nil {
		return nil
	} else if !os.IsNotExist(e) {
		return e
	}
	s.Phase = "keyword"
	if e := m.saveState(s); e != nil {
		return e
	}
	// Preserve unselected sources. Replace selected sources in the candidate,
	// so source deletions never leave stale searchable records after a full rebuild.
	source, e := openDatabase(run.Base.Catalog, false)
	if e != nil {
		return e
	}
	defer source.close()
	before, e := os.Stat(run.Base.Catalog)
	if e != nil {
		return e
	}
	if run.Base.SHA != "" {
		sha, e := indexFileSHA(run.Base.Catalog)
		if e != nil || sha != run.Base.SHA {
			return errors.New("active catalog identity changed")
		}
	}
	if before.Size() > int64(m.config.MaxCatalogMiB)*1024*1024 {
		return errors.New("baseline catalog exceeds the candidate ceiling")
	}
	var space syscall.Statfs_t
	if e = syscall.Statfs(filepath.Dir(path), &space); e != nil {
		return e
	}
	if uint64(space.Bavail)*uint64(space.Bsize) < uint64(before.Size())*2+uint64(256<<20) {
		return errors.New("insufficient free NAS space for a candidate and journal")
	}
	s.BaselineSHA = run.Base.SHA
	if s.BaselineSHA == "" {
		s.BaselineSHA, e = indexFileSHA(run.Base.Catalog)
		if e != nil {
			return e
		}
	}
	if e = m.saveState(s); e != nil {
		return e
	}
	reserved, e := os.CreateTemp(filepath.Dir(path), "catalog-*.partial")
	if e != nil {
		return e
	}
	partial := reserved.Name()
	reserved.Close()
	if e = source.snapshot(ctx, partial); e != nil {
		return e
	}
	after, e := os.Stat(run.Base.Catalog)
	if e != nil || !sameFileSnapshot(before, after) {
		return errors.New("active catalog changed during its snapshot")
	}
	d, e := openDatabase(partial, false)
	if e != nil {
		return e
	}
	defer d.close()
	sizeRows, e := d.rows("PRAGMA page_size")
	if e != nil || len(sizeRows) != 1 {
		return errors.New("candidate page size is unavailable")
	}
	pageSize, e := strconv.ParseInt(sizeRows[0][0], 10, 64)
	if e != nil || pageSize < 512 {
		return errors.New("candidate page size is invalid")
	}
	if e = d.exec(fmt.Sprintf("PRAGMA max_page_count=%d", int64(m.config.MaxCatalogMiB)*1024*1024/pageSize)); e != nil {
		return e
	}
	if e = d.exec("BEGIN IMMEDIATE"); e != nil {
		return e
	}
	defer d.exec("ROLLBACK")
	for _, c := range collections {
		if _, e = d.run("DELETE FROM documents WHERE source=?", c.Source); e != nil {
			return e
		}
	}
	if e = d.exec("DROP TABLE IF EXISTS indexing_checkpoint; CREATE TABLE indexing_checkpoint(id INTEGER PRIMARY KEY CHECK(id=1),state TEXT NOT NULL)"); e != nil {
		return e
	}
	encoded, _ := json.Marshal(s)
	if _, e = d.run("INSERT INTO indexing_checkpoint VALUES(1,?)", string(encoded)); e != nil {
		return e
	}
	if e = d.exec("COMMIT"); e != nil {
		return e
	}
	return os.Rename(partial, path)
}

// Reuse the application's exact full-text embedding contract. The worker has
// its own bounded gate and no Qwen key, daily allowance, or chat reservation.
func (m *indexManager) indexMeaning(ctx context.Context, run indexRun, s *indexState) error {
	if reconciled, _ := s.Validation["counts_reconciled"].(bool); !reconciled {
		return errors.New("meaning indexing requires reconciled catalog counts")
	}
	if s.Candidate == nil || !shaPattern.MatchString(m.config.ModelSHA) {
		return errors.New("meaning candidate identity is missing")
	}
	sc := &semanticClient{embeddingOrigin: m.config.EmbeddingOrigin, milvusOrigin: m.config.VectorOrigin, modelSHA: m.config.ModelSHA, catalogSHA: s.Candidate.SHA, collection: semanticCollection(s.Candidate.SHA, m.config.ModelSHA), client: &http.Client{Timeout: 60 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("model redirects disabled") }}}
	if run.Kind == "arxiv-update" {
		sc.responseLimit = 4 << 20
	}
	if e := sc.health(ctx); e != nil {
		return e
	}
	if e := indexVectorSetup(ctx, sc); e != nil {
		return e
	}
	d, e := openDatabase(s.Candidate.Catalog, false)
	if e != nil {
		return e
	}
	defer d.close()
	// Keyword validation independently reconciles these counters with source rows.
	// A resume must not scan every abstract again before reading its saved cursor.
	rows, e := d.rows("SELECT documents,missing_summaries FROM catalog_stats WHERE source='arxiv'")
	if e != nil || len(rows) != 1 || rows[0][0] == "0" || rows[0][1] != "0" {
		return errors.New("meaning indexing requires complete supplied arXiv abstracts")
	}
	total, _ := strconv.Atoi(rows[0][0])
	status := SemanticCoverage{Algorithm: semanticAlgorithm, CatalogSHA: sc.catalogSHA, ModelSHA: sc.modelSHA, Collection: sc.collection, Dimensions: semanticDimensions, Engine: semanticEngine(), Total: total, State: "indexing"}
	statusPath := m.runPath(s.ID, "semantic.json")
	for {
		if e = m.checkControl(ctx, s.ID); e != nil {
			return e
		}
		batchSize := 8
		if run.Kind == "arxiv-update" {
			batchSize = 128
		}
		batch, e := d.rows("SELECT rowid,id,summary,fingerprint,categories,published FROM documents WHERE source='arxiv' AND rowid>? ORDER BY rowid LIMIT ?", strconv.FormatInt(s.LastVectorRow, 10), strconv.Itoa(batchSize))
		if e != nil {
			return e
		}
		if len(batch) == 0 {
			break
		}
		begin := time.Now()
		records := []map[string]any{}
		if e = sc.health(ctx); e != nil {
			return e
		}
		reusable, e := indexReusableVectors(ctx, sc, run.Base, batch)
		if e != nil {
			return e
		}
		reused, newEmbeddings := int64(0), int64(0)
		for _, r := range batch {
			vector, found := reusable[r[1]]
			if found {
				reused++
			} else {
				vector, e = indexEmbedAbstract(ctx, sc, r[2])
				if e != nil {
					return e
				}
				newEmbeddings++
			}
			if len(r[1]) > 64 || len(r[4]) > 1022 {
				return errors.New("vector metadata exceeds its contract")
			}
			records = append(records, map[string]any{"id": r[1], "fingerprint": r[3], "categories": " " + r[4] + " ", "published": r[5], "embedding": vector})
		}
		var out struct {
			Code int `json:"code"`
			Data struct {
				Count int      `json:"upsertCount"`
				IDs   []string `json:"upsertIds"`
			} `json:"data"`
		}
		if e = sc.call(ctx, sc.milvusOrigin, "/v2/vectordb/entities/upsert", map[string]any{"collectionName": sc.collection, "data": records}, &out); e != nil {
			return e
		}
		if out.Code != 0 || out.Data.Count != len(batch) || len(out.Data.IDs) != len(batch) {
			return errors.New("vector write accounting failed")
		}
		seen := map[string]bool{}
		for _, id := range out.Data.IDs {
			seen[id] = true
		}
		for _, r := range batch {
			if !seen[r[1]] {
				return errors.New("vector IDs changed")
			}
		}
		// Idempotent upsert precedes durable checkpoint. A crash safely replays this batch.
		s.LastVectorRow, _ = strconv.ParseInt(batch[len(batch)-1][0], 10, 64)
		s.Vectors += int64(len(batch))
		s.ReusedVectors += reused
		s.NewEmbeddings += newEmbeddings
		if e = m.saveState(s); e != nil {
			return e
		}
		status.Indexed = int(s.Vectors)
		status.Updated = indexTime()
		if e = writeIndexJSON(statusPath, status); e != nil {
			return e
		}
		paced := int64(len(batch))
		if run.Kind == "arxiv-update" {
			paced = newEmbeddings
		}
		delay := time.Duration(paced)*time.Second/time.Duration(m.config.DocumentsPerSecond) - time.Since(begin)
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return errIndexPause
			case <-timer.C:
			}
		}
	}
	if int(s.Vectors) != total {
		return errors.New("vector checkpoint count does not match catalog")
	}
	if e = indexMilvus(ctx, sc, "/v2/vectordb/collections/flush", map[string]any{"collectionNames": []string{sc.collection}}); e != nil {
		return e
	}
	var count struct {
		Code int              `json:"code"`
		Data []map[string]any `json:"data"`
	}
	if e = sc.call(ctx, sc.milvusOrigin, "/v2/vectordb/entities/query", map[string]any{"collectionName": sc.collection, "filter": "", "outputFields": []string{"count(*)"}, "consistencyLevel": "Strong"}, &count); e != nil {
		return e
	}
	if count.Code != 0 || len(count.Data) != 1 || count.Data[0]["count(*)"] != float64(total) {
		return errors.New("independent vector count failed")
	}
	status.Indexed = total
	status.VerifiedCount = total
	status.State = "complete"
	status.Updated = indexTime()
	if e = writeIndexJSON(statusPath, status); e != nil {
		return e
	}
	s.Candidate.Semantic = statusPath
	s.Candidate.ModelSHA = m.config.ModelSHA
	s.Validation["verified_vectors"] = total
	return nil
}
func indexMilvus(ctx context.Context, s *semanticClient, path string, body any) error {
	var out struct {
		Code int `json:"code"`
	}
	if e := s.call(ctx, s.milvusOrigin, path, body, &out); e != nil {
		return e
	}
	if out.Code != 0 {
		return errors.New("vector service rejected the indexing operation")
	}
	return nil
}
func indexVectorSetup(ctx context.Context, s *semanticClient) error {
	var has struct {
		Code int `json:"code"`
		Data struct {
			Has bool `json:"has"`
		} `json:"data"`
	}
	if e := s.call(ctx, s.milvusOrigin, "/v2/vectordb/collections/has", map[string]any{"collectionName": s.collection}, &has); e != nil {
		return e
	}
	if has.Code != 0 {
		return errors.New("vector inventory failed")
	}
	if !has.Data.Has {
		fields := []map[string]any{{"fieldName": "id", "dataType": "VarChar", "isPrimary": true, "elementTypeParams": map[string]string{"max_length": "64"}}, {"fieldName": "fingerprint", "dataType": "VarChar", "elementTypeParams": map[string]string{"max_length": "64"}}, {"fieldName": "categories", "dataType": "VarChar", "elementTypeParams": map[string]string{"max_length": "1024"}}, {"fieldName": "published", "dataType": "VarChar", "elementTypeParams": map[string]string{"max_length": "10"}}, {"fieldName": "embedding", "dataType": "FloatVector", "elementTypeParams": map[string]string{"dim": "1024"}}}
		if e := indexMilvus(ctx, s, "/v2/vectordb/collections/create", map[string]any{"collectionName": s.collection, "schema": map[string]any{"autoID": false, "enableDynamicField": false, "fields": fields}}); e != nil {
			return e
		}
		if e := indexMilvus(ctx, s, "/v2/vectordb/collections/alter_properties", map[string]any{"collectionName": s.collection, "properties": map[string]string{"mmap.enabled": "true"}}); e != nil {
			return e
		}
		if e := indexMilvus(ctx, s, "/v2/vectordb/indexes/create", map[string]any{"collectionName": s.collection, "indexParams": []any{map[string]any{"fieldName": "embedding", "indexName": "embedding", "metricType": "COSINE", "params": map[string]string{"index_type": "IVF_SQ8", "nlist": "1024", "mmap.enabled": "true"}}}}); e != nil {
			return e
		}
	}
	return indexMilvus(ctx, s, "/v2/vectordb/collections/load", map[string]any{"collectionName": s.collection})
}

// Split large abstracts at valid UTF-8 boundaries, as the offline indexer does.
// Query windows cover every byte and the model confirms no truncation.
func indexEmbedAbstract(ctx context.Context, s *semanticClient, text string) ([]float64, error) {
	parts := []string{}
	for len(text) > 0 {
		n := len(text)
		if n > 32000 {
			n = 32000
			for n > 0 && (text[n]&0xc0) == 0x80 {
				n--
			}
			if space := strings.LastIndex(text[:n], " "); space > n/2 {
				n = space + 1
			}
		}
		parts = append(parts, text[:n])
		text = text[n:]
	}
	return indexEmbedParts(ctx, s, parts)
}
