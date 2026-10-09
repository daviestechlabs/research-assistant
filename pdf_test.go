package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/parquet-go/parquet-go"
)

type fixturePDF struct {
	PaperID string `parquet:"paper_id"`
	Version int32  `parquet:"version"`
	Kind    string `parquet:"kind"`
	Source  string `parquet:"source"`
	Path    string `parquet:"path"`
	Size    int64  `parquet:"size"`
	SHA256  string `parquet:"sha256"`
	Content []byte `parquet:"content"`
}

func fakePDFArchive(t *testing.T, corrupt, changed bool, size int64) *pdfArchive {
	t.Helper()
	var buffer bytes.Buffer
	w := parquet.NewGenericWriter[fixturePDF](&buffer, parquet.MaxRowsPerRowGroup(2))
	rows := []fixturePDF{}
	for _, v := range []int32{1, 2, 3} {
		data := []byte(fmt.Sprintf("%%PDF-1.7\nSaved version %d\n%%%%EOF\n", v))
		hash := sha256.Sum256(data)
		digest := hex.EncodeToString(hash[:])
		if corrupt {
			digest = strings.Repeat("0", 64)
		}
		n := int64(len(data))
		if size > 0 {
			n = size
		}
		rows = append(rows, fixturePDF{PaperID: "0704.0001", Version: v, Kind: "pdf", Source: "saved", Path: fmt.Sprintf("0704.0001v%d.pdf", v), Size: n, SHA256: digest, Content: data})
	}
	if _, err := w.Write(rows); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data := buffer.Bytes()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/training-data/"+pdfPrefix+"train-00000-of-01886.parquet" {
			t.Errorf("unexpected NAS operation %s %s", r.Method, r.URL.Path)
			http.Error(w, "forbidden", 403)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") || r.Header.Get("If-Match") != `"snapshot"` {
			t.Error("unsigned or unpinned read")
		}
		var start, end int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil || start < 0 || end >= len(data) {
			http.Error(w, "bad range", 400)
			return
		}
		etag := `"snapshot"`
		if changed {
			etag = `"changed"`
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.WriteHeader(206)
		w.Write(data[start : end+1])
	}))
	t.Cleanup(s.Close)
	return &pdfArchive{endpoint: s.URL, credentials: aws.Credentials{AccessKeyID: "test", SecretAccessKey: "test"}, client: s.Client(), objects: []pdfObject{{Key: pdfPrefix + "train-00000-of-01886.parquet", Size: int64(len(data)), ETag: `"snapshot"`}}, busy: make(chan struct{}, 1)}
}

func TestSavedPDFLatestVersionAndHTTPRanges(t *testing.T) {
	a := fakePDFArchive(t, false, false, 0)
	pdf, err := a.readPDF(context.Background(), "0704.0001")
	if err != nil || pdf.Version != 3 || !bytes.Contains(pdf.Data, []byte("Saved version 3")) {
		t.Fatalf("latest PDF: %+v %v", pdf, err)
	}
	if _, err = a.readPDF(context.Background(), "0704.9999"); !errors.Is(err, errPDFMissing) {
		t.Fatalf("missing PDF: %v", err)
	}
	r := httptest.NewRequest("GET", "http://desk/api/pdf?id=arxiv:0704.0001", nil)
	r.Header.Set("Range", "bytes=0-7")
	w := httptest.NewRecorder()
	a.servePDF(w, r)
	if w.Code != 206 || w.Body.String() != "%PDF-1.7" || w.Header().Get("Content-Type") != "application/pdf" || !strings.Contains(w.Header().Get("Content-Disposition"), "v3.pdf") {
		t.Fatalf("range response: %d %s %v", w.Code, w.Body.String(), w.Header())
	}
	for _, id := range []string{"../../secret", "arxiv:0704.0001v3", "https://arxiv.org/abs/0704.0001", "gutenberg:1"} {
		w = httptest.NewRecorder()
		a.servePDF(w, httptest.NewRequest("GET", "http://desk/api/pdf?id="+id, nil))
		if w.Code != 400 {
			t.Fatalf("accepted arbitrary reference %q: %d", id, w.Code)
		}
	}
}

func TestSavedPDFIntegritySnapshotAndSize(t *testing.T) {
	for _, tc := range []struct {
		name             string
		corrupt, changed bool
		size             int64
	}{{"checksum", true, false, 0}, {"snapshot", false, true, 0}, {"size", false, false, maxPDFBytes + 1}} {
		t.Run(tc.name, func(t *testing.T) {
			a := fakePDFArchive(t, tc.corrupt, tc.changed, tc.size)
			if _, err := a.readPDF(context.Background(), "0704.0001"); err == nil {
				t.Fatal("invalid saved PDF accepted")
			}
		})
	}
}

func TestSavedPDFPageCitationPinsChecksum(t *testing.T) {
	a := fakePDFArchive(t, false, false, 0)
	pdf, err := a.readPDF(context.Background(), "0704.0001")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		sha    string
		status int
	}{{"bad", 400}, {strings.Repeat("b", 64), 409}, {pdf.SHA256, 200}} {
		w := httptest.NewRecorder()
		a.servePDF(w, httptest.NewRequest("GET", "http://desk/api/pdf?id=arxiv:0704.0001&sha256="+tc.sha, nil))
		if w.Code != tc.status {
			t.Fatalf("version pin %s: %d", tc.sha, w.Code)
		}
	}
}

func TestSavedPDFRequiresIdentityAndHidesStorage(t *testing.T) {
	d := testDB(t)
	loadFixture(t, d)
	a := fakePDFArchive(t, false, false, 0)
	h := securedPDFHandler(d, testInference(t, d), "https://research.example", &tokenVerifier{}, a)
	for _, header := range []string{"", "Bearer forged"} {
		r := httptest.NewRequest("GET", "https://research.example/api/pdf?id=arxiv:0704.0001", nil)
		r.Header.Set("Authorization", header)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("private PDF access: %d", w.Code)
		}
	}
	docs, _ := d.getDocuments([]string{"arxiv:1706.03762"})
	clean := browserDocuments(docs, true)
	if clean[0].PDFURL != "/api/pdf?id=arxiv:1706.03762" || clean[0].NASRef != "" || docs[0].PDFURL != "" {
		t.Fatal("invalid browser PDF reference")
	}
}

func TestTopicSearchUsesAbstract(t *testing.T) {
	d := testDB(t)
	loadFixture(t, d)
	for _, query := range []string{"Vaswani", "Black holes", "retrieval augmented"} {
		result, err := d.search(SearchRequest{Query: query})
		if err != nil || len(result.Documents) != 0 {
			t.Fatalf("title or author match escaped abstract scope: %s %+v %v", query, result, err)
		}
	}
	result, err := d.search(SearchRequest{Query: "gravity quantum"})
	if err != nil || len(result.Documents) != 1 || result.Method != "abstract-bm25" {
		t.Fatalf("abstract-only topic: %+v %v", result, err)
	}
}
