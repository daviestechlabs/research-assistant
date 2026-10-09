package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/parquet-go/parquet-go"
)

func indexStreamSHA(r io.Reader) (string, error) {
	h := sha256.New()
	if _, e := io.Copy(h, r); e != nil {
		return "", e
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func collectionStore(c indexCollection) (*pdfArchive, error) {
	access, secret := os.Getenv(c.AccessEnv), os.Getenv(c.SecretEnv)
	if access == "" || secret == "" {
		return nil, errors.New("collection read credentials are unavailable")
	}
	region := c.Region
	if region == "" {
		region = "us-east-1"
	}
	return &pdfArchive{endpoint: strings.TrimSuffix(c.Endpoint, "/"), bucket: c.Bucket, region: region, credentials: aws.Credentials{AccessKeyID: access, SecretAccessKey: secret}, client: &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("storage redirects are disabled") }}}, nil
}
func indexHead(ctx context.Context, a *pdfArchive, key string) (indexObject, error) {
	var o indexObject
	r, e := a.request(ctx, "HEAD", key, nil, nil)
	if e != nil {
		return o, e
	}
	defer r.Body.Close()
	size, e := strconv.ParseInt(r.Header.Get("Content-Length"), 10, 64)
	if e != nil || size < 1 || r.Header.Get("ETag") == "" {
		return o, errors.New("metadata snapshot has no verified size or ETag")
	}
	return indexObject{Key: key, ETag: r.Header.Get("ETag"), Bytes: size}, nil
}
func discoverIndexObjects(ctx context.Context, a *pdfArchive, c indexCollection, max int) ([]indexObject, error) {
	if c.Key != "" {
		o, e := indexHead(ctx, a, c.Key)
		return []indexObject{o}, e
	}
	var out []indexObject
	next := ""
	for page := 0; page < 100; page++ {
		q := url.Values{"list-type": {"2"}, "prefix": {c.Prefix}, "max-keys": {"1000"}}
		if next != "" {
			q.Set("continuation-token", next)
		}
		r, e := a.request(ctx, "GET", "", q, nil)
		if e != nil {
			return nil, e
		}
		var listing struct {
			Objects   []pdfObject `xml:"Contents"`
			Truncated bool        `xml:"IsTruncated"`
			Next      string      `xml:"NextContinuationToken"`
		}
		e = xml.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&listing)
		r.Body.Close()
		if e != nil {
			return nil, errors.New("metadata inventory response is invalid")
		}
		for _, o := range listing.Objects {
			ext := map[string]string{"jsonl": ".jsonl", "arxiv-parquet": ".parquet", "gutenberg-manifest": ".json"}[c.Adapter]
			if !strings.HasSuffix(o.Key, ext) {
				continue
			}
			if !strings.HasPrefix(o.Key, c.Prefix) || !safeIndexKey(o.Key) || o.Size < 1 || o.ETag == "" {
				return nil, errors.New("metadata object is outside the configured collection")
			}
			out = append(out, indexObject{Key: o.Key, ETag: o.ETag, Bytes: o.Size})
			if len(out) > max {
				return nil, errors.New("collection inventory exceeds its configured object limit")
			}
		}
		if !listing.Truncated {
			sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
			if len(out) == 0 {
				return nil, errors.New("no supported metadata objects were found")
			}
			return out, nil
		}
		if listing.Next == "" || listing.Next == next {
			return nil, errors.New("metadata inventory cursor is invalid")
		}
		next = listing.Next
	}
	return nil, errors.New("metadata inventory exceeds its page limit")
}

// Metadata-only Parquet range reads. Reject corpus-body schemas, project only
// these columns, pin ETag, and cap each range and total bytes per object.
type indexRanges struct {
	ctx  context.Context
	a    *pdfArchive
	o    indexObject
	read int64
}

func (r *indexRanges) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= r.o.Bytes {
		return 0, io.EOF
	}
	n := len(p)
	if int64(n) > r.o.Bytes-off {
		n = int(r.o.Bytes - off)
	}
	r.read += int64(n)
	if n > 64<<20 || r.read > 8<<30 {
		return 0, errors.New("metadata range budget exhausted")
	}
	resp, e := r.a.request(r.ctx, "GET", r.o.Key, nil, http.Header{"Range": {fmt.Sprintf("bytes=%d-%d", off, off+int64(n)-1)}, "If-Match": {r.o.ETag}})
	if e != nil {
		return 0, e
	}
	defer resp.Body.Close()
	if resp.StatusCode != 206 || resp.Header.Get("ETag") != r.o.ETag || resp.Header.Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", off, off+int64(n)-1, r.o.Bytes) {
		return 0, errors.New("metadata snapshot changed or ignored its range")
	}
	got, e := io.ReadFull(resp.Body, p[:n])
	if e == nil && n < len(p) {
		e = io.EOF
	}
	return got, e
}

