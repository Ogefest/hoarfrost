# Operations

## Running

hoarfrost has two kinds of processes:

- `hoarfrost serve`, the **coordinator**. There is exactly one. It is the
  only process that uses the database (PostgreSQL, `HOARFROST_DATABASE`):
  it polls sources, keeps the job queue, answers deduplication lookups,
  indexes finished captures, seals finished weeks, cleans up `incoming/` and
  serves the UI and the API. By default it also runs an **embedded worker**.
- `hoarfrost worker`, an optional **capture node**. Add as many as you
  need, on the same machine or elsewhere. Workers have no database and no
  state: they lease a job from the coordinator, capture the page into a
  local spool file, upload the WARC file to `incoming/` in the storage and
  report the result.

```
            sources (RSS, sitemaps)          other applications (read API)
                       │                                ▲
                       ▼                                │
        ┌──────────── hoarfrost serve ──────────────────┴──┐
        │ scheduler · queue · index · sealing · UI · API   │──── PostgreSQL
        │ embedded worker (HOARFROST_WORKERS captures)     │
        └──────▲───────────────────────────────────────────┘
               │ /internal/lease, /dedup, /complete (HOARFROST_WORKER_TOKEN)
        ┌──────┴──────┐  ┌─────────────┐
        │ worker      │  │ worker      │  … 0..N, optional
        └──────┬──────┘  └──────┬──────┘
               ▼                ▼
        storage: incoming/*.warc.gz  →  weekly/*.wacz (sealed by the coordinator)
```

Start with step 1. Step 2 is only needed when one machine does not capture
fast enough; nothing has to be migrated, workers can be added and removed at
any time.

### Step 1: a single process

One `hoarfrost serve` does everything; its embedded worker captures
`HOARFROST_WORKERS` pages in parallel (default 2). This is enough for
typical news sources polled every hour.

```sh
HOARFROST_DATABASE=postgres://hoarfrost:password@db:5432/hoarfrost
HOARFROST_SECRET=<32+ random characters>
HOARFROST_STORAGE=file:///srv/archive          # or s3://…
HOARFROST_DATA_DIR=/var/lib/hoarfrost
HOARFROST_WORKERS=2                            # parallel captures of the embedded worker
./hoarfrost serve
```

With Docker, the same image runs `serve` by default:

```sh
docker run -d --name hoarfrost -p 8080:8080 \
  -e HOARFROST_DATABASE=… -e HOARFROST_SECRET=… -e HOARFROST_STORAGE=file:///srv/archive \
  -v hoarfrost-data:/var/lib/hoarfrost -v /srv/archive:/srv/archive \
  hoarfrost:latest
```

Open the UI, create the administrator and add sources. The image contains
Chromium (`chrome` engine) and `pdftotext`.

### Step 2: adding workers

Add workers when the queue keeps growing (Dashboard → Queue, or the
`hoarfrost_queue_jobs` metric), especially with the `chrome` engine, which
is slow and memory hungry, or to capture from another network or IP
address.

1. **Enable the worker API on the coordinator** by setting a shared secret
   and restarting it. Without it `/internal/*` is disabled and workers are
   refused:

   ```sh
   HOARFROST_WORKER_TOKEN=<random secret>
   ```

2. **Run a worker** with the same token, the coordinator's URL and the same
   storage. It needs neither the database nor `HOARFROST_SECRET`:

   ```sh
   HOARFROST_COORDINATOR=https://archive.example.org   # must reach the coordinator
   HOARFROST_WORKER_TOKEN=<same secret>
   HOARFROST_STORAGE=<same storage as the coordinator>  # + S3 credentials
   HOARFROST_WORKER_NAME=worker-2                       # shown on the Dashboard; default: host name
   HOARFROST_WORKERS=4                                  # parallel captures on this node
   ./hoarfrost worker                                   # or: -coordinator URL -slots 4 -engines chrome
   ```

   With Docker: `docker run -d -e … --tmpfs /var/lib/hoarfrost hoarfrost:latest worker`.
   The data directory holds only the spool, so it may be temporary.

3. **Check** that the worker appears under Workers on the Dashboard, with
   the number of leases it holds, within a minute.

Things to keep in mind:

- **Storage must be shared.** Workers write WARC files straight to the
  storage, so they need the same storages as the coordinator: an S3 bucket
  (simplest for workers on other machines) or a directory mounted at the same
  path. A worker only takes jobs for the storages it has configured
  (`HOARFROST_STORAGES`), so a worker can be dedicated to some of them.
- **Engines**: `-engines` limits what a worker offers, e.g. a large machine
  with `-engines chrome` for heavy pages while the coordinator handles
  `static`. `chrome` needs Chromium (`HOARFROST_CHROME`), present in the
  Docker image.
- **The coordinator can stop capturing** once dedicated workers exist:
  `HOARFROST_EMBEDDED_WORKER=false` leaves it with scheduling, indexing,
  sealing and the UI.
- **Politeness is global.** The limit of parallel captures per domain
  (Sources → Defaults) applies across all workers, so adding workers does
  not hammer a single site.
- **Workers can be stopped at any time.** A worker that dies loses nothing:
  its lease expires (`HOARFROST_LEASE`, default 10 minutes) and the job is
  handed out again. After three expired leases a job is marked failed and
  can be retried from the Queue page.
- The worker API is plain HTTP with a bearer token; across untrusted
  networks put the coordinator behind HTTPS.

### Development environment

`docker compose up --build` starts a complete setup to try both steps on one
machine:

| Service | Role |
|---|---|
| `postgres` | PostgreSQL 17 for the coordinator; set `HOARFROST_DATABASE` to use your own server instead |
| `hoarfrost` | coordinator with the embedded worker (step 1), port 8080 |
| `worker` | one additional worker (step 2), named `compose-worker` |
| `testsite` | a deterministic fake news site for testing, `http://testsite:8000/rss.xml`; it archives nothing |

`docker compose up hoarfrost` runs step 1 alone (the database and the test
site start as its dependencies); `docker compose up --scale worker=3` adds more workers (they
share the name `compose-worker`, so the Dashboard shows them as one entry;
the name is only informative). Machine-specific changes (another database,
ports, mounts) go into `docker-compose.override.yml`, which Compose merges
automatically and git ignores.

## Configuration

Instance settings come from environment variables, optionally from a `.env`
file (see `.env.example`).

The capture configuration is not a file: a new instance starts without
sources, and they are added on the **Sources** page (or through
`/api/sources`, see [api.md](api.md)). Sources, defaults, block list settings
and the poll state of every source (last poll, URLs found and new, last
error) are stored in the database, so the schedule survives restarts. Every
change is also copied to `config/sources/<timestamp>.yaml` in each storage,
so the archive documents how it was collected.

A source has feeds, sitemaps and/or fixed URLs and optional settings; empty
settings are inherited from the defaults (**Sources → Defaults and block
lists**): engine, fallback engine, poll interval, maximum entry age,
storage, minimum text length, priority, maximum response size, attachments,
User-Agent, Chrome scrolling and wait, blocked domains (added to the
defaults) and iframes not captured. Parallel captures per domain is global.
A source can be tested without saving, polled on demand, disabled or
deleted (its archived pages stay).

## Multiple storages

Besides the default storage (`HOARFROST_STORAGE`) any number of named storages
can be configured, e.g. several buckets or directories:

```sh
HOARFROST_STORAGES=press=s3://press-bucket/archive?endpoint=https://s3.example.com&region=eu-central,local=file:///srv/local
HOARFROST_REPLICAS=press=file:///mnt/backup/press          # optional, per storage
HOARFROST_S3_PRESS_ACCESS_KEY_ID=...                        # optional per-storage credentials
HOARFROST_S3_PRESS_SECRET_ACCESS_KEY=...
```

Captures are routed to a storage by the source (its storage setting,
inherited from the defaults) or explicitly when a URL is added
or a page uploaded (`storage` field in the API and a selector in the UI).
Fallback captures and attachments stay in the storage of their page.

Every storage is a complete, independent archive with its own `README.md`,
`incoming/`, weekly packages, catalog and a copy of the configuration.
Deduplication never crosses storages, so each package remains
self-contained, and the "already seen" URL filter is kept per storage (the
same article may be archived in two storages). Search covers all storages,
with a filter. Workers only take jobs for storages they have configured, so a
worker can be limited to some buckets. CLI commands take `-storage` (`seal`,
`verify`, `reindex`, `import-legacy`); without it `verify` and `reindex` cover
every storage. Packages are served at `/wacz/<storage>/<year>/W<week>.wacz`.

