package main

// The harvester owns a separate NAS database and versioned PDFs. It never
// changes a serving catalog, indexing configuration, or an active generation.
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const arxivOAI = "https://oaipmh.arxiv.org/oai"
const arxivPDF = "https://export.arxiv.org/pdf/"

type arxivSyncState struct {
	From         string `json:"from"`
	Token        string `json:"token,omitempty"`
	ResponseDate string `json:"response_date,omitempty"`
	Updated      string `json:"updated"`
	Pages        int64  `json:"pages"`
	Records      int64  `json:"records"`
	Harvests     int64  `json:"harvests"`
}
type arxivSyncStatus struct {
	Updated         string `json:"updated"`
	State           string `json:"state"`
	From            string `json:"from"`
	Harvests        int64  `json:"harvests"`
	Records         int64  `json:"records"`
	SavedPDFs       int64  `json:"saved_pdfs"`
	PendingPDFs     int64  `json:"pending_pdfs"`
	UnavailablePDFs int64  `json:"unavailable_pdfs"`
	Bytes           int64  `json:"saved_bytes"`
	Error           string `json:"error,omitempty"`
}
type arxivRecord struct {
	Header struct {
		ID     string `xml:"identifier"`
		Date   string `xml:"datestamp"`
		Status string `xml:"status,attr"`
	} `xml:"header"`
	Raw struct {
		ID         string `xml:"id"`
		Title      string `xml:"title"`
		Authors    string `xml:"authors"`
		Abstract   string `xml:"abstract"`
		Categories string `xml:"categories"`
		DOI        string `xml:"doi"`
		License    string `xml:"license"`
		Versions   []struct {
			Number string `xml:"version,attr"`
			Date   string `xml:"date"`
		} `xml:"version"`
	} `xml:"metadata>arXivRaw"`
}
type arxivPage struct {
	XMLName xml.Name `xml:"http://www.openarchives.org/OAI/2.0/ OAI-PMH"`
	Date    string   `xml:"responseDate"`
	Error   struct {
		Code string `xml:"code,attr"`
	} `xml:"error"`
	Records []arxivRecord `xml:"ListRecords>record"`
	Token   string        `xml:"ListRecords>resumptionToken"`
}
type arxivHarvester struct {
	root      string
	db        *database
	client    *http.Client
	oai       string
	pdf       string
	userAgent string
	interval  time.Duration
	last      time.Time
}

