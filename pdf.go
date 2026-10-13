package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/parquet-go/parquet-go"
)

const pdfPrefix = "papers/pdf/"
const maxPDFBytes = 64 << 20
const maxPDFGroupBytes = 768 << 20

var errPDFMissing = errors.New("this paper has no PDF in the saved archive")
var errPDFLarge = errors.New("this saved PDF exceeds the browser reader's memory limit")

type pdfObject struct {
	Key  string
	Size int64
	ETag string
}

// The only NAS operations are a fixed-prefix listing, HEAD, and pinned range GET.
// Neither object keys nor endpoint addresses come from browser requests.
type pdfArchive struct {
	books       map[string]libraryBook
	endpoint    string
	bucket      string
	prefix      string
	region      string
	shardCount  int
	credentials aws.Credentials
	client      *http.Client
	objects     []pdfObject
	mu          sync.Mutex
	busy        chan struct{}
	cachedID    string
	cachedPDF   savedPDF
	cachedAt    time.Time
}

func newPDFArchive() (*pdfArchive, error) {
	access, secret := os.Getenv("RESEARCH_S3_ACCESS_KEY"), os.Getenv("RESEARCH_S3_SECRET_KEY")
	if access == "" && secret == "" {
		if syncRoot() != "" {
			books, err := approvedBooks()
			if err != nil {
				return nil, err
			}
			return &pdfArchive{books: books, busy: make(chan struct{}, 1)}, nil
		}
		return nil, nil
	}
	if access == "" || secret == "" {
		return nil, errors.New("PDF archive credentials are incomplete")
	}
	endpoint := os.Getenv("RESEARCH_S3_ENDPOINT")
	u, parseErr := url.Parse(endpoint)
	bucket := os.Getenv("RESEARCH_S3_BUCKET")
	prefix := os.Getenv("RESEARCH_S3_PDF_PREFIX")
	count, countErr := strconv.Atoi(os.Getenv("RESEARCH_S3_PDF_SHARDS"))
	if parseErr != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || u.RawPath != "" || u.Opaque != "" || bucket == "" || strings.ContainsAny(bucket, "/?#\\") || !strings.HasSuffix(prefix, "/") || path.Clean(prefix)+"/" != prefix || strings.HasPrefix(prefix, "/") || countErr != nil || count < 1 || count > 10000 {
		return nil, errors.New("saved PDF archive requires a fixed origin, bucket, prefix, and verified shard count")
	}
	region := os.Getenv("RESEARCH_S3_REGION")
	if region == "" {
		region = "us-east-1"
	}
	books, err := approvedBooks()
	if err != nil {
		return nil, err
	}
	return &pdfArchive{books: books, endpoint: endpoint, bucket: bucket, prefix: prefix, region: region, shardCount: count, credentials: aws.Credentials{AccessKeyID: access, SecretAccessKey: secret},
		client: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("NAS redirects are disabled") }}, busy: make(chan struct{}, 1)}, nil
}

func (a *pdfArchive) request(ctx context.Context, method, key string, query url.Values, headers http.Header) (*http.Response, error) {
	bucket, region := a.bucket, a.region
	if bucket == "" {
		bucket = "training-data"
	} // Test fixtures use an isolated synthetic bucket.
	if region == "" {
		region = "us-east-1"
	}
	u := a.endpoint + "/" + bucket + "/" + key
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	r, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, errors.New("invalid archive request")
	}
	for k, values := range headers {
		r.Header[k] = values
	}
	const emptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	r.Header.Set("X-Amz-Content-Sha256", emptyHash)
	err = v4.NewSigner().SignHTTP(ctx, a.credentials, r, emptyHash, "s3", region, time.Now(), func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	if err != nil {
		return nil, errors.New("archive signing failed")
	}
	response, err := a.client.Do(r)
	if err != nil {
		return nil, errors.New("saved PDF storage is unavailable")
	}
	if response.StatusCode != 200 && response.StatusCode != 206 {
		response.Body.Close()
		return nil, errors.New("saved archive request was rejected")
	}
	return response, nil
}