## Block lists

Ads and trackers are filtered with public lists, the same ones Pi-hole and
ad blockers use. Configure them under **Sources → Defaults and block
lists** (or `PUT /api/settings/blocklists`): list URLs, the refresh interval
(default 24h; conditional requests with ETag/Last-Modified) and domains
that are never blocked. Until lists are configured the Pi-hole default list
(`https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts`) is
used; an empty list of URLs disables block lists.

Supported formats: hosts files (`0.0.0.0 domain`, exact host as in Pi-hole),
plain domain lists, `*.domain` wildcards and adblock rules `||domain^`
(domain with subdomains, only rules without page-specific options), with
`@@||domain^` exceptions.

- The coordinator downloads the lists, keeps copies in `<data>/blocklists/`
  (used after restarts and when a download fails) and refuses to replace a
  list with an empty download.
- Every lease carries the list version; workers fetch the compiled set from
  `/internal/blocklist` when it changes.
- Blocked requests are not made at all, so nothing from those domains is
  archived; the host of the archived page itself is never blocked by lists.
- The version of the lists is written to each capture's `warcinfo` record and
  the number of blocked requests to its metadata, so filtering is documented
  in the archive.
- The settings page shows each list, its rule count, last download and errors,
  and can update them on demand.

## Epochs and sealing

An epoch is an ISO week (UTC). The epoch of a capture is decided when the
job is leased. After the week ends and `HOARFROST_SEAL_GRACE` (default 6h)
passes, and no lease of the week is active, the coordinator:

1. lists `incoming/<year>/W<week>/` and checks it against the index,
2. builds `weekly/<year>/W<week>.wacz`, verifies it locally, uploads it,
   writes the `.sha256` file and `catalog/<year>/W<week>.json`,
3. moves index entries to absolute offsets inside the package,
4. verifies the stored package and records the result in the catalog,
5. after `HOARFROST_INCOMING_RETENTION` (default 30 days) deletes the
   packaged `incoming/` files (`hoarfrost cleanup` does the same on demand).
   On S3 you may use a lifecycle rule on `incoming/` instead.

A late completion for a week that is being sealed is rejected and the job
returns to the queue. Use `hoarfrost seal -epoch 2026-W41 -force` to seal a
week manually (e.g. in tests).

## Database

All state outside the archive lives in one PostgreSQL database
(`HOARFROST_DATABASE=postgres://user:password@host:5432/hoarfrost`):

- schema `app` – users, sessions, API tokens, the job queue, sources,
  settings and host aliases of discovered URLs (migrated in place by new
  versions);
- schema `archive_index` – record locations, pages, full-text search and
  statistics; a read-optimized copy of what is in storage, dropped and
  rebuilt from storage when a new version changes its layout.

### Browsing structured data

The structured data of every capture is also stored in tables of
`archive_index`, ready for SQL, BI tools or notebooks (the same data is in
the read API and the weekly Parquet files):

| Table | One row per | Main columns |
|---|---|---|
| `pages` | capture | `uri`, `title`, `description`, `captured`, `published`, `authors`, `keywords`, `schema_org`, `open_graph`, `microformats` (whole JSON per page) |
| `page_items` | schema.org object or microformats2 item, nested ones included | `page_id`, `format` (`schema.org`, `microformats2`), `type` (`NewsArticle`, `h-entry`), `types`, `parent_id`, `property` (`author`, `children`…), `data` (JSONB) |
| `page_properties` | Open Graph or Twitter Card value | `page_id`, `format` (`opengraph`, `twitter`), `name` (`og:image`, `article:tag`), `position`, `value` |
| `page_keywords` | keyword of a page | `page_id`, `keyword`, `key` (lower case without diacritics) |

