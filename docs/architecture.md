# Architecture

This document explains what hoarfrost is for, the rules its design follows,
and how the pieces fit together. [operations.md](operations.md) covers
deployment and configuration, [formats.md](formats.md) the archive formats in
detail, [api.md](api.md) the HTTP API, and [decisions.md](decisions.md) the
reasons behind individual design choices.

## Goals

hoarfrost is a long-term (10+ years) web archive for press articles and
individual web pages, which appear on news sites and then change or
disappear. It is meant for browsing, searching and sharing single articles,
for example for civic oversight. The archive as a whole is never published.

1. **Durable formats.** The data must stay readable in 10–20 years with
   standard tools and without this application.
2. **Space efficiency.** Disks cost money. Deduplication and sensible
   compression are requirements, not optimizations.
3. **Random access.** A single record, article or resource can be read
   without downloading the whole archive, also from S3 with HTTP range
   requests.
4. **Simplicity.** One binary, a storage location and a PostgreSQL database.
   No Kubernetes, no message broker.
5. **Scale.** Several TB of data, millions of URLs, optionally many workers.
6. **The application is replaceable.** The application captures, packages
   and presents. The archive is a separate layer that outlives it.

Out of scope: legal-grade evidence (qualified timestamps, signatures; hashes
are for fixity only, though nothing prevents adding signatures later),
re-checking pages for changes (each URL is captured once), deep crawling of
whole sites, and public access to the archive.

## Principles

These rules settle design questions. Code that breaks one of them is wrong.

1. **Storage is the only source of truth.** Everything in the database is
   either derived from storage or operational state.
2. **Objects are immutable.** An object written to storage is never modified;
   new data means new objects. The only exceptions are the storage's
   `README.md` and the `catalog/*.json` status files.
3. **The index is rebuildable.** The `archive_index` schema can be dropped and
   rebuilt from storage without losing information.
4. **Standard formats only.** WARC, CDXJ, WACZ, JSON Lines, YAML, ZIP, gzip,
   Parquet. Custom metadata goes only into extension points the standards
   provide: `warcinfo`, `metadata` records, additional JSON fields.
5. **Every layer behind an interface.** Storage, capture engines, the
   recorder and the index can be replaced independently.
6. **Self-describing storage.** The archive root contains a `README.md`
   describing the layout and formats.
7. **Secrets never enter the archive.** Users, passwords, tokens and `.env`
   stay local; `Cookie`, `Authorization` and `Set-Cookie` headers are removed
   from records.

## Components

```
   sources (RSS, sitemaps, URL lists) · API · browser uploads
                         │
                         ▼
   ┌───────────────────────────────┐   lease / dedup / complete   ┌─────────────────┐
   │ coordinator  (hoarfrost serve)│◄────────────────────────────►│ worker(s)       │
   │ queue, schedule, epochs,      │        /internal/*           │ engines +       │
   │ dedup, index, UI, API, seal   │                              │ WARC recorder   │
   │ (+ embedded worker)           │                              └────────┬────────┘
   └───────┬───────────────┬───────┘                                       │ incoming/…
           │ SQL           │ weekly/…, range reads                          ▼
           ▼               └──────────────────────────────►  STORAGE (directory | S3)
      PostgreSQL
```

One binary has two roles:

- **Coordinator** (`hoarfrost serve`), exactly one per instance. It is the
  only process that talks to the database. It polls sources and queues new
  URLs, leases jobs to workers, answers deduplication lookups, accepts
  completed captures and indexes them, seals weeks into WACZ packages and
  verifies them, and serves the web UI, the API and archive reads. By default
  it also runs an embedded worker, so one process is a complete instance.
- **Worker** (`hoarfrost worker`), zero or more. Stateless: it can join or
  leave at any time. Its loop is lease → capture with an engine → upload the
  WARC file to `incoming/` → report completion. It gets the epoch and the
  deduplication decisions from the coordinator and needs only the
  coordinator URL, the worker token and write access to storage.

### Queue and leases

