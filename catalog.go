package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const schema = `
CREATE TABLE IF NOT EXISTS documents (
 rowid INTEGER PRIMARY KEY, id TEXT NOT NULL UNIQUE, source TEXT NOT NULL,
 title TEXT NOT NULL, authors TEXT NOT NULL, summary TEXT NOT NULL,
 summary_kind TEXT NOT NULL, categories TEXT NOT NULL, language TEXT NOT NULL,
 published TEXT NOT NULL, doi TEXT NOT NULL, nas_ref TEXT NOT NULL,
 fingerprint TEXT NOT NULL, imported_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS documents_source ON documents(source);
CREATE INDEX IF NOT EXISTS documents_published ON documents(published);
CREATE INDEX IF NOT EXISTS documents_doi ON documents(doi);
CREATE TABLE IF NOT EXISTS catalog_stats (
 source TEXT PRIMARY KEY, documents INTEGER NOT NULL, missing_summaries INTEGER NOT NULL
);
CREATE VIRTUAL TABLE IF NOT EXISTS abstracts USING fts5(
 title, authors, summary, categories, doi, content='documents', content_rowid='rowid',
 tokenize='porter unicode61 remove_diacritics 2'
);
DROP TRIGGER IF EXISTS documents_insert;
DROP TRIGGER IF EXISTS documents_delete;
DROP TRIGGER IF EXISTS documents_update;
CREATE TRIGGER documents_insert AFTER INSERT ON documents BEGIN
 INSERT INTO abstracts(rowid,title,authors,summary,categories,doi)
 VALUES(new.rowid,new.title,new.authors,new.summary,new.categories,new.doi);
 INSERT INTO catalog_stats(source,documents,missing_summaries) VALUES(new.source,1,new.summary_kind='missing')
 ON CONFLICT(source) DO UPDATE SET documents=documents+1,missing_summaries=missing_summaries+(new.summary_kind='missing');
END;
CREATE TRIGGER documents_delete AFTER DELETE ON documents BEGIN
 INSERT INTO abstracts(abstracts,rowid,title,authors,summary,categories,doi)
 VALUES('delete',old.rowid,old.title,old.authors,old.summary,old.categories,old.doi);
 UPDATE catalog_stats SET documents=documents-1,missing_summaries=missing_summaries-(old.summary_kind='missing') WHERE source=old.source;
END;
CREATE TRIGGER documents_update AFTER UPDATE ON documents BEGIN
 INSERT INTO abstracts(abstracts,rowid,title,authors,summary,categories,doi)
 VALUES('delete',old.rowid,old.title,old.authors,old.summary,old.categories,old.doi);
 INSERT INTO abstracts(rowid,title,authors,summary,categories,doi)
 VALUES(new.rowid,new.title,new.authors,new.summary,new.categories,new.doi);
 UPDATE catalog_stats SET documents=documents-1,missing_summaries=missing_summaries-(old.summary_kind='missing') WHERE source=old.source;
 INSERT INTO catalog_stats(source,documents,missing_summaries) VALUES(new.source,1,new.summary_kind='missing')
 ON CONFLICT(source) DO UPDATE SET documents=documents+1,missing_summaries=missing_summaries+(new.summary_kind='missing');
END;
CREATE TABLE IF NOT EXISTS usage (
 day TEXT NOT NULL, kind TEXT NOT NULL, calls INTEGER NOT NULL, units INTEGER NOT NULL,
 PRIMARY KEY(day,kind)
);
PRAGMA user_version=2;
`

type Document struct {
	ID          string  `json:"id"`
	Source      string  `json:"source"`
	Title       string  `json:"title"`
	Authors     string  `json:"authors"`
	Summary     string  `json:"summary"`
	SummaryKind string  `json:"summary_kind"`
	Categories  string  `json:"categories"`
	Language    string  `json:"language"`
	Published   string  `json:"published"`
	DOI         string  `json:"doi"`
	NASRef      string  `json:"nas_ref,omitempty"`
	Edition     string  `json:"edition,omitempty"`
	Ruleset     string  `json:"ruleset,omitempty"`
	MediaType   string  `json:"media_type,omitempty"`
	BookURL     string  `json:"book_url,omitempty"`
	PDFURL      string  `json:"pdf_url,omitempty"`
	Fingerprint string  `json:"fingerprint"`
	Score       float64 `json:"score"`
}

var arxivID = regexp.MustCompile(`^(?:\d{4}\.\d{4,5}|[a-zA-Z][a-zA-Z0-9.-]*/\d{7})(?:v\d+)?$`)
var versionSuffix = regexp.MustCompile(`v\d+$`)

func canonicalID(source, id string) (string, error) {
	id = strings.TrimSpace(id)
	if source == "dnd" && bookID.MatchString(id) {
		return id, nil
	}
	if source == "arxiv" {
		id = strings.TrimPrefix(id, "arxiv:")
		id = strings.TrimPrefix(id, "https://arxiv.org/abs/")
		if !arxivID.MatchString(id) {
			return "", fmt.Errorf("invalid arxiv id %q", id)
		}
		return "arxiv:" + versionSuffix.ReplaceAllString(id, ""), nil
	}
	if source == "gutenberg" {
		id = strings.TrimPrefix(id, "gutenberg:")
		n, err := strconv.Atoi(id)
		if err != nil || n <= 0 {
			return "", fmt.Errorf("invalid gutenberg id %q", id)
		}
		return "gutenberg:" + strconv.Itoa(n), nil
	}
	return "", errors.New("source must be arxiv or gutenberg")
}

