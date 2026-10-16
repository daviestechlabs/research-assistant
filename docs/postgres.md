# PostgreSQL catalogs

PostgreSQL stores catalog metadata, full-text indexes, source counters, and metadata import checkpoints.
The app connects through a fixed, operator-configured host on port 5432.
PgBouncer transaction pooling is supported.
Connections verify the server certificate and hostname against a mounted certificate authority (CA).
Passwords come from mounted Secret files, rather than URLs or browser requests.

SQLite remains available for local catalogs.
Saved sessions, usage records, and the arXiv download spool retain small SQLite databases on the existing volume.
Raw documents remain in object storage or the configured saved-PDF directory.
The existing vector service retains abstract embeddings.

## Prepare the database

Use a dedicated database and a non-superuser owner role.
The owner needs permission to create schemas, tables, indexes, functions, and triggers in that database.
The app also needs those permissions for validated activation and managed catalog retention.
Do not share this owner credential with unrelated applications.

CloudNativePG users can adapt [the generic Cluster and Pooler sample](../operator/samples/postgres.yaml).
The existing CNPG operator owns those resources.
Research Desk Operator references them and does not own their lifecycle.
Configure physical backups, recovery tests, monitoring, and storage capacity separately.

Create the database owner Secret through your secret manager.
Use the `kubernetes.io/basic-auth` type with `username` and `password` keys.
Mount the cluster CA Secret with its `ca.crt` key.
The pooler hostname must appear in the server certificate.

## Import existing generations

Start with an initialized Library on SQLite and its existing `active.json` receipt.
Keep the current application and worker on SQLite during import.
Do not change the embedding model.

Run the application image with the `migrate-postgres` command in a separate Kubernetes Job.
Mount the catalog PVC read-only, plus the database owner and CA Secrets.
The Job needs no chat key, object-storage key, or Kubernetes API token.
Supply these environment variables:

| Variable | Value |
|---|---|
| `RESEARCH_INDEX_ROOT` | Existing absolute Library directory, such as `/catalog/indexing` |
| `RESEARCH_POSTGRES_HOST` | Fixed pooler DNS name |
| `RESEARCH_POSTGRES_DATABASE` | Dedicated database name |
| `RESEARCH_POSTGRES_USERNAME_FILE` | Mounted username file |
| `RESEARCH_POSTGRES_PASSWORD_FILE` | Mounted password file |
| `RESEARCH_POSTGRES_CA_FILE` | Mounted CA certificate file |
| `RESEARCH_POSTGRES_WRITER` | `true` for migration and worker processes |

The command imports the active generation, its rollback generation, and validated or meaning-phase candidates.
It preserves row cursors, canonical IDs, metadata fields, content fingerprints, and vector collection identities.
COPY batches and their import cursors commit together.
Run the same command again after an interruption; completed batches remain saved.
A changed source checksum rejects the import.

Each generation receives its own schema and registry receipt.
The importer checks source checksums before and after copying.
It compares complete ordered metadata digests and independent record counts before sealing a generation.
Sealed metadata rejects inserts, updates, deletes, and truncation through database triggers.
Database administrators retain administrative control over these guards.

Wait for the Job to succeed.
Compare source counts, saved-document fingerprints, identifier lookups, topic queries, and saved-PDF links before adoption.
PostgreSQL uses English text normalization and `ts_rank_cd`; rankings can differ from SQLite BM25.
Paper topics still search supplied abstracts only.
The Books filter also searches catalog titles, authors, subjects, and descriptions.
Natural-phrase fallback retains source, category, language, and date filters.
It first tries the closest topic overlap, then relaxes toward a two-term minimum.
Each relaxation stays inside the full-text index query.

## Adopt the pool in stages

Configure the existing ResearchDesk:

```yaml
spec:
  storage:
    memoryMiB: 0
  postgres:
    enabled: true
    workerEnabled: false
    host: research-pg-pooler.research.svc.cluster.local
    database: research
    secretName: research-pg-owner
    usernameKey: username
    passwordKey: password
    caSecretName: research-pg-ca
    caKey: ca.crt
    namespace: research
    poolerName: research-pg-pooler
```

Set `spec.image.digest` to a tested image containing PostgreSQL support.
Keep `spec.indexing.imageDigest` pinned while adopting the app.
`workerEnabled: false` leaves that worker's database connection configuration unchanged.
The chart permits PostgreSQL traffic only to the named pooler's namespace and Pod label.
The Library view identifies its active metadata backend.

Verify application health and actual search behavior.
Then set the tested worker digest and `postgres.workerEnabled: true`.
Finish active metadata validation before restarting an older worker.
An already-imported meaning-phase candidate can resume from its saved vector row cursor.
Idempotent vector writes can replay the last uncommitted batch.
This transition does not queue a new indexing run.

New candidates copy their baseline inside PostgreSQL.
A small generation marker remains on the catalog volume alongside control and audit receipts.
Validated activation seals the database generation before switching the existing active pointer.
The previous generation remains available for rollback.
One-backup retention removes only obsolete owned generation schemas and their recorded catalog/vector artifacts.
Unfinished candidates and required parents remain protected.

Metadata generations and physical database backups serve different recovery purposes.
The one-previous-catalog policy does not configure CNPG backup retention.
Do not remove source documents, sessions, models, or download checkpoints during catalog cleanup.

## Test the adapter

The optional integration runner creates an isolated local PostgreSQL container with temporary credentials and verified TLS:

```sh
python3 scripts/test-postgres-local.py
```

It requires Docker, OpenSSL, Go, and SQLite development headers.
It uses loopback port 5432 and synthetic metadata only.
Tests cover import, native search, source accounting, candidate cloning, activation, rollback, retention, and vector-cursor resume.
Fake model and vector endpoints make no external inference calls.
