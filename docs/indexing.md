# Library indexing

[New arXiv papers](arxiv-updates.md) describes incremental harvesting, saved PDFs, and update candidates.

Library controls manage an offline worker. The worker stores its queue and checkpoints on the catalog volume.
The browser sends configured collection IDs. It cannot send storage addresses, credentials, paths, or commands.
Only configured administrators can use these controls. Search and research retain their existing authentication rules.

## Five stages

1. Inventory configured catalogs or saved-object prefixes.
2. Build a separate catalog from supplied abstracts and book metadata.
3. Build keyword search and optional meaning search.
4. Check integrity, counts, queries, saved-link samples, and vector coverage.
5. Activate the candidate or restore the previous generation.

An inventory-only collection lists objects without reading their bodies. Its counts describe observed storage objects.
These counts do not establish the number of books or publications. S3 listings can change during a long inventory.
Potential metadata objects require review before import. Inventory does not activate private books or approve their rights.

Metadata adapters support JSONL, arXiv metadata Parquet, and Gutenberg manifests.
The Parquet adapter reads metadata columns through ETag-pinned ranges. It rejects schemas with corpus-body columns.
JSONL supports the existing arXiv and Gutenberg record formats. This release does not extract abstracts from PDF bodies.

## Configure the worker

Enable `ResearchDesk.spec.indexing`. Set `configMapName` to an external ConfigMap with an `indexing.json` entry.
The operator mounts this file in the application and worker. It does not publish or manage the private configuration.

```json
{
  "admin_subjects": ["replace-with-your-oidc-subject"],
  "max_objects": 1000,
  "max_catalog_mib": 16384,
  "documents_per_second": 5,
  "collections": [
    {
      "id": "papers",
      "label": "Saved paper abstracts",
      "source": "arxiv",
      "adapter": "jsonl",
      "endpoint": "https://storage.example.test",
      "bucket": "research",
      "prefix": "metadata/papers/",
      "access_env": "RESEARCH_INDEX_STORAGE_ACCESS",
      "secret_env": "RESEARCH_INDEX_STORAGE_SECRET"
    },
    {
      "id": "holdings",
      "label": "Saved objects",
      "source": "inventory",
      "adapter": "inventory",
      "endpoint": "https://storage.example.test",
      "bucket": "research",
      "prefix": "",
      "access_env": "RESEARCH_INDEX_STORAGE_ACCESS",
      "secret_env": "RESEARCH_INDEX_STORAGE_SECRET"
    }
  ]
}
```

Use `key` for one metadata object. Use `prefix` to discover compatible metadata objects.
An empty prefix is allowed only for inventory-only collections. Discovery remains inside the configured bucket.

The worker accepts at most 32 collections. Metadata discovery accepts at most 10,000 objects per collection.
An inventory-only run accepts at most 100 million objects. Split larger holdings into configured prefixes.

Map each credential variable through `indexing.credentialEnv`:

```yaml
indexing:
  enabled: true
  configMapName: private-indexing
  directory: indexing
  credentialEnv:
    - name: RESEARCH_INDEX_STORAGE_ACCESS
      secretName: storage-reader
      key: access-key
    - name: RESEARCH_INDEX_STORAGE_SECRET
      secretName: storage-reader
      key: secret-key
  nasCIDRs: ["203.0.113.8/32"]
  nasPorts: [443]
```

Use a storage account that permits the required listing, HEAD, and GET operations.
The worker does not perform source writes. It receives no chat key or Kubernetes API token.
Its network policy permits configured storage addresses, DNS, the embedding service, and the vector service.

Set `trustConfigMapName` for a private certificate authority. Supply its trust bundle as `ca.crt`.
Set worker node placement and resource bounds through `indexing.nodeSelector` and `indexing.resources`.

## Meaning search

Set `embedding_origin`, `vector_origin`, and `model_sha256` in the external configuration.
The supported contract is normalized BGE-M3 with 1,024 dimensions, CLS pooling, and a static 128-token engine.
Each supplied abstract uses complete text windows. The worker rejects truncation and invalid vectors.
Qwen and the chat gateway do not participate in indexing.

Unchanged vectors can come from the active generation. Reuse requires matching model, algorithm, snapshot identity, document ID, and content fingerprint.
Missing or changed records receive new embeddings. The worker writes a separate candidate collection.
A complete index requires an independent vector count. A partial index cannot become a validated meaning candidate.

## Run and switch

Open **Library** while signed in as an approved administrator. Select collections and choose **Inventory collections** or **Build replacement**.
Inventory-only collections support the inventory action. Clear them before building a replacement.
A rebuild replaces each selected source. Select every configured metadata collection for that source.

Other sources retain their existing metadata and file references. Approved private books retain their separate manifest and access checks.

