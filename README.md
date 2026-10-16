# Research Assistant

Search a library. Follow the evidence. Explore connections.

Research Assistant searches saved arXiv abstracts and Project Gutenberg catalog records.
It helps you learn from sources, compare findings, and propose experiments.
You can use the browser desk or the command line.
Research Desk Operator installs the desk through a Kubernetes `ResearchDesk` resource.

## Features

- Search abstracts with PostgreSQL full-text search or SQLite FTS5.
- Empty searches remove filler words, normalize I/O, and relax toward two topic terms.
- Optional reranking, question expansion, and semantic retrieval.
- Saved sessions, research notes, progress, cancellation, and durable jobs.
- Learn, Compare, and Connect ideas with source citations and evidence checks.
- Saved PDF reading with content checksums and physical-page citations.
- Knowledge graphs with reported connections and optional, clearly marked hypotheses.
- Phone layouts, a browser installation manifest, and JSON or SVG graph exports.
- OpenAI-compatible chat through LiteLLM, with a scoped service credential.
- Namespaced Helm reconciliation, a validated CRD, and external storage references.

Graphs do not establish scientific truth or novelty.
Answers distinguish source reports, interpretations, and missing evidence.
PDF reading uses selected passages, rather than complete-paper understanding.
The public package contains no corpus, PDFs, private inventory, credentials, or model weights.

## Run a local desk

Install Go 1.26.5, a C compiler, and SQLite development headers with FTS5 support.
On macOS, Homebrew SQLite works with the supplied Makefile.
On Debian, install `build-essential` and `libsqlite3-dev`.

```sh
make all
./research-assistant import --db /absolute/path/catalog.sqlite \
  --source arxiv --file /absolute/path/arxiv.jsonl
./research-assistant serve --db /absolute/path/catalog.sqlite \
  --listen 127.0.0.1:8094
```

Open `http://127.0.0.1:8094`.
Use `examples/synthetic-arxiv.jsonl` to preview the interface in a separate database.
Its two records are synthetic and contain no real scientific findings.
Import accepts JSON lines, Gutenberg CSV, and gzip files.
Paper records use the supplied `abstract` field.
Search does not read PDFs or call a model by default.
Broader results show their search terms and retain all filters.

```sh
./research-assistant search --db /absolute/path/catalog.sqlite --query geology
./research-assistant verify --db /absolute/path/catalog.sqlite
```

See [configuration](docs/configuration.md) for models, storage, authentication, and branding.
See [PostgreSQL migration](docs/postgres.md) for pooled catalogs and CloudNativePG.
See [Kubernetes installation](operator/README.md) for Research Desk Operator.

## Build an image

```sh
docker build --platform linux/amd64 -t YOUR_REGISTRY/research-assistant:0.1.0 .
docker push YOUR_REGISTRY/research-assistant:0.1.0
```

Use the resulting registry digest in `ResearchDesk.spec.image`.
The sample image digest is a placeholder, rather than a published image.
The PDF runtime packager currently supports Linux amd64.
The native application can run on macOS arm64.

## Development

```sh
make test check
python3 -m pip install -r operator/requirements-test.txt
python3 scripts/package-operator.py --check
python3 scripts/test-helm.py --helm helm
python3 scripts/check-public.py
```

The Go suite checks retrieval, citations, job persistence, authentication, and credential destination binding.
The Helm checks cover CRD parity, namespace permissions, external storage, and rejected configurations.
These checks do not establish corpus-wide accuracy or scientific validity.

## License

The application source uses the [MIT license](LICENSE).
Dependencies retain their own licenses.
See [third-party notices](THIRD_PARTY_NOTICES.md).
Your corpus needs its own access and redistribution authority.

Library controls provide inventory, candidate rebuilds, keyword and meaning indexes, validation, activation, and rollback.
An offline worker keeps durable progress on the catalog volume.
See [Library indexing](docs/indexing.md) for configuration and storage requirements.