Jobs live in the `app.jobs` table. A worker asks for work with the engines
and storages it supports; the coordinator leases jobs in one transaction
(`FOR UPDATE SKIP LOCKED`), assigning the epoch at lease time.

- A lease lasts `HOARFROST_LEASE` (10 minutes by default). An expired lease
  returns the job to the queue, so a crashed worker loses nothing.
- Order: jobs of crashed workers first, then the lowest priority number.
  Within a priority, sources take turns: the source with the fewest running
  jobs (then the one served least recently) gets its oldest job. A sitemap
  bringing a hundred thousand old articles does not delay today's articles
  from a feed; it is worked off with the slots the other sources leave.
- Politeness: at most *N* simultaneous leases per domain (`max_per_domain`,
  default 2). The limit is part of the lease, so it holds across all
  workers.
- Failed jobs are retried with a backoff, up to three attempts.
- If two workers complete the same job (a late report after the lease
  expired), the first completion wins; the second capture is kept as an
  extra capture.
- A week is sealed only when no lease of that week is active and the grace
  period has passed; a completion arriving after sealing is rejected and the
  job is queued again.

### Two levels of deduplication

| Level | Where | What |
|---|---|---|
| URL | coordinator, when a source is polled | A URL from a feed or sitemap is queued only if it was not seen before in that storage and is not already waiting. Before the check, fragments are dropped and URLs on a host alias are rewritten to the host the site lives on (see below). URLs added through the API or UI skip the filter. |
| Content | recorder, for every response | The payload digest is looked up in the current epoch; a hit is written as a `revisit` record, a miss as a full `response`. |

Sites often list one article under several hosts (`example.com`,
`m.example.com`, `www.example.com`), and a sitemap may switch between them
from one poll to the next. When the coordinator meets a new origin (scheme
and host) in a feed or sitemap, it requests one of its URLs without
following redirects. A permanent redirect (301 or 308) to the same path and
query on another origin makes the origin an alias of that one; the result is
kept in `app.host_aliases` and checked again after 30 days. At most 20
origins are probed per poll.

```
for every response:
  digest = sha1(payload)
  hit = local map of this capture, else coordinator.Lookup(storage, epoch, digest)
  if hit == nil: write response (full payload)
  else:          write revisit with WARC-Refers-To = hit
```

Lookups only return records that are already committed to the index, so a
revisit never points to a record that does not exist. Two workers fetching
the same new resource at the same moment both store a full copy, which is
correct, only slightly wasteful.

## Epochs