type indexPaper struct {
	ID         string     `parquet:"paper_id"`
	Title      *string    `parquet:"title,optional"`
	Authors    *string    `parquet:"authors,optional"`
	Abstract   *string    `parquet:"abstract,optional"`
	Categories *string    `parquet:"categories,optional"`
	DOI        *string    `parquet:"doi,optional"`
	Date       *time.Time `parquet:"first_version_date,optional,timestamp(millisecond)"`
}

func indexString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
func indexParquet(ctx context.Context, a *pdfArchive, o indexObject) (*parquet.File, error) {
	f, e := parquet.OpenFile(&indexRanges{ctx: ctx, a: a, o: o}, o.Bytes, parquet.SkipPageIndex(true), parquet.SkipBloomFilters(true), parquet.ReadBufferSize(64<<10))
	if e != nil {
		return nil, errors.New("metadata Parquet footer is invalid")
	}
	fields := map[string]bool{}
	for _, field := range f.Schema().Fields() {
		name := field.Name()
		fields[name] = true
		if name == "content" || name == "text" || name == "body" || name == "pdf" {
			return nil, errors.New("this object contains corpus bodies; choose a metadata catalog")
		}
	}
	for _, name := range []string{"paper_id", "title", "authors", "abstract", "categories", "doi", "first_version_date"} {
		if !fields[name] {
			return nil, errors.New("abstract metadata schema does not match")
		}
	}
	return f, nil
}
func metadataObject(ctx context.Context, a *pdfArchive, o indexObject) (io.ReadCloser, error) {
	r, e := a.request(ctx, "GET", o.Key, nil, http.Header{"If-Match": {o.ETag}})
	if e != nil {
		return nil, e
	}
	if r.Header.Get("ETag") != o.ETag {
		r.Body.Close()
		return nil, errors.New("metadata snapshot changed")
	}
	return r.Body, nil
}
func gutenbergEntries(ctx context.Context, a *pdfArchive, o indexObject) ([]Document, error) {
	if o.Bytes > 64<<20 {
		return nil, errors.New("book metadata manifest exceeds 64 MiB")
	}
	body, e := metadataObject(ctx, a, o)
	if e != nil {
		return nil, e
	}
	defer body.Close()
	var raw struct {
		Counts struct {
			Targets int `json:"targets"`
		} `json:"counts"`
		Entries []struct {
			ID          int    `json:"gutenberg_id"`
			Title       string `json:"title"`
			Authors     string `json:"authors"`
			Language    string `json:"language"`
			Description string `json:"description"`
			Ref         string `json:"s3_uri"`
		} `json:"entries"`
	}
	if e = json.NewDecoder(io.LimitReader(body, 64<<20+1)).Decode(&raw); e != nil || len(raw.Entries) != raw.Counts.Targets {
		return nil, errors.New("book manifest accounting is invalid")
	}
	out := make([]Document, 0, len(raw.Entries))
	for _, r := range raw.Entries {
		out = append(out, Document{ID: strconv.Itoa(r.ID), Source: "gutenberg", Title: r.Title, Authors: r.Authors, Language: r.Language, Summary: r.Description, NASRef: r.Ref})
	}
	return out, nil
}
func indexObjectRows(ctx context.Context, a *pdfArchive, c indexCollection, o indexObject) (int64, error) {
	if c.Adapter == "arxiv-parquet" {
		f, e := indexParquet(ctx, a, o)
		if e != nil {
			return 0, e
		}
		return f.NumRows(), nil
	}
	if c.Adapter == "gutenberg-manifest" {
		v, e := gutenbergEntries(ctx, a, o)
		return int64(len(v)), e
	}
	body, e := metadataObject(ctx, a, o)
	if e != nil {
		return 0, e
	}
	defer body.Close()
	if o.Bytes > 8<<30 {
		return 0, errors.New("metadata stream exceeds its size limit")
	}
	s := bufio.NewScanner(io.LimitReader(body, o.Bytes+1))
	s.Buffer(make([]byte, 65536), 1<<20)
	var count int64
	for s.Scan() {
		count++
	}
	return count, s.Err()
}