func syncRoot() string { return os.Getenv("RESEARCH_ARXIV_SYNC_ROOT") }
func runArxivSync(args []string) error {
	f := flag.NewFlagSet("arxiv-sync", flag.ContinueOnError)
	root := f.String("directory", syncRoot(), "absolute NAS staging directory")
	from := f.String("from", "", "initial overlapping harvest date YYYY-MM-DD")
	pages := f.Int("max-pages", 500, "maximum OAI pages in this invocation")
	pdfs := f.Int("max-pdfs", 5000, "maximum PDF downloads in this invocation")
	bytes := f.Int64("max-download-mib", 16384, "download byte ceiling per invocation")
	if e := f.Parse(args); e != nil {
		return e
	}
	if f.NArg() != 0 || !filepath.IsAbs(*root) || *pages < 1 || *pages > 10000 || *pdfs < 0 || *pdfs > 20000 || *bytes < 64 || *bytes > 131072 {
		return errors.New("invalid arXiv sync limits or NAS directory")
	}
	if *from != "" {
		if _, e := time.Parse("2006-01-02", *from); e != nil {
			return errors.New("invalid initial harvest date")
		}
	}
	if e := os.MkdirAll(*root, 0700); e != nil {
		return e
	}
	lock, e := indexLock(filepath.Join(*root, "sync.lock"))
	if e != nil {
		return e
	}
	defer indexUnlock(lock)
	d, e := openDatabase(filepath.Join(*root, "updates.sqlite"), true)
	if e != nil {
		return e
	}
	defer d.close()
	if e = d.initialize(); e != nil {
		return e
	}
	if e = d.exec(`CREATE TABLE IF NOT EXISTS arxiv_updates(id TEXT PRIMARY KEY,version INTEGER NOT NULL,datestamp TEXT NOT NULL,license TEXT NOT NULL,deleted INTEGER NOT NULL,pdf_state TEXT NOT NULL,sha256 TEXT NOT NULL,bytes INTEGER NOT NULL,attempts INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS arxiv_sync_checkpoint(id INTEGER PRIMARY KEY CHECK(id=1),state TEXT NOT NULL);`); e != nil {
		return e
	}
	rows, e := d.rows("SELECT state FROM arxiv_sync_checkpoint WHERE id=1")
	if e != nil {
		return e
	}
	s := arxivSyncState{From: *from}
	if len(rows) > 0 {
		if e = json.Unmarshal([]byte(rows[0][0]), &s); e != nil {
			return e
		}
	} else if s.From == "" {
		return errors.New("first harvest requires an explicit --from coverage date")
	}
	h := arxivHarvester{root: *root, db: d, oai: arxivOAI, pdf: arxivPDF, userAgent: "ResearchAssistant/1.0 (+https://github.com/daviestechlabs/research-assistant)", interval: 3 * time.Second, client: &http.Client{Timeout: 120 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("arXiv redirects are disabled") }}}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 6*time.Hour)
	defer cancel()
	e = h.harvest(ctx, &s, *pages)
	if e == nil {
		e = h.download(ctx, *pdfs, *bytes<<20)
	}
	status, receiptErr := h.status(s, e)
	if receiptErr != nil {
		return receiptErr
	}
	if e != nil {
		return errors.New("arXiv sync paused or failed; its durable NAS checkpoint is retained")
	}
	return printJSON(status)
}
func (h *arxivHarvester) get(ctx context.Context, u string) (*http.Response, error) {
	for attempt := 0; attempt < 4; attempt++ {
		wait := h.interval - time.Since(h.last)
		if wait > 0 {
			if e := syncWait(ctx, wait); e != nil {
				return nil, e
			}
		}
		r, e := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if e != nil {
			return nil, e
		}
		r.Header.Set("User-Agent", h.userAgent)
		h.last = time.Now()
		res, e := h.client.Do(r)
		if e != nil {
			return nil, errors.New("arXiv connection failed")
		}
		if res.StatusCode != 429 && res.StatusCode != 503 && res.StatusCode < 500 {
			return res, nil
		}
		res.Body.Close()
		delay := time.Duration(30*(1<<attempt)) * time.Second
		if v, e := strconv.Atoi(res.Header.Get("Retry-After")); e == nil && v > 0 {
			delay = time.Duration(v) * time.Second
		} else if v, e := http.ParseTime(res.Header.Get("Retry-After")); e == nil && time.Until(v) > delay {
			delay = time.Until(v)
		}
		if delay > time.Hour {
			return nil, errors.New("arXiv requests a later retry")
		}
		if e = syncWait(ctx, delay); e != nil {
			return nil, e
		}
	}
	return nil, errors.New("arXiv retry limit reached")
}
func syncWait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
func (h *arxivHarvester) harvest(ctx context.Context, s *arxivSyncState, maxPages int) error {
	for n := 0; n < maxPages; n++ {
		q := url.Values{"verb": {"ListRecords"}, "metadataPrefix": {"arXivRaw"}, "from": {s.From}}
		if s.Token != "" {
			q = url.Values{"verb": {"ListRecords"}, "resumptionToken": {s.Token}}
		}
		res, e := h.get(ctx, h.oai+"?"+q.Encode())
		if e != nil {
			return e
		}
		data, e := io.ReadAll(io.LimitReader(res.Body, 16<<20+1))
		res.Body.Close()
		if e != nil || len(data) > 16<<20 || res.StatusCode != 200 {
			return errors.New("arXiv metadata response failed its bounds")
		}
		var page arxivPage
		if e = xml.Unmarshal(data, &page); e != nil {
			return errors.New("invalid OAI metadata")
		}
		if page.Error.Code == "badResumptionToken" && s.Token != "" {
			s.Token = ""
			s.ResponseDate = ""
			if e = h.saveCheckpoint(s); e != nil {
				return e
			}
			continue
		}
		if page.Error.Code != "" && page.Error.Code != "noRecordsMatch" {
			return errors.New("OAI rejected the harvest")
		}
		responseDate, e := time.Parse(time.RFC3339, page.Date)
		if e != nil {
			return errors.New("OAI response date is missing")
		}
		if s.ResponseDate == "" {
			s.ResponseDate = page.Date
		}
		if len(page.Token) > 8192 {
			return errors.New("OAI cursor exceeds its limit")
		}
		before := *s
		if e = h.db.exec("BEGIN IMMEDIATE"); e != nil {
			return e
		}
		for _, record := range page.Records {
			if e = h.storeRecord(record); e != nil {
				break
			}
			s.Records++
		}
		if e == nil {
			s.Pages++
			s.Token = strings.TrimSpace(page.Token)
			if s.Token == "" {
				first, _ := time.Parse(time.RFC3339, s.ResponseDate)
				next := first.Add(-24 * time.Hour).Format("2006-01-02")
				if next > s.From {
					s.From = next
				}
				s.ResponseDate = ""
				s.Harvests++
			}
			_ = responseDate
			e = h.saveCheckpoint(s)
		}
		if e == nil {
			e = h.db.exec("COMMIT")
		}
		if e != nil {
			h.db.exec("ROLLBACK")
			*s = before
			return e
		}
		if _, e = h.status(*s, nil); e != nil {
			return e
		}
		if s.Token == "" {
			return nil
		}
	}
	return nil // Page ceiling retains the cursor; PDFs can drain between harvests.
}
func (h *arxivHarvester) saveCheckpoint(s *arxivSyncState) error {
	s.Updated = indexTime()
	b, e := json.Marshal(s)
	if e != nil {
		return e
	}
	_, e = h.db.run("INSERT OR REPLACE INTO arxiv_sync_checkpoint VALUES(1,?)", string(b))
	return e
}
func (h *arxivHarvester) storeRecord(r arxivRecord) error {
	id := strings.TrimPrefix(r.Header.ID, "oai:arXiv.org:")
	canonical, e := canonicalID("arxiv", id)
	if e != nil {
		return e
	}
	if _, e = time.Parse("2006-01-02", r.Header.Date); e != nil {
		return errors.New("invalid OAI datestamp")
	}
	if r.Header.Status == "deleted" {
		_, e = h.db.run("INSERT INTO arxiv_updates(id,version,datestamp,license,deleted,pdf_state,sha256,bytes) VALUES(?,0,?,'',1,'deleted','',0) ON CONFLICT(id) DO UPDATE SET datestamp=excluded.datestamp,deleted=1,pdf_state='deleted'", canonical, r.Header.Date)
		if e == nil {
			_, e = h.db.run("DELETE FROM documents WHERE id=?", canonical)
		}
		return e
	}
	if r.Raw.ID != id || len(r.Raw.Versions) < 1 || len(r.Raw.Versions) > 1000 {
		return errors.New("arXiv version identity is missing")
	}
	version := 0
	published := ""
	for _, v := range r.Raw.Versions {
		n, e := strconv.Atoi(strings.TrimPrefix(v.Number, "v"))
		if e != nil || n < 1 || n > 10000 {
			return errors.New("invalid arXiv version")
		}
		date, e := http.ParseTime(v.Date)
		if e != nil {
			return errors.New("invalid arXiv version date")
		}
		if n == 1 {
			published = date.Format("2006-01-02")
		}
		if n > version {
			version = n
		}
	}
	if published == "" {
		return errors.New("arXiv publication date is missing")
	}
	doc, e := normalize(Document{ID: id, Source: "arxiv", Title: r.Raw.Title, Authors: r.Raw.Authors, Summary: r.Raw.Abstract, Categories: r.Raw.Categories, DOI: r.Raw.DOI, Published: published, NASRef: "arxiv-sync:" + id + "v" + strconv.Itoa(version)})
	if e != nil || doc.Summary == "" {
		return errors.New("arXiv record has invalid metadata or no abstract")
	}
	if _, e = h.db.put(doc); e != nil {
		return e
	}
	_, e = h.db.run(`INSERT INTO arxiv_updates(id,version,datestamp,license,deleted,pdf_state,sha256,bytes) VALUES(?,?,?,?,0,'pending','',0)
ON CONFLICT(id) DO UPDATE SET datestamp=excluded.datestamp,license=excluded.license,deleted=0,
pdf_state=CASE WHEN version=excluded.version AND pdf_state='saved' THEN 'saved' ELSE 'pending' END,
sha256=CASE WHEN version=excluded.version THEN sha256 ELSE '' END,bytes=CASE WHEN version=excluded.version THEN bytes ELSE 0 END,
attempts=CASE WHEN version=excluded.version THEN attempts ELSE 0 END,version=excluded.version`, canonical, strconv.Itoa(version), r.Header.Date, r.Raw.License)
	return e
}
func syncPDFName(id string, version int) (string, error) {
	c, e := canonicalID("arxiv", id)
	if e != nil || c != id || version < 1 || version > 10000 {
		return "", errors.New("invalid saved update ID")
	}
	return hex.EncodeToString([]byte(id)) + "v" + strconv.Itoa(version) + ".pdf", nil
}
func (h *arxivHarvester) download(ctx context.Context, max int, budget int64) error {
	if e := os.MkdirAll(filepath.Join(h.root, "pdf"), 0700); e != nil {
		return e
	}
	var used int64
	for n := 0; n < max && used+maxPDFBytes <= budget; n++ {
		if n%20 == 0 {
			rows, err := h.db.rows("SELECT state FROM arxiv_sync_checkpoint WHERE id=1")
			if err != nil {
				return err
			}
			if len(rows) == 1 {
				var state arxivSyncState
				if err = json.Unmarshal([]byte(rows[0][0]), &state); err != nil {
					return err
				}
				if _, err = h.status(state, nil); err != nil {
					return err
				}
			}
		}
		var space syscall.Statfs_t
		if e := syscall.Statfs(h.root, &space); e != nil {
			return e
		}
		if uint64(space.Bavail)*uint64(space.Bsize) < uint64(maxPDFBytes+512<<20) {
			return errors.New("insufficient free NAS space for PDF updates")
		}
		rows, e := h.db.rows("SELECT id,version FROM arxiv_updates WHERE deleted=0 AND pdf_state='pending' ORDER BY attempts,datestamp,id LIMIT 1")
		if e != nil {
			return e
		}
		if len(rows) == 0 {
			return nil
		}
		id := rows[0][0]
		version, _ := strconv.Atoi(rows[0][1])
		name, e := syncPDFName(id, version)
		if e != nil {
			return e
		}
		if _, e = h.db.run("UPDATE arxiv_updates SET attempts=attempts+1 WHERE id=?", id); e != nil {
			return e
		}
		res, e := h.get(ctx, h.pdf+strings.TrimPrefix(id, "arxiv:")+"v"+strconv.Itoa(version))
		if e != nil {
			return e
		}
		if res.StatusCode == 404 || res.StatusCode == 410 {
			res.Body.Close()
			_, e = h.db.run("UPDATE arxiv_updates SET pdf_state='unavailable' WHERE id=?", id)
			if e != nil {
				return e
			}
			continue
		}
		if res.StatusCode != 200 {
			res.Body.Close()
			return errors.New("arXiv PDF response failed")
		}
		if res.ContentLength > maxPDFBytes {
			res.Body.Close()
			_, e = h.db.run("UPDATE arxiv_updates SET pdf_state='oversized' WHERE id=?", id)
			if e != nil {
				return e
			}
			continue
		}
		f, e := os.CreateTemp(filepath.Join(h.root, "pdf"), ".download-")
		if e != nil {
			res.Body.Close()
			return e
		}
		tmp := f.Name()
		hash := sha256.New()
		size, e := io.Copy(io.MultiWriter(f, hash), io.LimitReader(res.Body, maxPDFBytes+1))
		res.Body.Close()
		if e == nil {
			e = f.Sync()
		}
		closeErr := f.Close()
		if e == nil {
			e = closeErr
		}
		if e != nil || size > maxPDFBytes || size < 8 {
			os.Remove(tmp)
			return errors.New("arXiv PDF download failed its size bounds")
		}
		if e = validSyncPDF(tmp, size); e != nil {
			os.Remove(tmp)
			return e
		}
		if e = os.Rename(tmp, filepath.Join(h.root, "pdf", name)); e != nil {
			os.Remove(tmp)
			return e
		}
		dir, e := os.Open(filepath.Join(h.root, "pdf"))
		if e != nil {
			return e
		}
		e = dir.Sync()
		dir.Close()
		if e != nil {
			return e
		}
		_, e = h.db.run("UPDATE arxiv_updates SET pdf_state='saved',sha256=?,bytes=? WHERE id=? AND version=?", hex.EncodeToString(hash.Sum(nil)), strconv.FormatInt(size, 10), id, strconv.Itoa(version))
		if e != nil {
			return e
		}
		used += size
	}
	return nil
}
func validSyncPDF(path string, size int64) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	head := make([]byte, 5)
	if _, e = io.ReadFull(f, head); e != nil || string(head) != "%PDF-" {
		return errors.New("download is not a PDF")
	}
	n := min(size, int64(2048))
	tail := make([]byte, n)
	if _, e = f.ReadAt(tail, size-n); e != nil || !strings.Contains(string(tail), "%%EOF") {
		return errors.New("PDF download is incomplete")
	}
	return nil
}
func (h *arxivHarvester) status(s arxivSyncState, runErr error) (arxivSyncStatus, error) {
	out := arxivSyncStatus{Updated: indexTime(), State: "catching-up", From: s.From, Harvests: s.Harvests, Records: s.Records}
	rows, e := h.db.rows("SELECT sum(pdf_state='saved'),sum(pdf_state='pending'),sum(pdf_state IN ('unavailable','oversized')),coalesce(sum(bytes),0) FROM arxiv_updates WHERE deleted=0")
	if e != nil {
		return out, e
	}
	if len(rows) > 0 {
		out.SavedPDFs, _ = strconv.ParseInt(rows[0][0], 10, 64)
		out.PendingPDFs, _ = strconv.ParseInt(rows[0][1], 10, 64)
		out.UnavailablePDFs, _ = strconv.ParseInt(rows[0][2], 10, 64)
		out.Bytes, _ = strconv.ParseInt(rows[0][3], 10, 64)
	}
	if s.Token == "" && s.Harvests > 0 {
		out.State = "up-to-date"
	}
	if out.PendingPDFs > 0 {
		out.State = "downloading"
	}
	if runErr != nil {
		out.State = "retry-needed"
		out.Error = "Harvest or download paused; its checkpoint is retained."
	}
	return out, writeIndexJSON(filepath.Join(h.root, "status.json"), out)
}

