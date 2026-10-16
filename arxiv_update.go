package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
)

func syncPublicStatus() *arxivSyncStatus {
	root := syncRoot()
	if root == "" {
		return nil
	}
	var s arxivSyncStatus
	if readIndexJSON(filepath.Join(root, "status.json"), &s) != nil {
		return &arxivSyncStatus{State: "waiting-for-first-harvest"}
	}
	return &s
}
func freezeArxivUpdates(ctx context.Context, path string) error {
	d, e := openDatabase(filepath.Join(syncRoot(), "updates.sqlite"), false)
	if e != nil {
		return errors.New("arXiv updates are not available yet")
	}
	defer d.close()
	rows, e := d.rows("SELECT count(*) FROM arxiv_updates")
	if e != nil || len(rows) != 1 || rows[0][0] == "0" {
		return errors.New("no harvested arXiv updates are available")
	}
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	f.Close()
	return d.snapshot(ctx, path)
}

func (m *indexManager) executeArxivUpdate(ctx context.Context, run indexRun, s *indexState) error {
	if run.ConfigSHA != m.configSHA {
		return errors.New("index configuration changed")
	}
	s.Status = "running"
	s.Error = ""
	if e := m.saveState(s); e != nil {
		return e
	}
	path := m.runPath(run.ID, "catalog.sqlite")
	if s.Phase == "inventory" || s.Phase == "keyword" {
		if e := m.prepareCandidate(ctx, run, s, path, nil); e != nil {
			return e
		}
		d, e := openDatabase(path, false)
		if e != nil {
			return e
		}
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
		delta, e := openDatabase(m.runPath(run.ID, "updates.sqlite"), false)
		if e != nil {
			d.close()
			return e
		}
		for {
			if e = m.checkControl(ctx, run.ID); e != nil {
				break
			}
			batch, err := delta.rows(`SELECT u.rowid,u.id,u.deleted,d.title,d.authors,d.summary,d.categories,d.published,d.doi,d.nas_ref
FROM arxiv_updates u LEFT JOIN documents d ON u.id=d.id WHERE u.rowid>? ORDER BY u.rowid LIMIT 100`, strconv.FormatInt(s.Row, 10))
			if err != nil {
				e = err
				break
			}
			if len(batch) == 0 {
				break
			}
			before := *s
			if e = d.exec("BEGIN IMMEDIATE"); e != nil {
				break
			}
			for _, r := range batch {
				if r[2] == "1" {
					_, e = d.run("DELETE FROM documents WHERE id=? AND source='arxiv'", r[1])
				} else {
					var doc Document
					doc, e = normalize(Document{ID: r[1], Source: "arxiv", Title: r[3], Authors: r[4], Summary: r[5], Categories: r[6], Published: r[7], DOI: r[8], NASRef: r[9]})
					if e == nil {
						_, e = d.put(doc)
					}
				}
				if e != nil {
					break
				}
				s.Read++
				s.Row, _ = strconv.ParseInt(r[0], 10, 64)
			}
			if e == nil {
				s.Phase = "keyword"
				encoded, _ := json.Marshal(s)
				_, e = d.run("INSERT OR REPLACE INTO indexing_checkpoint VALUES(1,?)", string(encoded))
			}
			if e == nil {
				e = d.exec("COMMIT")
			}
			if e != nil {
				d.exec("ROLLBACK")
				*s = before
				break
			}
			if e = m.saveState(s); e != nil {
				break
			}
		}
		if e == nil {
			count, err := delta.rows("SELECT count(*) FROM arxiv_updates")
			if err != nil || len(count) != 1 || count[0][0] != strconv.FormatInt(s.Read, 10) {
				e = errors.New("incremental snapshot row count did not reconcile")
			}
		}
		delta.close()
		if e == nil {
			s.Phase = "validate-keywords"
			e = m.checkpoint(d, s)
		}
		d.close()
		if e != nil {
			return e
		}
	}
	if s.Phase == "validate-keywords" {
		if e := m.checkControl(ctx, run.ID); e != nil {
			return e
		}
		d, e := openDatabase(path, false)
		if e != nil {
			return e
		}
		defer d.close()
		progress, e := m.observeValidation(run.ID, d)
		if e != nil {
			return e
		}
		defer progress.finish(false)
		if e = d.verifyObserved(progress.stage); e != nil {
			return e
		}
		progress.stage("source-counts")
		counts, e := d.rows("SELECT source,count(*),sum(summary_kind='missing') FROM documents GROUP BY source ORDER BY source")
		if e != nil {
			return e
		}
		stats, e := d.rows("SELECT source,documents,missing_summaries FROM catalog_stats WHERE documents>0 ORDER BY source")
		if e != nil {
			return e
		}
		a, _ := json.Marshal(counts)
		b, _ := json.Marshal(stats)
		if string(a) != string(b) {
			return errors.New("updated catalog count reconciliation failed")
		}
		if _, e = d.run("ATTACH DATABASE ? AS baseline", run.Base.Catalog); e != nil {
			return e
		}
		progress.stage("catalog-diff")
		changes, e := d.rows(`SELECT (SELECT count(*) FROM documents n LEFT JOIN baseline.documents b ON n.id=b.id WHERE b.id IS NULL),
(SELECT count(*) FROM baseline.documents b LEFT JOIN documents n ON n.id=b.id WHERE n.id IS NULL),
(SELECT count(*) FROM documents n JOIN baseline.documents b ON n.id=b.id WHERE n.fingerprint!=b.fingerprint)`)
		if e != nil {
			return e
		}
		if e = d.exec("DETACH DATABASE baseline; DROP TABLE IF EXISTS indexing_checkpoint"); e != nil {
			return e
		}
		progress.stage("catalog-checksum")
		sha, e := indexFileSHA(path)
		if e != nil {
			return e
		}
		s.Candidate = &indexGeneration{ID: run.ID, Catalog: path, SHA: sha, Parent: run.Base.ID, ParentSHA: s.BaselineSHA, UpdateSHA: run.UpdateSHA}
		s.Validation = map[string]any{"sqlite_integrity": true, "fts_integrity": true, "counts_reconciled": true, "all_snapshot_rows_read": true, "source_counts": counts, "added_records": changes[0][0], "removed_records": changes[0][1], "changed_records": changes[0][2], "chat_model_calls": 0, "saved_link_scope": "Downloaded updates retain per-version SHA-256 receipts; configured samples are checked separately."}
		progress.stage("saved-pdf-samples")
		if len(m.config.LinkSamples) > 0 {
			archive, e := newPDFArchive()
			if e != nil || archive == nil {
				return errors.New("saved sample reader is unavailable")
			}
			for _, id := range m.config.LinkSamples {
				if _, e = archive.readSourcePDF(ctx, id); e != nil {
					return errors.New("saved document sample failed")
				}
			}
			s.Validation["saved_links_verified"] = len(m.config.LinkSamples)
		}
		progress.finish(true)
		s.Phase = "meaning"
		if e = m.saveState(s); e != nil {
			return e
		}
	}
	if s.Phase == "meaning" {
		if e := m.indexMeaning(ctx, run, s); e != nil {
			return e
		}
		s.Phase = "validated"
		if e := m.saveState(s); e != nil {
			return e
		}
	}
	if s.Phase != "validated" || s.Candidate == nil {
		return errors.New("invalid incremental candidate phase")
	}
	sha, e := indexFileSHA(path)
	if e != nil || sha != s.Candidate.SHA {
		return errors.New("incremental candidate changed")
	}
	s.Status = "ready"
	return m.saveState(s)
}