- An epoch is an **ISO week** (`2026-W41`) of one storage.
- Deduplication works only inside an epoch: a revisit can only point to a
  record of the same week. Each week is therefore self-contained: its
  volumes can be moved, verified, replayed or deleted together,
  independently of other weeks. A revisit may point into another volume of
  its week, so tools load a week as a whole (pywb: one collection of the
  WARC files of all volumes; ReplayWeb.page: the week's `collection.json`).
- A capture belongs to the epoch in which its job was **leased**, not to the
  timestamps of its records: a page started on Sunday at 23:59 belongs
  entirely to the ending week.

The cost is that a shared resource (CSS, font, logo) is stored in full once a
week, which is negligible compared to thousands of captures.

### Data lifecycle

1. **Capture.** One page with all its resources becomes one WARC file in
   `incoming/<year>/W<week>/`, with a CDXJ index next to it. The unit of
   writing is the page, not the resource: roughly a thousand objects a day,
   not tens of thousands.
2. **Seal.** After the end of the week and the grace period
   (`HOARFROST_SEAL_GRACE`), the week's files are packed into WACZ volumes
   in `weekly/<year>/W<week>/`.
3. **Cleanup.** `incoming/` files are deleted only after the week has been
   verified, and only after the retention period (`HOARFROST_INCOMING_RETENTION`).

### Sealing

1. List `incoming/<year>/W<week>/` and compare it with the index.
2. Split the capture files, in package order, into **volumes** of at most
   `HOARFROST_VOLUME_SIZE` bytes (default 4 GiB; a capture file is never
   split, a larger one gets a volume of its own). For each volume:
   - build the WACZ (ZIP with every entry stored, ZIP64 when needed) in the
     spool directory. Every capture file is read once; its bytes are copied
     into the volume and scanned for the index on the way. A volume holds:
     - `archive/*.warc.gz` – the capture files, optionally concatenated per
       source (`<source>-<n>.warc.gz`: concatenated gzip members are a valid
       `.warc.gz`, and names are unique across the volumes of the week);
     - `indexes/index.cdxj` – sorted; large volumes use a ZipNum block index;
     - `pages/pages.jsonl` – URL, title, time, source and extracted **text**;
     - `datapackage.json` (SHA-256 of every entry) and `datapackage-digest.json`;
   - store it as `weekly/<year>/W<week>/<year>-W<week>-<n>.wacz` with **one
     PUT request**: the server writes it as it arrives, with no multipart
     upload to assemble (servers limit the parts of an upload, or take
     minutes to complete a large one and may report success to a retried
     request before it is done);
   - store its **captures list** (`<year>-W<week>-<n>.captures.jsonl`):
     every capture file with its absolute offset in the volume and the
     SHA-256 of its bytes. The list marks the volume complete;
   - remove the local file.

   Index and page lines are sorted in bounded memory through temporary
   files in the spool directory. Local disk holds one volume at a time and
   memory does not grow with the size of the week: a terabyte week needs the
   same machine as a gigabyte one.
3. Move the index to the volumes: the records and pages of a capture file
   keep their offsets, shifted by where the file starts in its volume.
4. Write `SHA256SUMS` and `collection.json` (a multi-WACZ collection) of the
   volumes, the Parquet page table and `catalog/<year>/W<week>.json`, then
   verify the stored volumes (below) and record the result in the catalog.

A seal interrupted while storing volumes keeps the volumes stored with
their captures lists and continues with the next one; anything else in the
week's directory (a volume stored without its list) is removed first, as
the index points into volumes only after all are stored. Every later step
can be repeated. `incoming/` files are removed only after verification, so
nothing is lost by starting over.

Weeks sealed before volumes keep their single package
`weekly/<year>/W<week>.wacz` with `W<week>.wacz.sha256` and
`W<week>.captures.jsonl`; they are read, verified and reindexed like a week
of one volume.

### Verification

Verification reads every stored volume once from start to end and proves
that nothing was lost between `incoming/` and the volumes:

- every volume matches its SHA-256 (catalog and `SHA256SUMS`), and every
  entry matches `datapackage.json`; `SHA256SUMS` and `collection.json` list
  exactly the volumes of the catalog, and nothing else is in the week's
  directory;
- every capture file of the captures lists is in its volume byte for byte
  (its SHA-256 was taken while it was read from `incoming/`), no capture is
  in two volumes, and every capture the index knows of the week is listed;
- every WARC record is readable and its block and payload digests are valid;
- every indexed record of the week is in a volume exactly where the index
  says, and no indexable record of a volume is missing from the index; every
  revisit points to a record of the week with the same digest;
- every CDXJ and `pages.jsonl` line points at its record.

Records are compared with the index in batches, so memory does not grow
with the number of records.

### Seal attempts

Every attempt is recorded in the application database (`seal_runs`) with
the instance that made it, its phase, progress and a heartbeat. An attempt
whose heartbeat stops (the process ran out of memory, crashed or was
restarted) is marked *interrupted* and its week *failed*. Failed weeks are
sealed again automatically (3 attempts, an hour apart) and are flagged on the
dashboard, under System and by the `HoarfrostSealFailed` alert until a seal
succeeds.

After sealing, reading a record is **one range request**: volume key,
absolute offset, length, gunzip.

## Recorder and engines

Engines differ only in how they fetch. **Writing is shared**: every engine
writes through the same recorder.

| Engine | Use | How |
|---|---|---|
| `static` | default for server-rendered news sites | Parses HTML and CSS, resolves resources and fetches them without running JavaScript. |
| `chrome` | sites that need JavaScript | Headless Chromium (go-rod). Every browser request is intercepted over the DevTools protocol and fetched by the recorder; the rendered DOM is stored as well. |
| `http` | PDFs and other single files | One request. |

The `static` engine handles `<base href>`, `src`, `srcset` (one variant, not
all), `<picture><source>`, `poster`, `<link rel=stylesheet|icon|preload>`,
lazy-loading attributes (`data-src`, `data-srcset`, `data-lazy-src`),
`<noscript>`, CSS `url()` and `@import` (recursively, also in `<style>` and
`style` attributes), iframes, blocked domains and size limits. Links to PDF,
DOCX, XLSX or ODT files become new `http` jobs.

**Fallback.** After a capture the text extractor measures the article text.
Below a per-source threshold the job is repeated with the fallback engine
(usually `chrome`). Fallback rates per source are shown in the UI and suggest
changing a source's default engine.

**Browser uploads** (MHTML, SingleFile HTML, WACZ/WARC from ArchiveWeb.page)
do not go through an engine: they are written as `resource` records (WACZ
contents are rewritten into a new WARC file with sensitive headers removed
and digests recomputed) in the current week.

The recorder:

- records the **raw bytes** of every HTTP exchange (one connection per
  request, HTTP/1.1, no transparent decompression or header normalization);
- computes `WARC-Payload-Digest` and `WARC-Block-Digest`;
- consults deduplication and writes `response` or `revisit`;
- removes sensitive headers from `request` records;
- starts every file with a `warcinfo` record (software version, engine,
  worker, epoch, source, block list version) and adds a `metadata` record per
  page (engine, fallback, timings, extracted text, title, publication date,
  description, authors, keywords, and the page's structured data: schema.org
  objects, Open Graph and Twitter Card properties, microformats2 items);
- on close produces the file, its CDXJ lines and the list of records for the
  index.

A WARC file is written to the local `spool/` directory and uploaded only
when complete, so storage only ever sees finished, immutable objects.

### Fetching from the network or from the archive

Page traversal is separated from where the bytes come from. The same code
that captures a page from the network can assemble it from the archive (with
revisits resolved). The **HTML export** and the quick preview use it to build
one HTML file with all resources inlined as `data:` URIs, without storing a
second copy.

## Storage

Storage holds only finished, immutable objects:

```go
type Storage interface {
    Put(ctx context.Context, key string, r io.Reader, size int64) error
    Open(ctx context.Context, key string) (io.ReaderAt, int64, error)
    List(ctx context.Context, prefix string) ([]Object, error)
    Stat(ctx context.Context, key string) (Object, error)
    Delete(ctx context.Context, key string) error // only for incoming/ cleanup
}
```

- `Put` is atomic: a temporary file and `rename` on disk, checksum
  verification on S3.
- `Open` returns an `io.ReaderAt`; on S3 every read is a range request with a
  small block cache.
- Backends are configured with URLs (`file:///srv/archive`,
  `s3://bucket/prefix?endpoint=…&region=…`) and can be wrapped with an
  asynchronous replica.
- The layout is the same for every backend, so moving an archive is a plain
  copy (`rclone copy`).
- Several **named storages** can be configured; each is a complete,
  independent archive with its own epochs, packages and deduplication.

New backends implement the interface and must pass the shared contract tests
in `internal/storage/storagetest`.

## Index and reading

The PostgreSQL database has two schemas:

- `archive_index` – **derived**: files, WARC records with their locations
  (`location`, `offset`, `length`), URLs with the metadata of their latest
  capture, pages with full-text search (`tsvector`, diacritics folded, title
  weighted above description and keywords, then text), the structured data
  of pages in browsable tables (`page_items`, `page_properties`,
  `page_keywords`), epochs and statistics. Its schema
  has a version; an index with another version is dropped and rebuilt from
  storage on start.
- `app` – **operational state** that cannot be rebuilt: users, sessions, API
  tokens and keys, the job queue, sources and settings. It is migrated in
  place and must be backed up. Losing the queue is not critical: the next
  source polls find missing URLs again; only URLs added by hand and not yet
  captured would be lost.

Reading a record:

1. Find the record for the URL closest to the requested time (by SURT; an
   exact URL match wins, redirects are followed).
2. If it is a `revisit`, follow `WARC-Refers-To` to the original for the
   payload, keeping the HTTP headers of the revisit.
3. `storage.Open(location)` → `ReadAt(offset, length)` → gunzip → parse the
   WARC record.

### Reindexing

On start, and with `hoarfrost reindex`, the coordinator compares `catalog/`
and `weekly/` with its `epochs` table and rebuilds missing or inconsistent
weeks in the background:

1. Sealed weeks: read `indexes/index.cdxj` and `pages/pages.jsonl` from each
   volume with range requests (no WARC scan) and compute absolute offsets
   from the ZIP central directory, one volume at a time.
2. The open week: read the CDXJ files next to the WARC files in `incoming/`.
3. Fallback: a full WARC scan of a volume whose index is damaged.

A sealed week with a volume missing from storage is not rebuilt (and not
taken for an open week): it is marked failed with the missing key, and
`hoarfrost reindex` exits with an error.

## Web application

Server-rendered with `html/template` and htmx; templates and static assets
(CSS, fonts, icons, htmx) are embedded in the binary with `go:embed`, so the
UI makes no external requests. The only exception is full replay:
ReplayWeb.page (AGPL-3.0) is loaded from a CDN and reads the week's
volumes (through its `collection.json`) from the instance with range
requests.

| Screen | Contents |
|---|---|
| Dashboard | archive size, captures per day, deduplication savings, errors, fallback rates, queue, workers and their leases, state of the latest week |
| Sources | feeds, sitemaps and URL lists with engine, fallback, interval and storage; test, poll now, enable/disable; defaults and block lists |
| Queue | waiting, running and failed jobs with the reason; retry; add a URL now |
| Archive | full-text search with filters (source, storage, domain, kind, dates), timeline of captures of a URL |
| Page | quick preview (works for the open week too), full replay, extracted text and structured data, HTML export, raw WARC record |
| System | storages, weeks and their verification, reindexing, block lists, users, read-only API keys |

Security: argon2id passwords with a per-user salt and an HMAC-SHA256 pepper
from `HOARFROST_SECRET` (a leaked database alone does not allow offline
cracking), server-side sessions in `HttpOnly`/`SameSite=Lax` cookies, CSRF
tokens, roles (viewer, editor, admin), API tokens and read-only API keys
stored as SHA-256 digests only, a strict Content-Security-Policy, and a
separate token for `/internal/*`. Access is recovered without the old
`.env` with `hoarfrost user reset-password` on the server.

Metrics are exported at `/metrics` in the Prometheus format.

## Verifying there is no lock-in

Rules:

- No field required for reading that a standard does not define.
- Custom metadata only in `warcinfo`, `metadata` records and additional JSON
  fields (`pages.jsonl`, CDXJ) that other tools ignore.
- ZIP entries always stored; WARC always gzip per record.
- No data needed for reading exists only in the database.

The **conformance suite** (`scripts/conformance.sh`, `conformance/`, run in
CI with `HOARFROST_CONFORMANCE=required`) checks whole weeks, split into
volumes, only with independent tools:

| Check | Tool |
|---|---|
| Record structure and digests | `warcio check`, `warcio index` |
| Package validity | `wacz validate` (py-wacz) |
| Stored ZIP entries | `zipinfo` |
| CDXJ order | `LC_ALL=C sort -c` |
| SURT keys | the Python `surt` library (differential test) |
| Replay | pywb: load the WARC files of all volumes of the week and fetch pages, revisits resolved across volumes |
| Integrity | `sha256sum -c SHA256SUMS` and `datapackage.json` hashes |

The **exit test** (`scripts/exit-test.sh`) simulates the application having
disappeared and uses only `rclone`, `unzip`, `dd`, `gunzip`, `warcio` and
`pywb`:

1. Download a random week (all its volumes) from storage and check
   `SHA256SUMS`.
2. List the volumes and read their `pages/pages.jsonl`: articles and their
   text.
3. Find an article in the volumes' `indexes/index.cdxj` with `grep` and cut
   the record out with `dd`.
4. Load the WARC files of all volumes into one pywb collection and open the
   article.
5. Compare the text with hoarfrost's own export.

The test fails if any step needs hoarfrost code. The storage `README.md`
describes the same steps for whoever reads the archive in the future.

## Testing strategy

| Level | What | Where |
|---|---|---|
| Unit | URL resolution, `srcset`, CSS `url()`/`@import`, lazy loading, SURT and CDXJ order (against `LC_ALL=C sort`), digests, `response`/`revisit` fields, ISO weeks across year boundaries, the seen-URL filter, argon2id with pepper | `internal/*` |
| Golden files | captures of a local test site compared with stored WARC files after normalizing dates and UUIDs; revisits within a week and full records in a new week; HTML export against the original | `internal/engine/static`, `internal/export` |
| Storage contract | put/open/range reads, listing, atomicity, objects over 5 GB and ZIP64, on the filesystem, an in-process S3 fake and a real S3 server | `internal/storage`, `internal/wacz` |
| Properties | a record read by absolute offset equals the one read sequentially; every revisit has an original with the same digest in the same week; `datapackage.json` matches the entries | `internal/wacz`, `internal/archive` |
| Reproducibility | dump the index, drop it, reindex, dump again: identical; also with a damaged CDXJ | `internal/archive` |
| Concurrency and failures | many workers, expired leases, deduplication races, sealing with active leases | `internal/queue`, `internal/coordinator` |
| Fuzzing | WARC, HTTP, CDXJ, SURT, HTML, CSS, sitemap and MHTML parsers | `make fuzz` |
| End to end | the real binary: RSS source, embedded and remote worker, upload, seal, search, export, rebuild on a fresh instance; filesystem and S3 | `e2e/` |

## Code map

```
cmd/hoarfrost/           main and subcommands
cmd/testsite/            deterministic fake news site for development
internal/config/         environment, capture configuration and validation
internal/appdb/          schema app and its migrations
internal/pgdb/           PostgreSQL helpers
internal/sourcestore/    sources and settings in the database
internal/sources/        RSS/Atom, sitemaps, URL lists
internal/queue/          jobs, leases, per-domain limits
internal/coordinator/    scheduling, internal API, dedup lookups, completion, sealing
internal/worker/         lease → capture → put → complete loop, HTTP and local clients
internal/engine/         engine interface; static/ and chrome/ engines
internal/recorder/       recording transport and WARC writer
internal/warcio/         WARC record reader and writer
internal/cdxj/           SURT, CDXJ lines, sorting, ZipNum
internal/wacz/           WACZ building and reading, absolute offsets
internal/epoch/          ISO weeks and storage keys
internal/archive/        sealing, verification, catalog, Parquet, reindexing, storage README
internal/storage/        Storage interface, filesystem, S3, replica, contract tests
internal/index/          schema archive_index, queries, full-text search
internal/extract/        article text, metadata, schema.org, PDF text, OCR
internal/blocklist/      Pi-hole/adblock list parsing and refresh
internal/export/         reading pages from the archive, single-file HTML export
internal/upload/         MHTML, SingleFile and WACZ uploads
internal/legacy/         import of single-file HTML archives
internal/auth/           users, passwords, sessions, tokens, API keys
internal/web/            UI, public API, WACZ serving, replay, metrics
conformance/             checks with independent tools
e2e/                     end-to-end test of the binary
scripts/                 conformance.sh, exit-test.sh
extension/               minimal browser extension
```

## Glossary

- **Epoch** – an ISO week of one storage; the unit of deduplication and
  packaging.
- **Capture** – one page with its resources, written as one WARC file.
- **Revisit** – a WARC record without payload that points to an identical
  earlier record of the same week.
- **Seal** – closing a week and packing it into a WACZ file.
- **Fixity** – integrity checking with hashes, without legal weight.
- **SURT** – a canonical URL form with the host reversed
  (`com,example)/style.css`), the sort key of CDXJ.
