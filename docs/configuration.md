# Configuration

The application reads operator-supplied environment variables.
Browser requests cannot choose model endpoints, storage addresses, or object keys.

## Catalog storage

SQLite remains the default for a local desk.
Shared catalogs can use PostgreSQL through a transaction pooler.
See [PostgreSQL migration](postgres.md) for TLS, credential references, and staged adoption.

## Local inference

Use a dedicated LiteLLM virtual key for the research desk.
Configure its allowed model aliases and request limits in LiteLLM.
The supported research aliases are `smart-local` and `fast-local`.
These aliases can route to your self-hosted models.

```sh
export RESEARCH_LIMIT_OWNER=litellm
export RESEARCH_LITELLM_ORIGIN=http://127.0.0.1:4000
export RESEARCH_CHAT_URL=http://127.0.0.1:4000/v1/chat/completions
export RESEARCH_CHAT_MODEL=smart-local
# Set RESEARCH_MODEL_KEY from your secret store.
```

Gateway mode records usage without applying the application's daily allowance.
The application permits one active inference request per instance.
Set request and concurrency limits on the dedicated LiteLLM key.
One instance and a shared persistent desk database remain required.
The application cannot verify that your gateway enforces its configured limits.

Optional `RESEARCH_RERANK_URL` enables reranking.
The chat credential does not accompany reranker requests.
Semantic search requires a verified BGE-M3 index and its complete identity configuration.
Its variables are `RESEARCH_EMBEDDING_ORIGIN`, `RESEARCH_VECTOR_ORIGIN`, `RESEARCH_SEMANTIC_STATUS`, `RESEARCH_CATALOG_SHA256`, and `RESEARCH_EMBEDDING_SHA256`.
Leave all five unset to disable semantic search.
An incomplete configuration fails at startup.

## Saved paper PDFs

The reader expects an S3-compatible Parquet archive, rather than arbitrary loose PDFs.
The operator supplies these variables:

| Variable | Meaning |
|---|---|
| `RESEARCH_S3_ENDPOINT` | Fixed HTTP or HTTPS origin, without a path or URL credentials |
| `RESEARCH_S3_BUCKET` | Archive bucket |
| `RESEARCH_S3_PDF_PREFIX` | Relative prefix ending with `/` |
| `RESEARCH_S3_PDF_SHARDS` | Exact reviewed number of archive shards |
| `RESEARCH_S3_REGION` | Signing region; default `us-east-1` |
| `RESEARCH_S3_ACCESS_KEY` | Read-only access key from your secret store |
| `RESEARCH_S3_SECRET_KEY` | Read-only secret key from your secret store |

Shards use `train-00000-of-00001.parquet` naming with five-digit indexes and counts.
The reader validates the complete shard sequence and pins reads to object ETags.
Records contain the paper identifier, version, path, size, SHA-256 checksum, and binary content.
Two additional metadata columns retain the original archive layout.
Paper identifiers must be sorted across shard boundaries.

See `pdfMetadata` in `pdf.go` and the synthetic record in `pdf_test.go` for the supported layout.
The reader verifies content checksums before returning PDF bytes.

It uses bounded reads and does not fall back to an external PDF website.

## Private book manifests

The default `books.json` is empty.
An approved manifest requires `RESEARCH_BOOK_MANIFEST`, `RESEARCH_BOOK_PLAN_SHA256`, and `RESEARCH_BOOK_COUNT`.
The reviewed plan fingerprint and exact entry count must match the manifest.
The manifest declares retrieval authority and disables distribution.
Each entry binds an object key, content checksum, byte count, format, edition, and ruleset.

Use `research-private-book-library-v1` as the schema.
Book identifiers use the `dnd:` namespace.

PDF questions require saved-page evidence; catalog metadata cannot establish a rule.
Keep manifests outside the repository and mount them read-only.

## Authentication

Remote serving requires these variables:

| Variable | Meaning |
|---|---|
| `RESEARCH_PUBLIC_ORIGIN` | HTTPS desk origin |
| `RESEARCH_OIDC_ISSUER` | OIDC issuer |
| `RESEARCH_OIDC_AUDIENCE` | Expected token audience |
| `RESEARCH_OIDC_SUBJECTS` | Comma-separated allowed subjects |

Envoy Gateway OIDC configuration provides browser sign-in.
The application also verifies private API access.
The Helm chart configures these settings from `spec.auth` and optional `spec.gateway`.
An existing PVC holds catalog data, saved sessions, and usage records.
The operator never creates or owns that PVC.

## Branding

`RESEARCH_DESK_NAME` defaults to Research Desk.
`RESEARCH_DESK_DESCRIPTION` controls the About text.
`RESEARCH_DESK_ICON_ALT` controls the icon description.
`RESEARCH_PRIVATE_CONFIG_DIR` can provide a deployment-owned `icon.svg`.
The app escapes text before rendering markup.
The installation manifest uses the configured desk name.

The chart exposes these settings through `spec.branding`.
Its optional `configMapName` mounts `/private-config` read-only.
That ConfigMap can contain `icon.svg`, `books.json`, and `quality_generation_audit.json`.
The public package supplies no private audit approval.
Private deployments can configure `RESEARCH_GENERATION_AUDIT_MANIFEST` separately.