// Stream normalized rows in bounded batches. A cursor advances only in the
// same SQLite transaction that commits the batch, so replays cannot skip rows.
func streamIndexObject(ctx context.Context, a *pdfArchive, c indexCollection, o indexObject, offset int64, accept func([]Document, int64) error) error {
	batch := make([]Document, 0, 500)
	cursor := int64(0)
	emit := func(d Document) error {
		cursor++
		if cursor <= offset {
			return nil
		}
		batch = append(batch, d)
		if len(batch) == 500 {
			if e := accept(batch, cursor); e != nil {
				return e
			}
			batch = batch[:0]
		}
		return nil
	}
	finish := func() error {
		if len(batch) > 0 {
			return accept(batch, cursor)
		}
		return nil
	}
	switch c.Adapter {
	case "arxiv-parquet":
		f, e := indexParquet(ctx, a, o)
		if e != nil {
			return e
		}
		for _, g := range f.RowGroups() {
			if cursor+g.NumRows() <= offset {
				cursor += g.NumRows()
				continue
			}
			r := parquet.NewGenericRowGroupReader[indexPaper](g)
			rows := make([]indexPaper, 128)
			for {
				n, e := r.Read(rows)
				for _, p := range rows[:n] {
					date := ""
					if p.Date != nil {
						date = p.Date.UTC().Format("2006-01-02")
					}
					if x := emit(Document{ID: p.ID, Source: "arxiv", Title: indexString(p.Title), Authors: indexString(p.Authors), Summary: indexString(p.Abstract), Categories: indexString(p.Categories), DOI: indexString(p.DOI), Published: date}); x != nil {
						r.Close()
						return x
					}
				}
				if e == io.EOF {
					break
				}
				if e != nil {
					r.Close()
					return errors.New("metadata Parquet row read failed")
				}
			}
			r.Close()
		}
	case "gutenberg-manifest":
		entries, e := gutenbergEntries(ctx, a, o)
		if e != nil {
			return e
		}
		for _, d := range entries {
			if e = emit(d); e != nil {
				return e
			}
		}
	case "jsonl":
		body, e := metadataObject(ctx, a, o)
		if e != nil {
			return e
		}
		defer body.Close()
		s := bufio.NewScanner(io.LimitReader(body, o.Bytes+1))
		s.Buffer(make([]byte, 65536), 1<<20)
		for s.Scan() {
			var raw struct {
				Document
				Abstract    string `json:"abstract"`
				Description string `json:"description"`
			}
			if e = json.Unmarshal(s.Bytes(), &raw); e != nil {
				return errors.New("metadata JSONL row is invalid")
			}
			d := raw.Document
			d.Source = c.Source
			if d.Summary == "" {
				if c.Source == "arxiv" {
					d.Summary = raw.Abstract
				} else {
					d.Summary = raw.Description
				}
			}
			if e = emit(d); e != nil {
				return e
			}
		}
		if e = s.Err(); e != nil {
			return e
		}
	default:
		return errors.New("unsupported metadata adapter")
	}
	if cursor != o.Rows {
		return errors.New("metadata row count changed during import")
	}
	return finish()
}

// Inventory-only adapters list saved objects without reading their bodies or
// interpreting rights. Counts are observations, not an immutable S3 snapshot.
func (m *indexManager) inventoryHolding(ctx context.Context, a *pdfArchive, c indexCollection, s *indexState, ci int) error {
	inv := &s.Inventory[ci]
	for !inv.ListingComplete {
		if e := m.checkControl(ctx, s.ID); e != nil {
			return e
		}
		q := url.Values{"list-type": {"2"}, "prefix": {c.Prefix}, "max-keys": {"1000"}}
		if inv.Cursor != "" {
			q.Set("continuation-token", inv.Cursor)
		}
		r, e := a.request(ctx, "GET", "", q, nil)
		if e != nil {
			return e
		}
		var listing struct {
			Objects   []pdfObject `xml:"Contents"`
			Truncated bool        `xml:"IsTruncated"`
			Next      string      `xml:"NextContinuationToken"`
		}
		e = xml.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&listing)
		r.Body.Close()
		if e != nil {
			return errors.New("saved-object listing is invalid")
		}
		for _, o := range listing.Objects {
			if !strings.HasPrefix(o.Key, c.Prefix) || o.Size < 0 {
				return errors.New("saved-object inventory scope changed")
			}
			inv.ObservedObjects++
			inv.ObservedBytes += o.Size
			if strings.Contains(o.Key, "metadata/") || strings.Contains(o.Key, "manifests/") || strings.HasSuffix(o.Key, ".jsonl") {
				inv.MetadataCandidates++
			}
		}
		if inv.ObservedObjects > 100000000 {
			return errors.New("inventory exceeds 100 million objects; split configured prefixes")
		}
		if listing.Truncated && (listing.Next == "" || listing.Next == inv.Cursor) {
			return errors.New("saved-object inventory cursor did not advance")
		}
		inv.Cursor = listing.Next
		inv.ListingComplete = !listing.Truncated
		if e = m.saveState(s); e != nil {
			return e
		}
	}
	return nil
}