func normalize(doc Document) (Document, error) {
	id, err := canonicalID(doc.Source, doc.ID)
	if err != nil {
		return doc, err
	}
	doc.ID = id
	doc.Title = strings.Join(strings.Fields(doc.Title), " ")
	doc.Summary = strings.TrimSpace(doc.Summary)
	if doc.Published != "" {
		if _, err := time.Parse("2006-01-02", doc.Published); err != nil {
			return doc, errors.New("published date must use YYYY-MM-DD")
		}
	}
	if doc.Title == "" {
		return doc, errors.New("title is required")
	}
	if len(doc.Title) > 4096 || len(doc.Authors) > 65536 || len(doc.Summary) > 65536 || len(doc.Categories) > 8192 || len(doc.NASRef) > 4096 {
		return doc, errors.New("metadata field exceeds size limit")
	}
	if doc.Summary == "" {
		doc.SummaryKind = "missing"
	} else if doc.Source == "arxiv" {
		doc.SummaryKind = "abstract"
	} else {
		doc.SummaryKind = "catalog-description"
	}
	doc.Score = 0
	doc.Fingerprint = ""
	b, _ := json.Marshal(doc)
	h := sha256.Sum256(b)
	doc.Fingerprint = hex.EncodeToString(h[:])
	return doc, nil
}

func (d *database) initialize() error {
	rows, err := d.rows("PRAGMA user_version")
	if err != nil {
		return err
	}
	version := rows[0][0]
	if version != "0" && version != "1" && version != "2" {
		return errors.New("unsupported database schema")
	}
	if err = d.exec("BEGIN IMMEDIATE"); err != nil {
		return err
	}
	defer d.exec("ROLLBACK")
	if err = d.exec(schema); err != nil {
		return err
	}
	if version == "1" {
		if _, err = d.run("INSERT INTO catalog_stats SELECT source,count(*),sum(summary_kind='missing') FROM documents GROUP BY source"); err != nil {
			return err
		}
	}
	return d.exec("COMMIT")
}

func (d *database) verify() error {
	return d.verifyObserved(nil)
}
func (d *database) verifyObserved(stage func(string)) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if stage != nil {
		stage("sqlite-integrity")
	}
	r, err := d.rows("PRAGMA integrity_check")
	if err != nil {
		return err
	}
	if len(r) != 1 || r[0][0] != "ok" {
		return errors.New("database integrity check failed")
	}
	if stage != nil {
		stage("full-text-integrity")
	}
	_, err = d.run("INSERT INTO abstracts(abstracts,rank) VALUES('integrity-check',1)")
	return err
}

func (d *database) put(doc Document) (int, error) {
	return d.run(`INSERT INTO documents(id,source,title,authors,summary,summary_kind,categories,language,published,doi,nas_ref,fingerprint,imported_at)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET
 source=excluded.source,title=excluded.title,authors=excluded.authors,summary=excluded.summary,
 summary_kind=excluded.summary_kind,categories=excluded.categories,language=excluded.language,
 published=excluded.published,doi=excluded.doi,nas_ref=excluded.nas_ref,
 fingerprint=excluded.fingerprint,imported_at=excluded.imported_at
 WHERE documents.fingerprint != excluded.fingerprint`,
		doc.ID, doc.Source, doc.Title, doc.Authors, doc.Summary, doc.SummaryKind, doc.Categories, doc.Language,
		doc.Published, doc.DOI, doc.NASRef, doc.Fingerprint, time.Now().UTC().Format(time.RFC3339))
}

type ImportReport struct {
	Read           int   `json:"read"`
	Changed        int   `json:"changed"`
	Unchanged      int   `json:"unchanged"`
	Skipped        int   `json:"skipped"`
	MissingSummary int   `json:"missing_summary"`
	Committed      int   `json:"committed"`
	ElapsedMS      int64 `json:"elapsed_ms"`
}

// The importer consumes only a metadata stream. It has no PDF reader, directory
// crawler, download client, or path resolver. Replays are content-addressed.
func importCatalog(d *database, source, format string, in io.Reader, max int) (report ImportReport, err error) {
	return importCatalogBatched(context.Background(), d, source, format, in, max, 500, nil)
}

