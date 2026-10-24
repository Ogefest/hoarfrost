# HTTP API

hoarfrost collects and stores pages; other applications present and analyse
them through the read API. Two kinds of credentials are accepted:

- **API keys** (`hfr_…`) for applications. An admin creates them under
  System → API keys, optionally with an expiry date. They belong to the
  instance, not to a user, and only work on the read endpoints marked
  *key* below; anything else answers 401.
- **User tokens** (`hf_…`), created on the Account page or with
  `hoarfrost user token <login> <name>`, carry the role of their user:
  viewer can read, editor can add and upload, admin manages the system.

The read API is specified in [openapi.yaml](openapi.yaml), also served by
every instance at `/api/openapi.yaml`; generate clients from it.

Send either as `Authorization: Bearer <secret>`. Only a hash is stored, so
a lost secret cannot be shown again; revoke it and create a new one.

## Reading the archive

| Method | Path | Access | Description |
|---|---|---|---|
| GET | `/api/pages?after=&limit=&source=&storage=&domain=&kind=&keyword=&from=&to=` | key, viewer | pages in the order they were archived, after a cursor; JSON `{results, next}` |
| GET | `/api/search?q=&source=&storage=&domain=&kind=&keyword=&from=&to=&limit=&offset=` | key, viewer | full-text search, newest or best match first; JSON `{total, results}` |
| GET | `/api/facets` | key, viewer | values for the filters: `sources` (with page counts), `storages`, `kinds` |
| GET | `/api/pages/{id}` | key, viewer | page metadata with extracted text |
| GET | `/api/pages/{id}/content` | key, viewer | the captured file as archived (HTML, PDF, image…) with its content type |
| GET | `/api/export/{id}.html` | key, viewer | single-file HTML export with inlined resources |
| GET | `/api/record/{record-id}` | key, viewer | raw WARC record (`.warc.gz` member); id with or without `<urn:uuid:…>` |
| GET | `/wacz/{storage}/{year}/W{week}.wacz` | key, viewer | weekly package of a storage, with HTTP Range support |
| GET | `/metrics` | key, viewer | Prometheus metrics, see [Monitoring](operations.md#monitoring) |

`{id}` is either the `captureId` or the numeric `id` of a page. **Store the
`captureId`**: it is the UUID of the capture's WARC metadata record and never
changes, while numeric ids are reassigned when a week is reindexed.

Filters: `source`, `storage` and `kind` (`page`, `attachment`, `ocr`) match
exactly; `domain` matches the host and its subdomains; `keyword` matches
pages tagged with it (see `keywords` below), ignoring case and diacritics;
`from` and `to` take
a date (`2026-10-01`, whole day, server time zone) or an RFC 3339 time
(URL-encode the `+`). `limit` is at most 500.

A page:

```json
{
  "id": 2,
  "captureId": "f0d49dfc-0812-476d-9136-8c277663e5ac",
  "url": "https://news.example.com/news/new-tram-line.html",
  "title": "New tram line approved",
  "description": "The city has opened a tender for a new tram line.",
  "captured": "2026-10-24T07:06:17.298683Z",
  "published": "2026-10-08",
  "authors": ["Anna Kowalska"],
  "schemaTypes": ["NewsArticle"],
  "schemaOrg": [{"@type": "NewsArticle", "headline": "New tram line approved"}],
  "openGraph": {"og:type": "article", "og:image": "https://news.example.com/tram.jpg",
                "article:tag": ["Transport", "Investments"], "twitter:card": "summary_large_image"},
  "microformats": [{"type": ["h-entry"], "properties": {"name": ["New tram line approved"],
                    "category": ["transport"], "published": ["2026-10-08T09:30:00+02:00"]}}],
  "keywords": ["Transport", "Investments", "transport"],
  "source": "news-a", "engine": "static", "kind": "page",
  "mime": "text/html", "status": 200, "textLength": 420,
  "storage": "default", "epoch": "2026-W43",
  "recordId": "<urn:uuid:fbaaaeab-06dc-44ed-9f6c-a7e320b8f984>",
  "links": {
    "self": "/api/pages/f0d49dfc-0812-476d-9136-8c277663e5ac",
    "content": "/api/pages/f0d49dfc-0812-476d-9136-8c277663e5ac/content",
    "html": "/api/export/f0d49dfc-0812-476d-9136-8c277663e5ac.html",
    "record": "/api/record/fbaaaeab-06dc-44ed-9f6c-a7e320b8f984"
  }
}
```

`text` (the extracted text) is included only by `/api/pages/{id}`. Optional
fields are left out when unknown.

Structured data comes from the page as captured:

- `schemaOrg` – schema.org objects from JSON-LD, microdata and RDFa Lite;
- `openGraph` – Open Graph (`og:`, `article:`, `book:`, `profile:`,
  `music:`, `video:`) and Twitter Card (`twitter:`) properties; a repeated
  property (`og:image`, `article:tag`) is an array;
- `microformats` – microformats2 items (`h-entry`, `h-card`, `h-event`…) in
  the [mf2 JSON format](https://microformats.org/wiki/microformats2-parsing),
  without embedded HTML;
- `keywords` – tags from schema.org `keywords` and `articleSection`,
  `article:tag`, `p-category` of `h-entry` items and `<meta name="keywords">`;
- `authors` and `published` are derived from all of them.

Captures made before a kind of data was extracted do not have it. `/content` adds `X-Hoarfrost-URL`,
`X-Hoarfrost-Captured` and `X-Hoarfrost-Status` (the original HTTP status)
and is served as an attachment in a sandbox, never rendered as part of
hoarfrost.

### Following the archive

To mirror new captures into another application, keep the last `next` value
and ask again:

```sh
curl -H "Authorization: Bearer $KEY" "https://archive.example.org/api/pages?after=0&limit=200"
# → {"results": [...], "next": 200}; then ?after=200, and so on.
```

An empty `results` means you are up to date; `next` stays the same. Delivery
is at least once: reindexing a week inserts its pages again under new ids,
so deduplicate by `captureId`.

## Changing the archive

| Method | Path | Role | Description |
|---|---|---|---|
| POST | `/api/urls` | editor | queue a URL now (bypasses the seen-URL filter); JSON `{url, source?, engine?, storage?}` |
| POST | `/api/upload` | editor | upload MHTML, SingleFile HTML, WACZ or WARC |
| GET | `/api/sources` | editor | sources with poll state, defaults and block list settings |
| PUT | `/api/sources/{name}` | editor | create (201) or replace (200) a source |
| DELETE | `/api/sources/{name}` | editor | delete a source; its archived pages stay |
| POST | `/api/sources/{name}/poll` | editor | poll a source now, JSON `{found, new, error}` |
| PUT | `/api/settings/defaults` | editor | replace the defaults sources inherit |
| PUT | `/api/settings/blocklists` | editor | replace block list settings; `null` restores the default list |
| POST | `/api/blocklists/update` | editor | download block lists now |

These need a user token; API keys are refused.

## Sources

A source is JSON with at least one of `feeds`, `sitemaps`, `urls`; other
fields are optional and inherited from the defaults when omitted:

```sh
curl -X PUT -H "Authorization: Bearer $TOKEN" https://archive.example.org/api/sources/news-a -d '{
  "feeds": ["https://news-a.example.com/rss"],
  "sitemaps": ["https://news-a.example.com/sitemap-news.xml"],
  "poll": "30m",            // default 1h
  "engine": "static",       // static | chrome | http
  "fallback": "chrome",     // "" disables the fallback
  "storage": "press",       // a storage from HOARFROST_STORAGES
  "max_age": "7d", "min_text_chars": 500, "max_size": "100MB", "attachments": true,
  "priority": 100, "user_agent": "", "block": ["ads.example"], "skip_frames": ["youtube.com"],
  "chrome": {"scroll": true, "wait": "3s"},
  "enabled": true
}'
```

(Comments only for illustration; JSON does not allow them.) Unknown fields
are rejected. `GET /api/sources` adds `lastPoll`, `nextPoll`, `found`, `new`
and `lastError` to every source. Defaults take the same settings plus
`max_per_domain`; block list settings are `{"urls": [...], "refresh": "24h",
"allow": [...]}`.

## Upload

Multipart form with any file field and optional `url`, `title`, `date`
(RFC 3339), `storage` and `queue=1` (also queue the URL for an HTTP capture):

```sh
curl -H "Authorization: Bearer $TOKEN" -F file=@page.mhtml -F queue=1 https://archive.example.org/api/upload
```

or a raw body with the same fields as query parameters:

```sh
curl -H "Authorization: Bearer $TOKEN" --data-binary @saved.html \
  "https://archive.example.org/api/upload?url=https://news.example.com/a"
```

The page URL is taken from the file when possible (MHTML
`Snapshot-Content-Location`, SingleFile comment, WACZ `pages.jsonl`).
`Cookie`, `Authorization` and `Set-Cookie` headers are removed from uploaded
WARC data.

## Internal API

`POST /internal/lease`, `/internal/complete`, `/internal/dedup` connect
workers to the coordinator. They require `Authorization: Bearer
$HOARFROST_WORKER_TOKEN` and are disabled when the token is not set.