func (a *pdfArchive) inventory(ctx context.Context) ([]pdfObject, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.objects) > 0 {
		return a.objects, nil
	}
	prefix, count := a.prefix, a.shardCount
	if prefix == "" {
		prefix = pdfPrefix
	}
	if count == 0 {
		count = 1
	}
	objects := []pdfObject{}
	next := ""
	for page := 0; page < 10; page++ {
		query := url.Values{"list-type": {"2"}, "prefix": {prefix}, "max-keys": {"1000"}}
		if next != "" {
			query.Set("continuation-token", next)
		}
		r, err := a.request(ctx, "GET", "", query, nil)
		if err != nil {
			return nil, err
		}
		var listing struct {
			Objects   []pdfObject `xml:"Contents"`
			Truncated bool        `xml:"IsTruncated"`
			Next      string      `xml:"NextContinuationToken"`
		}
		err = xml.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&listing)
		r.Body.Close()
		if err != nil {
			return nil, errors.New("invalid PDF archive inventory")
		}
		for _, o := range listing.Objects {
			if !strings.HasPrefix(o.Key, prefix+"train-") || !strings.HasSuffix(o.Key, ".parquet") || o.Size <= 0 || o.ETag == "" {
				return nil, errors.New("unexpected PDF archive object")
			}
			objects = append(objects, o)
		}
		if !listing.Truncated {
			if len(objects) != count {
				return nil, errors.New("PDF archive shard count changed")
			}
			sort.Slice(objects, func(i, j int) bool { return objects[i].Key < objects[j].Key })
			for i, o := range objects {
				if o.Key != fmt.Sprintf("%strain-%05d-of-%05d.parquet", prefix, i, count) {
					return nil, errors.New("PDF archive shard sequence changed")
				}
			}
			a.objects = objects
			return objects, nil
		}
		if listing.Next == "" || listing.Next == next {
			return nil, errors.New("invalid PDF inventory continuation")
		}
		next = listing.Next
	}
	return nil, errors.New("PDF inventory exceeds its listing limit")
}

type pdfRanges struct {
	archive *pdfArchive
	ctx     context.Context
	object  pdfObject
	mu      sync.Mutex
	read    int64
}

func (s *pdfRanges) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= s.object.Size {
		return 0, io.EOF
	}
	length := len(p)
	if int64(length) > s.object.Size-off {
		length = int(s.object.Size - off)
	}
	if length == 0 {
		return 0, nil
	}
	s.mu.Lock()
	s.read += int64(length)
	exceeded := s.read > 1024<<20 || length > 768<<20
	s.mu.Unlock()
	if exceeded {
		return 0, errPDFLarge
	}
	headers := http.Header{"Range": {fmt.Sprintf("bytes=%d-%d", off, off+int64(length)-1)}, "If-Match": {s.object.ETag}}
	r, err := s.archive.request(s.ctx, "GET", s.object.Key, nil, headers)
	if err != nil {
		return 0, err
	}
	defer r.Body.Close()
	if r.StatusCode != 206 || r.Header.Get("ETag") != s.object.ETag || r.Header.Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", off, off+int64(length)-1, s.object.Size) {
		return 0, errors.New("PDF archive snapshot changed or range was ignored")
	}
	n, err := io.ReadFull(r.Body, p[:length])
	if err != nil {
		return n, err
	}
	if length < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (a *pdfArchive) open(ctx context.Context, o pdfObject) (*parquet.File, error) {
	return parquet.OpenFile(&pdfRanges{archive: a, ctx: ctx, object: o}, o.Size, parquet.SkipPageIndex(true), parquet.SkipBloomFilters(true), parquet.ReadBufferSize(64<<10))
}

