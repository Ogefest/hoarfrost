# hoarfrost

[![ci](https://github.com/Ogefest/hoarfrost/actions/workflows/ci.yml/badge.svg)](https://github.com/Ogefest/hoarfrost/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

**hoarfrost** is a self-hosted web archiving system for preserving the
internet's most short-lived content: news articles and individual web pages
that are edited, moved or deleted days after publication. It follows RSS
feeds and sitemaps, captures every new page with all its resources, and keeps
it in standard web archive formats (WARC, CDXJ, WACZ) on a local disk or any
S3-compatible storage, where it can be searched, read, exported and replayed
years later.

The archive outlives the application. Everything needed to read it lives in
storage in formats that `warcio`, pywb and ReplayWeb.page understand, together
with a `README.md` describing the layout. The index in PostgreSQL can be
dropped and rebuilt from storage at any time, and the test suite proves the
archive can be read with standard tools alone.

```
 RSS / sitemaps / URLs / browser uploads
                  │
                  ▼
   hoarfrost serve ── leases jobs ──► hoarfrost worker(s)   (optional)
   (UI, API, queue,                    capture pages into WARC
    embedded worker)                            │
         │                                      ▼
    PostgreSQL ◄── index ── storage: incoming/*.warc.gz ── weekly ──► weekly/*.wacz
```

Captured pages are deduplicated within ISO weeks and sealed every week into a
self-contained, verified WACZ package. Other applications read the archive
through a read-only [HTTP API](docs/api.md) ([OpenAPI](docs/openapi.yaml)).

## Features

- **One binary**, two roles: `hoarfrost serve` (coordinator, web UI, API,
  embedded worker) and `hoarfrost worker` (additional capture nodes).
- **Sources**: RSS/Atom feeds, sitemaps (including news sitemaps and
  indexes), static URL lists, URLs added through the API, and pages saved in
  a browser (MHTML, SingleFile HTML, WACZ/WARC). Sources are added and edited
  in the web UI (or the API), each with its own poll interval, engine and
  storage; the last poll and the next one are shown per source.
- **Capture engines** sharing one WARC recorder that keeps raw HTTP bytes:
  - `static` – server-rendered pages: HTML and CSS parsing, `<base>`,
    `srcset`/`<picture>`, lazy-loading attributes, `<noscript>`, CSS `url()`
    and `@import`, iframes, blocked domains, size limits;
  - `chrome` – headless Chromium; every browser request goes through the
    recorder; the rendered DOM is stored as well;
  - `http` – single files (PDF, DOCX, XLSX, ODT) discovered in articles;
  - automatic **fallback** to another engine when the extracted text is too
    short, with per-source statistics in the UI.
- **Block lists**: public Pi-hole/adblock lists (hosts files, domain lists,
  `||domain^` rules) refreshed periodically filter ads and trackers out of
  every capture; the list version is recorded in each WARC file.
- **Deduplication** within ISO-week epochs (`revisit` records with
  `WARC-Refers-To`); a revisit never points to a record that is not stored.
- **Weekly packages**: WACZ 1.1.1 (stored ZIP, ZIP64), sorted CDXJ (or a
  ZipNum block index), `pages.jsonl` with article text, SHA-256 fixity in
  `datapackage.json` and a `.sha256` file; records are readable with a
  single range request.
- **Verification** of every package (hashes, digests, revisit targets,
  stored entries) after sealing and on demand.
- **Structured data**: meta description and schema.org objects (JSON-LD,
  microdata, RDFa) of every page, kept per URL in the database and per week
  in a Parquet page table next to the package, sorted by URL.
- **Full-text search** (PostgreSQL, diacritics-insensitive), timeline of
  captures per URL, quick preview (single HTML with `data:` URIs, sandboxed),
  HTML export, raw WARC record download, full replay with ReplayWeb.page.
- **Read API** for other applications: search, filters, an incremental
  feed of new captures, metadata and text, captured files; read-only API
  keys managed by the admin ([docs/api.md](docs/api.md)).
- **Storage** backends: filesystem and S3 (range reads with a block cache),
  optional asynchronous replica; **several named storages** (buckets or
  directories), chosen per source or per added URL, each one a complete,
  independent archive.
- **Users** with roles (viewer, editor, admin), argon2id passwords with a
  server-side pepper, API tokens, Prometheus `/metrics`.
- **Import** of an existing archive of single-file HTML pages (`monolith`).
- On-demand **OCR** (tesseract) for scanned PDFs; PDF text via `pdftotext`.

## Quick start

Requirements: Go 1.26 or newer and a PostgreSQL database (CI tests on
PostgreSQL 17).

```sh
go build -o hoarfrost ./cmd/hoarfrost      # or: make build
cp .env.example .env                       # set HOARFROST_DATABASE and HOARFROST_SECRET (32+ random chars)
./hoarfrost serve
```

Open <http://localhost:8080>, create the administrator and add sources on
the Sources page (or through `/api/sources`). The instance starts without
any; sources, their schedule and poll state are kept in the database.
Optional external tools: Chromium (`chrome` engine), `pdftotext` (PDF text),
`tesseract` + `pdftoppm` (OCR).

One `hoarfrost serve` is a complete instance: it captures pages with its
embedded worker. When it is not fast enough, add `hoarfrost worker` processes
on the same or other machines; see [Running](docs/operations.md#running).

With Docker, a complete environment with PostgreSQL, a test news site, a
coordinator and an extra worker; the archive is written to `./archive` on the
host:

```sh
docker compose up --build
```

## Commands

| Command | Purpose |
|---|---|
| `hoarfrost serve` | coordinator: UI, API, scheduler, sealing, embedded worker |
| `hoarfrost worker -coordinator URL` | additional stateless worker (needs `HOARFROST_WORKER_TOKEN` and storage access) |
| `hoarfrost user add\|list\|delete\|role\|reset-password\|token` | account management, also without the UI |
| `hoarfrost seal -epoch 2026-W41 [-force]` | package a week now |
| `hoarfrost verify [-epoch …]` | verify sealed packages (exit status 1 on problems) |
| `hoarfrost reindex [-epoch …] [-force]` | rebuild the index from storage |
| `hoarfrost export -page ID \| -url URL [-at TIME] -o file.html` | single-file HTML export |
| `hoarfrost import-legacy DIR` | import single-file HTML pages as WARC `resource` records |
| `hoarfrost cleanup` | delete packaged `incoming/` files after the retention period |

## Storage layout

```
<root>/
  README.md                                   layout and formats, written by hoarfrost
  config/sources/2026-10-11T120000Z.yaml      history of capture configuration
  incoming/2026/W41/<ts>-<source>-<id>.warc.gz   one file per capture (temporary)
  weekly/2026/W41.wacz                        weekly package (permanent)
  weekly/2026/W41.wacz.sha256
  weekly/2026/W41.parquet                     page table: URL, title, description, schema.org
  catalog/2026/W41.json                       package status and verification result
```

Database (`HOARFROST_DATABASE`, PostgreSQL): schema `archive_index` (derived,
rebuildable from storage) and schema `app` (users, sessions, tokens, queue,
sources – back it up). Local data (`HOARFROST_DATA_DIR`): `spool/`, block lists.

## Documentation

| Document | Contents |
|---|---|
| [docs/architecture.md](docs/architecture.md) | goals, principles, components, epochs, deduplication, sealing, indexing, testing strategy |
| [docs/operations.md](docs/operations.md) | running a single process or adding workers, configuration, storages, backups, recovery, monitoring |
| [docs/formats.md](docs/formats.md) | archive formats and how to read the archive without hoarfrost |
| [docs/api.md](docs/api.md) | HTTP API: read API keys, following new captures, sources, uploads |
| [docs/openapi.yaml](docs/openapi.yaml) | OpenAPI 3.1 specification of the read API |
| [docs/decisions.md](docs/decisions.md) | design decisions and their reasons |
| [extension/](extension/) | minimal browser extension for saving the current tab |
| [CONTRIBUTING.md](CONTRIBUTING.md) | rules and workflow for contributors |
| [SECURITY.md](SECURITY.md) | reporting vulnerabilities |

## Development

Database tests are skipped unless `HOARFROST_TEST_DATABASE` points to a
PostgreSQL database; a disposable one is enough:

```sh
docker run -d --name hoarfrost-test-pg -p 127.0.0.1:55432:5432 \
  -e POSTGRES_USER=hoarfrost -e POSTGRES_PASSWORD=hoarfrost -e POSTGRES_DB=hoarfrost_test postgres:17
export HOARFROST_TEST_DATABASE='postgres://hoarfrost:hoarfrost@127.0.0.1:55432/hoarfrost_test?sslmode=disable'

make test          # unit and integration tests
make race          # with the race detector
make fuzz          # short fuzzing of all parsers
make e2e           # end-to-end test of the binary
make conformance-tools && make e2e   # adds warcio, py-wacz and pywb checks
```

Set `HOARFROST_TEST_S3=s3://bucket/prefix?endpoint=http://host:port&region=us-east-1&path_style=true`
(with `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`) to run the storage contract
tests and the end-to-end test against a real S3-compatible server.
See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

Apache License 2.0. ReplayWeb.page (AGPL-3.0) is not bundled; the replay
page loads it from a CDN.
Bundled web assets: htmx 2.0.4 (0BSD), Inter font (SIL OFL 1.1,
`internal/web/static/fonts/OFL.txt`), Lucide icons (ISC).