// The signed-in reader uses locally saved updates, never an external fallback.
func readSyncPDF(ctx context.Context, root, id string) (savedPDF, error) {
	if !filepath.IsAbs(root) {
		return savedPDF{}, errPDFMissing
	}
	d, e := openDatabase(filepath.Join(root, "updates.sqlite"), false)
	if e != nil {
		return savedPDF{}, errPDFMissing
	}
	defer d.close()
	r, e := d.rows("SELECT version,sha256,bytes FROM arxiv_updates WHERE id=? AND deleted=0 AND pdf_state='saved'", id)
	if e != nil {
		return savedPDF{}, e
	}
	if len(r) == 0 {
		return savedPDF{}, errPDFMissing
	}
	version, _ := strconv.Atoi(r[0][0])
	size, _ := strconv.ParseInt(r[0][2], 10, 64)
	name, e := syncPDFName(id, version)
	if e != nil || size < 8 || size > maxPDFBytes || !shaPattern.MatchString(r[0][1]) {
		return savedPDF{}, errors.New("saved update receipt is invalid")
	}
	if e = ctx.Err(); e != nil {
		return savedPDF{}, e
	}
	f, e := os.Open(filepath.Join(root, "pdf", name))
	if e != nil {
		return savedPDF{}, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, maxPDFBytes+1))
	hash := sha256.Sum256(b)
	if e != nil || int64(len(b)) != size || hex.EncodeToString(hash[:]) != r[0][1] {
		return savedPDF{}, errors.New("saved update checksum mismatch")
	}
	return savedPDF{Data: b, Version: int32(version), SHA256: r[0][1], Path: name}, nil
}