func pdfBounds(g parquet.RowGroup) (string, string, error) {
	if g.NumRows() == 0 || len(g.ColumnChunks()) != 8 {
		return "", "", errors.New("unexpected PDF shard schema")
	}
	// The corpus uses flat, required columns and sorted paper IDs.
	chunk, ok := g.ColumnChunks()[0].(*parquet.FileColumnChunk)
	if !ok {
		return "", "", errors.New("invalid PDF paper-ID column")
	}
	min, max, ok := chunk.Bounds()
	if !ok {
		return "", "", errors.New("PDF paper-ID bounds are unavailable")
	}
	return string(min.ByteArray()), string(max.ByteArray()), nil
}

type pdfMetadata struct {
	PaperID string `parquet:"paper_id"`
	Version int32  `parquet:"version"`
	Path    string `parquet:"path"`
	Size    int64  `parquet:"size"`
	SHA256  string `parquet:"sha256"`
}

type savedPDF struct {
	Data    []byte
	Version int32
	SHA256  string
	Path    string
}

func primaryPDF(p pdfMetadata) bool {
	name := path.Base(p.Path)
	for _, id := range []string{p.PaperID, strings.ReplaceAll(p.PaperID, "/", ""), strings.ReplaceAll(p.PaperID, "/", "_")} {
		if name == fmt.Sprintf("%sv%d.pdf", id, p.Version) {
			return true
		}
	}
	return false
}