// Batch size controls durable checkpoint frequency. Progress describes committed
// input only; cancellation rolls back the current batch before returning.
func importCatalogBatched(ctx context.Context, d *database, source, format string, in io.Reader, max, batchSize int, progress io.Writer) (report ImportReport, err error) {
	if batchSize < 1 || batchSize > 50000 {
		return report, errors.New("batch size must be 1 to 50000")
	}
	if source != "arxiv" && source != "gutenberg" {
		return report, errors.New("invalid source")
	}
	if format != "jsonl" && !(format == "csv" && source == "gutenberg") {
		return report, errors.New("use jsonl, or csv for gutenberg")
	}
	start := time.Now()
	defer func() { report.ElapsedMS = time.Since(start).Milliseconds() }()
	d.mu.Lock()
	defer d.mu.Unlock()
	if err = d.exec("BEGIN IMMEDIATE"); err != nil {
		return
	}
	defer func() {
		if err != nil {
			_ = d.exec("ROLLBACK")
		}
	}()
	var scanner *bufio.Scanner
	var csvReader *csv.Reader
	var header []string
	if format == "csv" {
		csvReader = csv.NewReader(in)
		csvReader.ReuseRecord = true
		header, err = csvReader.Read()
		if err != nil {
			return
		}
		header = append([]string(nil), header...)
		foundID, foundTitle := false, false
		for _, h := range header {
			foundID = foundID || strings.TrimPrefix(h, "\ufeff") == "Text#"
			foundTitle = foundTitle || h == "Title"
		}
		if !foundID || !foundTitle {
			err = errors.New("CSV requires Text# and Title columns")
			return
		}
	} else {
		scanner = bufio.NewScanner(in)
		scanner.Buffer(make([]byte, 65536), 1024*1024)
	}
	for max <= 0 || report.Read < max {
		if err = ctx.Err(); err != nil {
			return
		}
		var doc Document
		if format == "csv" {
			var row []string
			row, err = csvReader.Read()
			if errors.Is(err, io.EOF) {
				err = nil
				break
			}
			if err != nil {
				return
			}
			values := map[string]string{}
			for i, h := range header {
				values[strings.TrimPrefix(h, "\ufeff")] = row[i]
			}
			report.Read++
			if values["Type"] != "" && values["Type"] != "Text" {
				report.Skipped++
				continue
			}
			doc = Document{ID: values["Text#"], Source: source, Title: values["Title"], Authors: values["Authors"],
				Summary: values["Description"], Categories: values["Subjects"] + "; " + values["Bookshelves"],
				Language: values["Language"], Published: values["Issued"], NASRef: values["NASRef"]}
		} else {
			if !scanner.Scan() {
				err = scanner.Err()
				if err != nil {
					return
				}
				break
			}
			report.Read++
			var raw struct {
				Document
				Abstract    string `json:"abstract"`
				Description string `json:"description"`
				UpdateDate  string `json:"update_date"`
				Versions    []struct {
					Created string `json:"created"`
				} `json:"versions"`
			}
			if err = json.Unmarshal(scanner.Bytes(), &raw); err != nil {
				err = fmt.Errorf("row %d: %w", report.Read, err)
				return
			}
			doc = raw.Document
			doc.Source = source
			if doc.Summary == "" {
				if source == "arxiv" {
					doc.Summary = raw.Abstract
				} else {
					doc.Summary = raw.Description
				}
			}
			if doc.Published == "" && len(raw.Versions) > 0 {
				var created time.Time
				created, err = time.Parse("Mon, 2 Jan 2006 15:04:05 -0700", raw.Versions[0].Created)
				if err != nil {
					err = fmt.Errorf("row %d: invalid first version date: %w", report.Read, err)
					return
				}
				doc.Published = created.Format("2006-01-02")
			}
		}
		doc, err = normalize(doc)
		if err != nil {
			err = fmt.Errorf("row %d: %w", report.Read, err)
			return
		}
		if doc.Summary == "" {
			report.MissingSummary++
		}
		var changed int
		changed, err = d.put(doc)
		if err != nil {
			return
		}
		if changed > 0 {
			report.Changed++
		} else {
			report.Unchanged++
		}
		if report.Read%batchSize == 0 {
			if err = d.exec("COMMIT"); err != nil {
				return
			}
			report.Committed = report.Read
			if progress != nil {
				checkpoint := report
				checkpoint.ElapsedMS = time.Since(start).Milliseconds()
				if err = json.NewEncoder(progress).Encode(map[string]any{"import_checkpoint": checkpoint}); err != nil {
					return
				}
			}
			if err = d.exec("BEGIN IMMEDIATE"); err != nil {
				return
			}
		}
	}
	if err = ctx.Err(); err != nil {
		return
	}
	if err = d.exec("COMMIT"); err != nil {
		return
	}
	report.Committed = report.Read
	return
}

func openMetadata(path string) (io.ReadCloser, error) {
	if path == "-" {
		return io.NopCloser(os.Stdin), nil
	}
	if path == "" {
		return nil, errors.New("metadata file path is required")
	}
	base := strings.TrimSuffix(strings.ToLower(path), ".gz")
	if strings.HasSuffix(base, ".pdf") || strings.HasSuffix(base, ".epub") {
		return nil, errors.New("use metadata files; PDF and EPUB imports are prohibited")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(path, ".gz") {
		z, e := gzip.NewReader(f)
		if e != nil {
			f.Close()
			return nil, e
		}
		return &gzipInput{z, f}, nil
	}
	return f, nil
}

type gzipInput struct {
	*gzip.Reader
	file *os.File
}

func (g *gzipInput) Close() error { g.Reader.Close(); return g.file.Close() }
