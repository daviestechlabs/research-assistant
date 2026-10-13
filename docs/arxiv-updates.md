# New arXiv papers

The harvester checks arXiv's OAI-PMH feed for new papers and metadata revisions.
It saves abstracts, identifiers, version dates, licenses, and deletion receipts.
It downloads versioned PDFs into a separate directory on your library volume.
It does not change the serving catalog or activate a generation.
It uses no chat or embedding model.

## Start and resume

Choose an initial coverage date from your existing source snapshot.
A maximum publication date does not prove that every older paper exists.
Start before that date to include late announcements and revisions.
This feature catches updates after that boundary; it does not repair all historical gaps.

```sh
research-assistant arxiv-sync --directory /nas/library/arxiv-sync \
  --from 2026-01-01 --max-pages 500 --max-pdfs 5000 \
  --max-download-mib 16384
```

The first run requires `--from`.
Later runs use the saved checkpoint and ignore a changed initial date.
The harvester follows resumption tokens and keeps each page transaction durable.
An expired token restarts that harvest from its saved date.
Repeated records and saved versions do not require another PDF download.
A completed harvest retains a one-day overlap for later updates.

Requests use one connection with at least three seconds between requests.
The client honors bounded Retry-After delays and retries temporary server failures.
Only the fixed official metadata and PDF hosts receive requests.
Redirects are disabled.

Each invocation has page, file, byte, and time limits.
PDFs have a 64 MiB ceiling and require a PDF header, end marker, and checksum.
Files move into their final location before their saved receipt commits.
Interrupted downloads retain their pending state.
Temporary files from an interrupted process may need operator cleanup.
Unavailable and oversized PDFs remain explicit in the status receipt.
Metadata remains available when a PDF cannot download.
Old saved versions remain on the volume.

## Kubernetes schedule

Set `ResearchDesk.spec.arxivSync` to enable the operator-managed CronJob.
The job uses the existing library claim, an immutable image, and no credentials or API token.
Its policy permits DNS and public HTTPS; it permits no inbound connection.
The command contacts only arXiv.
The schedule uses its explicit time zone.

```yaml
arxivSync:
  enabled: true
  imageDigest: sha256:REPLACE_WITH_VERIFIED_DIGEST
  directory: arxiv-sync
  fromDate: "2026-01-01"
  schedule: "0 6 * * *"
  timeZone: Etc/UTC
  maxPages: 500
  maxPDFs: 5000
  maxDownloadMiB: 16384
```

The CronJob forbids overlapping jobs.
A filesystem lock also prevents competing harvesters.
Large catch-up downloads can need multiple scheduled runs.
Monitor `status.json`, NAS capacity, and failed Jobs.
Metadata and PDFs stay outside the public source repository.
The stored license describes each paper; personal storage does not grant redistribution or training rights.
See [arXiv's access terms](https://info.arxiv.org/help/api/tou.html) and [harvesting guide](https://info.arxiv.org/help/oa/index.html).

## Update the searchable catalog

Finish and activate your complete meaning catalog first.
Keep the old indexing worker pinned while a full rebuild runs.
After it finishes, deploy the updated worker and set `indexing.arxivUpdatesEnabled: true`.
This separate switch prevents a download configuration change from restarting the current worker.

Open **Library** and choose **Build candidate from arXiv updates**.
The application freezes one consistent update snapshot.
The worker copies the active catalog and applies those records and deletion receipts.
It preserves all other papers and sources.
Keyword search updates through the existing database triggers.
Matching fingerprints reuse saved vectors; changed records receive new abstract embeddings.
Copied vectors use bounded batches without the embedding rate delay.
Complete vector counting, integrity checks, and configured saved-PDF samples remain required.
The update never re-downloads the original archive or re-embeds unchanged papers.

Activation remains a separate administrator action.
The candidate must retain the same active parent identity.
Rollback retains the previous catalog and vectors.
New harvests after queueing enter a later candidate.
Sessions and usage remain on their existing volume.

Signed-in PDF reading first checks the saved update receipt and checksum.
Older papers retain their existing archive reader.
The browser receives no NAS path or credential.
Public search results do not grant public access to saved PDFs.