func (a *pdfArchive) readPDF(ctx context.Context, id string) (savedPDF, error) {
	result := savedPDF{}
	a.mu.Lock()
	if a.cachedID == id && time.Since(a.cachedAt) < 5*time.Minute {
		cached := a.cachedPDF
		a.mu.Unlock()
		return cached, nil
	}
	a.mu.Unlock()
	objects, err := a.inventory(ctx)
	if err != nil {
		return result, err
	}
	// The source snapshot sorts paper IDs across shard boundaries. Binary search
	// reads metadata from about eleven shards, never their content columns.
	lo, hi := 0, len(objects)
	files := map[int]*parquet.File{}
	for lo < hi {
		mid := (lo + hi) / 2
		f, e := a.open(ctx, objects[mid])
		if e != nil {
			return result, errors.New("cannot read saved PDF shard metadata")
		}
		files[mid] = f
		groups := f.RowGroups()
		if len(groups) == 0 {
			return result, errors.New("empty PDF shard")
		}
		_, max, e := pdfBounds(groups[len(groups)-1])
		if e != nil {
			return result, e
		}
		if max < id {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo == len(objects) {
		return result, errPDFMissing
	}
	f := files[lo]
	if f == nil {
		f, err = a.open(ctx, objects[lo])
		if err != nil {
			return result, err
		}
	}
	if fields := f.Schema().Fields(); len(fields) != 8 || fields[0].Name() != "paper_id" || fields[7].Name() != "content" {
		return result, errors.New("PDF archive schema changed")
	}
	bestGroup, bestRow := -1, -1
	var best pdfMetadata
	for gi, g := range f.RowGroups() {
		min, max, e := pdfBounds(g)
		if e != nil {
			return result, e
		}
		if id < min || id > max {
			continue
		}
		r := parquet.NewGenericRowGroupReader[pdfMetadata](g)
		rows := make([]pdfMetadata, 1)
		row := 0
		for {
			n, e := r.Read(rows)
			if n > 0 && rows[0].PaperID == id && (bestGroup < 0 || rows[0].Version > best.Version || (rows[0].Version == best.Version && (primaryPDF(rows[0]) && !primaryPDF(best) || primaryPDF(rows[0]) == primaryPDF(best) && rows[0].Path < best.Path))) {
				best = rows[0]
				bestGroup = gi
				bestRow = row
			}
			row += n
			if e == io.EOF {
				break
			}
			if e != nil {
				r.Close()
				return result, errors.New("cannot read saved PDF inventory rows")
			}
		}
		r.Close()
	}
	if bestGroup < 0 {
		return result, errPDFMissing
	}
	if best.Size <= 0 || best.Size > maxPDFBytes {
		return result, errPDFLarge
	}
	metadata := f.Metadata().RowGroups[bestGroup]
	if metadata.TotalByteSize > maxPDFGroupBytes {
		return result, errPDFLarge
	}
	if metadata.Columns[7].MetaData.TotalCompressedSize > 768<<20 {
		return result, errPDFLarge
	}
	// Only the chosen group's content column is decoded. Other shard content
	// and other columns remain unread. Select one row, then verify its digest.
	pages := f.RowGroups()[bestGroup].ColumnChunks()[7].Pages()
	defer pages.Close()
	if err = pages.SeekToRow(int64(bestRow)); err != nil {
		return result, errors.New("cannot locate saved PDF content")
	}
	page, err := pages.ReadPage()
	if err != nil {
		return result, errors.New("cannot decode saved PDF content")
	}
	values := make([]parquet.Value, 1)
	n, err := page.Values().ReadValues(values)
	if n != 1 || (err != nil && err != io.EOF) {
		return result, errors.New("saved PDF content is incomplete")
	}
	data := values[0].ByteArray()
	if int64(len(data)) != best.Size || !bytes.HasPrefix(data, []byte("%PDF-")) {
		return result, errors.New("saved object is not a complete PDF")
	}
	hash := sha256.Sum256(data)
	if hex.EncodeToString(hash[:]) != best.SHA256 {
		return result, errors.New("saved PDF checksum mismatch")
	}
	result = savedPDF{Data: append([]byte(nil), data...), Version: best.Version, SHA256: best.SHA256, Path: best.Path}
	a.mu.Lock()
	// Retain only one verified PDF so Safari's subsequent range requests do
	// not decode the same archive group again. No PDF goes onto disk.
	a.cachedID = id
	a.cachedPDF = result
	a.cachedAt = time.Now()
	a.mu.Unlock()
	return result, nil
}

func (a *pdfArchive) servePDF(w http.ResponseWriter, r *http.Request) {
	id, err := canonicalSavedID(r.URL.Query().Get("id"))
	if err != nil || r.URL.Query().Get("id") != id {
		http.Error(w, "invalid paper ID", 400)
		return
	}
	if strings.HasPrefix(id, "dnd:") && a.books[id].MediaType != "application/pdf" {
		http.Error(w, "Unknown saved PDF book", 404)
		return
	}
	requestedSHA := r.URL.Query().Get("sha256")
	if requestedSHA != "" {
		decoded, decodeErr := hex.DecodeString(requestedSHA)
		if decodeErr != nil || len(decoded) != 32 || len(requestedSHA) != 64 {
			http.Error(w, "invalid PDF checksum", 400)
			return
		}
	}
	select {
	case a.busy <- struct{}{}:
	default:
		http.Error(w, "Another saved PDF is loading. Please try again shortly.", 429)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	pdf, err := a.readSourcePDF(ctx, id)
	// Release the archive reader before sending bytes. Concurrent Safari range
	// responses share the verified cache rather than holding the loading gate.
	<-a.busy
	if err != nil {
		code := 502
		if errors.Is(err, errPDFMissing) {
			code = 404
		}
		if errors.Is(err, errPDFLarge) {
			code = 413
		}
		// Internal storage errors and credential-bearing requests never enter responses.
		message := "The saved PDF could not load. Please try again."
		if code == 404 || code == 413 {
			message = err.Error()
		}
		http.Error(w, message, code)
		return
	}
	if requestedSHA != "" && requestedSHA != pdf.SHA256 {
		http.Error(w, "The saved PDF version changed. Read it again to refresh page citations.", http.StatusConflict)
		return
	}
	filename := strings.ReplaceAll(strings.TrimPrefix(id, "arxiv:"), "/", "-") + "v" + strconv.Itoa(int(pdf.Version)) + ".pdf"
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", `inline; filename="`+filename+`"`)
	w.Header().Set("ETag", `"`+pdf.SHA256+`"`)
	w.Header().Set("X-Saved-PDF-Version", strconv.Itoa(int(pdf.Version)))
	http.ServeContent(w, r, filename, time.Time{}, bytes.NewReader(pdf.Data))
}