Pause stops at a durable batch checkpoint. Resume continues that run with the same configuration and source ETags.
A changed source snapshot or configuration requires a new run. Cancel releases the run without deleting its receipts or candidate.
Only one unfinished run can exist. The worker holds a filesystem lock to prevent competing workers.

Validation checks SQLite, the keyword index, actual source counts, source accounting, ID queries, and an abstract keyword query.
Conflicting records with the same ID fail the run. Identical duplicates receive one catalog entry and a duplicate count.
The receipt reports added, removed, and changed IDs against the previous catalog.

The receipt reports duplicate DOIs. These can represent separate editions or records; the count does not establish semantic duplication.

Configure `link_samples` with at most eight saved PDF IDs. Their checksum checks do not establish availability of every saved file.
Metadata manifests can describe planned downloads. Their records do not prove those downloads completed.

Choose **Activate this catalog** after validation. Active research jobs must finish or be cancelled first.
The application drains requests and reloads under its process supervisor. Kubernetes restarts the container automatically.
A standalone deployment also needs a supervisor. Startup verifies the catalog checksum before opening the selected generation.

Sessions and usage remain beside the original catalog. They do not move into candidate directories.
A candidate from an older active generation cannot activate.
Rebuild it from the current catalog first.

Activation keeps the previous pointer and files. **Restore previous catalog** verifies their checksum before switching back.
A keyword-only activation disables meaning search. A later verified meaning generation or rollback restores it.

## Storage and maintenance

Use NAS storage with reliable filesystem locks and atomic rename. SQLite uses durable rollback journals rather than WAL.
Reserve space for the active catalog, a candidate, retained generations, and temporary snapshot and journal files.
The metadata database ceiling does not limit all retained storage. Monitor actual storage capacity.

Large inventories and indexes can run for hours or days. Closing a browser does not stop their worker.

The Library view displays a worker heartbeat and saved progress. A missing heartbeat identifies an unavailable worker.
A failed run requires an explicit resume request. Automatic restart does not replay a failed source contract.

History currently retains at most 100 runs. An operator must archive finished run directories when retention needs maintenance.
Do not delete the active generation, previous generation, or session and usage databases.

`research-assistant check-indexing` reads bounded metadata samples and embeds one abstract when configured.
It does not write source objects or vectors. Use it to verify an adapter before a large run.

## Keep one rollback catalog

Enable `ResearchDesk.spec.indexing.retention.enabled` to keep the live catalog and one previous catalog.
Cleanup runs every minute after the application opens the active generation.
It retires older managed catalog files, frozen update snapshots, and their exact vector collections.
Unfinished runs and their parent catalogs stay protected.
Small run receipts remain for audit and restart recovery.
Library shows the policy and any cleanup error.

The original imported catalog stays protected by default.
Set `indexing.retention.pruneOriginal: true` to permit its cleanup after it leaves both protected slots.
This option also permits retiring its recorded semantic collection.
Use it only when this desk owns the original catalog and semantic receipt.
The policy preserves saved PDFs, books, models, sessions, and source archives.
A new candidate temporarily uses additional storage while it builds and validates.

Standalone deployments use `RESEARCH_INDEX_RETENTION=true` and optionally `RESEARCH_INDEX_RETENTION_PRUNE_ORIGINAL=true`.
Retention uses the existing indexing lock and makes no model calls.
A changed checksum, linked path, or changed ownership record stops that cleanup pass.
Vector deletion failure preserves the retired catalog file for retry.

## Observe validation work

Library shows separate SQLite integrity, full-text integrity, count, catalog-diff, checksum, and saved-PDF validation stages.
During database queries, the worker samples SQLite execution callbacks every ten seconds.
One work tick represents a callback interval of 10,000 SQLite virtual-machine instructions.
Ticks show observed database execution, not processed rows or a completion percentage.
The last-advance timestamp changes when ticks increase or the validation stage changes.
The receipt timestamp shows the observer heartbeat separately.
Checksum and PDF stages do not increment database work ticks.
A blocked storage read can delay both a query and its next execution callback.

Older workers expose their existing heartbeat and saved batch checkpoint only.
Their Library cards state that detailed progress is unavailable.
Pin the worker image during an active validation pass.
Upgrade after the run finishes to avoid repeating that pass.
The observer does not change the catalog, skip validation, or call an embedding model.

## Choose metadata storage

Row count alone does not determine when a catalog needs a server database.
SQLite fits an embedded reader and portable catalog snapshots.
This application can load a bounded catalog image for serving.
The offline validator still scans its on-disk candidate and baseline.
Network-backed files can make those scans expensive.

For a shared deployment, assess PostgreSQL for metadata and concurrent updates.
Retain the vector service for embeddings and object storage for saved documents.
A migration needs an import check, source-count reconciliation, retrieval comparisons, and a rollback path.
Keep the current store until those checks pass.
Changing metadata storage does not inherently require changing the embedding model.