```sql
SET search_path = archive_index;

-- Which kinds of structured data do the sources publish?
SELECT p.source, i.format, i.type, count(*) FROM page_items i JOIN pages p USING (page_id)
GROUP BY 1, 2, 3 ORDER BY 1, 4 DESC;

-- Most frequent keywords this month.
SELECT min(k.keyword) AS keyword, count(*) FROM page_keywords k JOIN pages p USING (page_id)
WHERE p.captured >= date_trunc('month', now()) GROUP BY k.key ORDER BY 2 DESC LIMIT 50;

-- People and organizations named in articles, with the property naming them.
SELECT i.type, i.property, i.data->>'name' AS name, count(*) FROM page_items i
WHERE i.format = 'schema.org' AND i.type IN ('Person', 'Organization') AND i.parent_id IS NOT NULL
GROUP BY 1, 2, 3 ORDER BY 4 DESC;

-- Articles with their Open Graph image.
SELECT p.uri, p.title, o.value AS image FROM pages p
JOIN page_properties o ON o.page_id = p.page_id AND o.name = 'og:image' AND o.position = 0;
```

These tables are part of the derived index: they are rebuilt from storage
with the rest of it, and queries should identify pages by `metadata_id`
(the `captureId` of the API), not by `page_id`.

Only the coordinator and the command-line tools connect to the database;
workers do not. Instances that used SQLite (`app.sqlite`) are moved with
`hoarfrost import-sqlite <data dir>/app.sqlite` into an empty database; the
index is rebuilt from storage on the first start.

## Backups and recovery

- **The archive in storage is the source of truth.** Replicate it with
  `HOARFROST_STORAGE_REPLICA` or any copy tool (`rclone copy` works, layouts
  are identical across backends).
- The database holds two schemas. `archive_index` (the index) is derived:
  drop it (`DROP SCHEMA archive_index CASCADE`) and run `hoarfrost reindex`,
  or just restart; missing epochs are rebuilt at startup.
- Schema `app` holds users, tokens, the queue and sources; back it up with
  `pg_dump -n app`. Losing it is not fatal:
  feeds and sitemaps are polled again and `seen_urls` is rebuilt from the
  index; only manually added, not yet captured URLs are lost.
- `HOARFROST_SECRET` is the password pepper. If it is lost, reset passwords
  with `hoarfrost user reset-password <login>` (needs server access).

## Integrity

`hoarfrost verify` re-reads every package: SHA-256 of the file and of each
entry, every ZIP entry stored, every record readable at its absolute offset
with valid block and payload digests, every revisit pointing to a record with
the same digest inside the same package. Results are written to the catalog
and shown on the System page. Hashes are for fixity, not legal evidence.

The independent conformance suite (`scripts/conformance.sh`) and the exit
test (`scripts/exit-test.sh`) check packages with `warcio`, `py-wacz`, `pywb`,
`unzip` and `sha256sum` only.

## Monitoring

`GET /healthz` answers `ok` while the process serves HTTP. `GET /metrics`
exposes Prometheus metrics; authenticate with a read-only API key (System →
API keys) or a user token:

```yaml
scrape_configs:
  - job_name: hoarfrost
    scheme: https
    authorization:
      credentials_file: /etc/prometheus/hoarfrost.key   # hfr_…
    static_configs:
      - targets: [archive.example.org]
```

Problems are also visible without Prometheus: the last poll error of each
source (and its last success) on the Sources page, failed jobs with their
reason on the Queue page, seal and verification results under System, and
warnings in the log (`source poll`, `seal`, `block lists`, `capture failed`).

### Metrics

| Metric | Labels | Meaning |
|---|---|---|
| `hoarfrost_source_failing` | `source` | 1 when the last poll of the feeds and sitemaps reported an error: HTTP error, timeout, invalid XML, also when only one sitemap of an index failed |
| `hoarfrost_source_last_success_timestamp_seconds` | `source` | last poll without errors (kept in the database across restarts) |
| `hoarfrost_source_last_poll_timestamp_seconds` | `source` | last poll, successful or not |
| `hoarfrost_source_poll_interval_seconds` | `source` | the source's poll interval, for thresholds relative to it |
| `hoarfrost_source_urls_found` | `source` | URLs found by the last poll; 0 for a working source usually means the feed moved or changed format |
| `hoarfrost_source_poll_errors_total` | `source` | failed polls since the start |
| `hoarfrost_captures_total` | `source` | captures committed to the archive |
| `hoarfrost_capture_failures_total` | `source`, `reason` | failed capture attempts (see below) |
| `hoarfrost_fallbacks_total` | `source` | captures repeated with the fallback engine because the text was too short (also a hint of consent walls and paywalls) |
| `hoarfrost_queue_jobs` | `state` | jobs by state: `queued`, `leased`, `done`, `failed` |
| `hoarfrost_workers` | | workers seen in the last hour |
| `hoarfrost_epochs` | `storage`, `state` | weeks by state: `open`, `sealing`, `sealed`, `verified`, `failed` (verification failed) |
| `hoarfrost_oldest_unsealed_epoch_end_timestamp_seconds` | `storage` | end of the oldest week that is not sealed yet |
| `hoarfrost_seal_errors` | `storage` | weeks whose last sealing attempt failed |
| `hoarfrost_blocklist_failing` | `list` | 1 when the last refresh of a block list failed |
| `hoarfrost_records_total`, `hoarfrost_capture_bytes_total`, `hoarfrost_archive_bytes`, `hoarfrost_pages`, `hoarfrost_revisit_records`, `hoarfrost_dedup_saved_bytes`, `hoarfrost_blocklist_rules` | | volume and deduplication |

