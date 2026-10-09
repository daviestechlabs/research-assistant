package main

import (
	"context"
	"errors"
	"os"
	"time"
)

// An operator smoke check reads at most the first metadata batch per configured
// catalog, embeds one abstract, and makes no vector writes or chat calls.
func checkIndexing() error {
	c, _, e := loadIndexConfig(os.Getenv("RESEARCH_INDEX_CONFIG"))
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	stop := errors.New("bounded metadata probe complete")
	results := []map[string]any{}
	embedded := false
	for _, collection := range c.Collections {
		if collection.Adapter == "inventory" {
			continue
		}
		a, e := collectionStore(collection)
		if e != nil {
			return errors.New("indexing probe storage credentials are unavailable")
		}
		objects, e := discoverIndexObjects(ctx, a, collection, c.MaxObjects)
		if e != nil {
			return errors.New("indexing probe metadata discovery failed")
		}
		o := objects[0]
		o.Rows, e = indexObjectRows(ctx, a, collection, o)
		if e != nil {
			return errors.New("indexing probe metadata footer or manifest failed")
		}
		count := 0
		e = streamIndexObject(ctx, a, collection, o, 0, func(batch []Document, _ int64) error {
			for _, raw := range batch {
				d, e := normalize(raw)
				if e != nil {
					return errors.New("indexing probe metadata conversion failed")
				}
				count++
				if !embedded && c.ModelSHA != "" && d.Source == "arxiv" {
					s := &semanticClient{embeddingOrigin: c.EmbeddingOrigin, client: a.client}
					if e = s.health(ctx); e != nil {
						return errors.New("indexing probe embedding identity failed")
					}
					if _, e = indexEmbedAbstract(ctx, s, d.Summary); e != nil {
						return errors.New("indexing probe complete abstract embedding failed")
					}
					embedded = true
				}
			}
			return stop
		})
		if e != nil && !errors.Is(e, stop) {
			return errors.New("indexing probe metadata projection failed")
		}
		results = append(results, map[string]any{"collection": collection.ID, "metadata_rows": o.Rows, "sample_rows_checked": count, "etag_pinned": true})
	}
	return printJSON(map[string]any{"collections": results, "one_abstract_embedding_verified": embedded, "chat_model_calls": 0, "vector_writes": 0, "source_writes": 0})
}
