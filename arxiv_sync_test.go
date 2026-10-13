package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func syncFixture(t *testing.T) (*arxivHarvester, *arxivSyncState) {
	t.Helper()
	root := t.TempDir()
	if e := runArxivSync([]string{"--directory", root, "--from", "invalid"}); e == nil {
		t.Fatal("invalid date accepted")
	}
	d, e := openDatabase(filepath.Join(root, "updates.sqlite"), true)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(d.close)
	if e = d.initialize(); e != nil {
		t.Fatal(e)
	}
	if e = d.exec(`CREATE TABLE arxiv_updates(id TEXT PRIMARY KEY,version INTEGER NOT NULL,datestamp TEXT NOT NULL,license TEXT NOT NULL,deleted INTEGER NOT NULL,pdf_state TEXT NOT NULL,sha256 TEXT NOT NULL,bytes INTEGER NOT NULL,attempts INTEGER NOT NULL DEFAULT 0);CREATE TABLE arxiv_sync_checkpoint(id INTEGER PRIMARY KEY,state TEXT NOT NULL);`); e != nil {
		t.Fatal(e)
	}
	return &arxivHarvester{root: root, db: d, userAgent: "fixture"}, &arxivSyncState{From: "2026-10-01"}
}
func oaiFixture(id, version, abstract string) string {
	return fmt.Sprintf(`<record><header><identifier>oai:arXiv.org:%s</identifier><datestamp>2026-10-12</datestamp></header><metadata><arXivRaw xmlns="http://arxiv.org/OAI/arXivRaw/"><id>%s</id><version version="v1"><date>Mon, 12 Oct 2026 12:00:00 GMT</date></version>%s<title>Geological experiments</title><authors>A. Author</authors><categories>physics.geo-ph</categories><license>https://creativecommons.org/licenses/by/4.0/</license><abstract>%s</abstract></arXivRaw></metadata></record>`, id, id, version, abstract)
}
func TestArxivSyncResumeDeduplicateVersionsAndChecksum(t *testing.T) {
	h, s := syncFixture(t)
	requests, pdfs := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("unexpected credential")
		}
		if strings.HasPrefix(r.URL.Path, "/pdf/") {
			pdfs++
			w.Write([]byte("%PDF-1.4\nfixture\n%%EOF\n"))
			return
		}
		requests++
		token := ""
		record := oaiFixture("2610.00001", "", "Geology surveys compare rock structures.")
		if r.URL.Query().Get("resumptionToken") == "" {
			token = "resume &amp; token"
		} else {
			if r.URL.Query().Get("metadataPrefix") != "" || r.URL.Query().Get("from") != "" {
				t.Error("resume leaked initial parameters")
			}
			record = oaiFixture("2610.00002", "", "Geology studies mineral deposits.")
		}
		fmt.Fprintf(w, `<OAI-PMH xmlns="http://www.openarchives.org/OAI/2.0/"><responseDate>2026-10-13T03:00:00Z</responseDate><ListRecords>%s<resumptionToken>%s</resumptionToken></ListRecords></OAI-PMH>`, record, token)
	}))
	defer server.Close()
	h.oai = server.URL
	h.pdf = server.URL + "/pdf/"
	h.client = server.Client()
	if e := h.harvest(context.Background(), s, 1); e != nil {
		t.Fatal(e)
	}
	if s.Token != "resume & token" || s.Harvests != 0 || s.From != "2026-10-01" {
		t.Fatalf("page incorrectly completed: %+v", s)
	}
	r, e := h.db.rows("SELECT state FROM arxiv_sync_checkpoint WHERE id=1")
	if e != nil {
		t.Fatal(e)
	}
	var resumed arxivSyncState
	if e = json.Unmarshal([]byte(r[0][0]), &resumed); e != nil {
		t.Fatal(e)
	}
	if e = h.harvest(context.Background(), &resumed, 2); e != nil {
		t.Fatal(e)
	}
	if resumed.From != "2026-10-12" || resumed.Harvests != 1 || requests != 2 {
		t.Fatalf("bad completed checkpoint: %+v", resumed)
	}
	if e = h.download(context.Background(), 10, 128<<20); e != nil {
		t.Fatal(e)
	}
	if pdfs != 2 {
		t.Fatal("wrong download count")
	}
	if e = h.download(context.Background(), 10, 128<<20); e != nil || pdfs != 2 {
		t.Fatal("redownloaded saved versions")
	}
	p, e := readSyncPDF(context.Background(), h.root, "arxiv:2610.00001")
	if e != nil || p.Version != 1 {
		t.Fatalf("saved reader: %+v %v", p, e)
	}
	name, _ := syncPDFName("arxiv:2610.00001", 1)
	if e = os.WriteFile(filepath.Join(h.root, "pdf", name), []byte("corrupt"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = readSyncPDF(context.Background(), h.root, "arxiv:2610.00001"); e == nil {
		t.Fatal("corrupt saved PDF accepted")
	}
}
func TestArxivSyncPageRollbackExpiredCursorAndDeletion(t *testing.T) {
	h, s := syncFixture(t)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			fmt.Fprint(w, `<OAI-PMH xmlns="http://www.openarchives.org/OAI/2.0/"><error code="badResumptionToken">expired</error></OAI-PMH>`)
			return
		}
		fmt.Fprintf(w, `<OAI-PMH xmlns="http://www.openarchives.org/OAI/2.0/"><responseDate>2026-10-13T03:00:00Z</responseDate><ListRecords>%s%s</ListRecords></OAI-PMH>`, oaiFixture("2610.00001", "", "A valid abstract."), oaiFixture("../../escape", "", "Invalid ID."))
	}))
	defer server.Close()
	h.oai = server.URL
	h.client = server.Client()
	s.Token = "expired"
	if e := h.harvest(context.Background(), s, 2); e == nil {
		t.Fatal("bad record accepted")
	}
	r, _ := h.db.rows("SELECT count(*) FROM documents")
	if r[0][0] != "0" || s.Records != 0 || s.From != "2026-10-01" {
		t.Fatal("partial page advanced checkpoint")
	}
	var rec arxivRecord
	if e := decodeSyncFixture(oaiFixture("2610.00001", "", "A valid abstract."), &rec); e != nil {
		t.Fatal(e)
	}
	if e := h.storeRecord(rec); e != nil {
		t.Fatal(e)
	}
	rec.Header.Status = "deleted"
	if e := h.storeRecord(rec); e != nil {
		t.Fatal(e)
	}
	r, _ = h.db.rows("SELECT deleted,pdf_state FROM arxiv_updates")
	if r[0][0] != "1" || r[0][1] != "deleted" {
		t.Fatal("deletion lost")
	}
}
func TestArxivSyncRejectsHTMLAndKeepsPending(t *testing.T) {
	h, _ := syncFixture(t)
	var rec arxivRecord
	decodeSyncFixture(oaiFixture("2610.00001", "", "A valid abstract."), &rec)
	if e := h.storeRecord(rec); e != nil {
		t.Fatal(e)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "<html>not a PDF</html>") }))
	defer server.Close()
	h.client = server.Client()
	h.pdf = server.URL + "/pdf/"
	if e := h.download(context.Background(), 1, 64<<20); e == nil {
		t.Fatal("HTML accepted")
	}
	r, _ := h.db.rows("SELECT pdf_state FROM arxiv_updates")
	if r[0][0] != "pending" {
		t.Fatal("failed PDF marked saved")
	}
	entries, _ := os.ReadDir(filepath.Join(h.root, "pdf"))
	if len(entries) != 0 {
		t.Fatal("failed file retained")
	}
	for _, id := range []string{"../../escape", "arxiv:2610.00001v1", "https://evil.test/"} {
		if _, e := syncPDFName(id, 1); e == nil {
			t.Fatal("invalid saved path accepted")
		}
	}
}
func decodeSyncFixture(s string, r *arxivRecord) error { return xml.Unmarshal([]byte(s), r) }