Source metrics cover enabled sources only, so disabling a source silences
its alerts. Counters start from zero when the coordinator restarts; use
`increase()` or `rate()`.

`reason` of `hoarfrost_capture_failures_total` is one of:

| Reason | Cause |
|---|---|
| `http_401`, `http_403` | login required, paywall, IP address or User-Agent blocked |
| `http_404`, `http_410` | the page was removed before it was captured, or a broken link |
| `http_429` | rate limited: lower `max_per_domain` |
| `http_4xx`, `http_5xx` | other client and server errors (5xx and 429 are retried) |
| `timeout`, `network` | timeouts, DNS errors, refused connections, TLS errors |
| `blocked` | the URL's host is on a block list or the source's blocked domains |
| `storage` | the capture could not be uploaded, or the worker lacks the storage |
| `blocklist` | the worker could not load the block lists from the coordinator |
| `engine` | engine or recorder failure, e.g. Chromium did not start |
| `other` | reported by a worker of an older version |

The page of an HTTP error response is still stored in the archive, as
evidence of what the server returned.

### Alerts

[`deploy/alerts.yml`](../deploy/alerts.yml) contains ready Prometheus rules,
unit-tested in CI with `promtool test rules deploy/alerts_test.yml`:

| Alert | Fires when |
|---|---|
| `HoarfrostSourceFailing` | a source's polls have been failing for 2 hours |
| `HoarfrostSourceStale` | no successful poll for 3 poll intervals (at least 6 hours) |
| `HoarfrostSourceEmpty` | polls succeed but find no URLs for 6 hours |
| `HoarfrostSourceNotPolled` | the scheduler has not polled a source for 3 intervals |
| `HoarfrostAccessDenied` | more than 3 pages of a source answered 401/403 within an hour |
| `HoarfrostRateLimited` | more than 10 answers 429 within an hour |
| `HoarfrostCaptureErrorRate` | over half of a source's capture attempts failed for an hour |
| `HoarfrostStorageErrors` | any capture could not be written to storage |
| `HoarfrostEngineErrors` | repeated engine failures (Chromium) |
| `HoarfrostNoCaptures` | jobs are queued but nothing was captured for 3 hours |
| `HoarfrostQueueGrowing` | more than 1000 queued jobs and still growing |
| `HoarfrostNoWorkers` | no worker contacted the coordinator for 15 minutes |
| `HoarfrostSealFailed` | sealing a week failed |
| `HoarfrostVerificationFailed` | a weekly package failed verification |
| `HoarfrostWeekNotSealed` | a week ended over a day ago and is still open |
| `HoarfrostBlocklistFailing` | a block list could not be refreshed for a day |
| `HoarfrostDown` | the scrape fails (assumes the job name `hoarfrost`) |

```yaml
# prometheus.yml
rule_files: [/etc/prometheus/hoarfrost-alerts.yml]
```

Thresholds are starting points; copy the file and tune them to your sources.

## Security notes

- Run behind TLS; cookies are `Secure` unless `HOARFROST_INSECURE_COOKIE=true`.
- Archived pages are shown in a sandboxed preview without scripts and network
  access (Content-Security-Policy `sandbox`).
- `Cookie`, `Authorization` and `Proxy-Authorization` are removed from
  request records; uploads from browsers also lose `Set-Cookie`.
- Secrets never enter the archive; only the capture configuration is copied
  there (`config/sources/`).
