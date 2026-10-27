# Archive formats

hoarfrost writes only standard formats; its own metadata uses extension points
that other tools ignore.

| Element | Format |
|---|---|
| HTTP records | WARC 1.1, gzip per record (`.warc.gz`), raw bytes as received |
| Record index | CDXJ (SURT key, 14-digit timestamp, JSON), sorted bytewise |
| Week | WACZ 1.1.1 volumes, ZIP with every entry stored (ZIP64 when needed) |
| Week collection | multi-WACZ `collection.json` (ReplayWeb.page) |
| Page list and text | `pages/pages.jsonl` |
| Fixity | SHA-256 in `datapackage.json` and `SHA256SUMS` (sha256sum format) |
| Payload digest | `WARC-Payload-Digest: sha1:<BASE32>` over the bytes after the HTTP headers |

## WARC records

Each capture file starts with a `warcinfo` record (software, engine, worker,
epoch, source). Then for every HTTP exchange a `request` record (sensitive
headers removed) and a `response` record, or a `revisit` record:

```
WARC-Type: revisit
WARC-Profile: http://netpreserve.org/warc/1.1/revisit/identical-payload-digest
WARC-Refers-To: <urn:uuid:…>
WARC-Refers-To-Target-URI: https://news.example.com/style.css
WARC-Refers-To-Date: 2026-10-11T07:02:00Z
WARC-Payload-Digest: sha1:…
```

A revisit holds the HTTP headers of its own capture and no body. It always
refers to a record of the same ISO week by record ID, never by file name, so
a week is self-contained; the record may be in another volume of the week. A `304 Not Modified` response is stored
as a revisit with the `server-not-modified` profile.

`resource` records hold content without an HTTP layer: MHTML or SingleFile
HTML from browsers (`Content-Type: multipart/related` / `text/html`), the
rendered DOM of chrome captures (`WARC-Target-URI: urn:dom:<url>`) and
imported legacy pages.

Every captured page has a `metadata` record (`WARC-Target-URI:
urn:pageinfo:<url>` as in Browsertrix, so replay tools never confuse it with
the page; `Content-Type: application/json;
profile="https://github.com/ogefest/hoarfrost/capture-metadata"`;
`WARC-Refers-To` and `WARC-Refers-To-Target-URI` = the page record; files
written before the project was renamed use the profile
`https://github.com/ogefest/bindery/capture-metadata`, which readers accept
as well) with: `url`, `recordId`, `title`,
`text`, `ts`, `published`, `source`, `engine`, `fallback`, `kind`
(`page`, `attachment`, `ocr`), `status`, `mime`, resource counts and
`extra` (e.g. `finalUrl`, `dom`). Pages captured since structured data is
extracted also carry `description` (meta or Open Graph description),
`schemaOrg` (the page's top-level schema.org objects from JSON-LD, microdata
and RDFa, each with `@type`; at most 32 objects and 64 KiB) and `authors`
(names given as author, creator, contributor or editor in those objects,
`h-entry` authors and `article:author` names). Newer captures also carry
`openGraph` (Open Graph and Twitter Card properties: `og:`, `article:`,
`book:`, `profile:`, `music:`, `video:`, `twitter:`; a string, or a list
when repeated; at most 100 values), `microformats` (top-level microformats2
items in the JSON format of the mf2 parsing specification, relative URLs
resolved, embedded HTML of `e-*` properties left out; at most 32 items and
64 KiB) and `keywords` (schema.org `keywords` and `articleSection`,
`article:tag`, `p-category` of `h-entry` items, `<meta name="keywords">`;
at most 50). The same fields are in `pages/pages.jsonl` of the volume.

## Page table

Sealing a week also writes `weekly/<year>/W<week>.parquet` (Apache Parquet,
zstd), one row per page capture, rows sorted by `url` and `captured`, row
groups of 10000 rows with statistics, sorting metadata and a bloom filter on
`url`. Columns: `url`, `captured`, `title`, `description`, `published`,
`authors`, `schema_types`, `schema_org` (JSON), `open_graph` (JSON),
`microformats` (JSON), `keywords`, `source`, `kind`, `mime`,
`status`, `text_length`, `warc_file`, `record_id`, `metadata_id`. The file is
derived from the index at sealing time; its SHA-256 and row count are in the
catalog and checked by `hoarfrost verify`. Files of weeks sealed before a
column was added lack it; DuckDB reads mixed files with
`read_parquet('weekly/*/*.parquet', union_by_name = true)`.

## Week layout

```
weekly/2026/W41/2026-W41-0001.wacz             volume 1 (WACZ)
weekly/2026/W41/2026-W41-0001.captures.jsonl   capture files in volume 1
weekly/2026/W41/2026-W41-0002.wacz             …
weekly/2026/W41/SHA256SUMS                     "<sha256>  2026-W41-0001.wacz" per volume
weekly/2026/W41/collection.json                multi-WACZ collection of the volumes
weekly/2026/W41.parquet                        page table
catalog/2026/W41.json                          volumes, counts, hashes, verification
```

Volumes hold about `HOARFROST_VOLUME_SIZE` bytes of capture files (4 GiB
by default); a capture file is never split between volumes. Every volume
is a complete WACZ package; a revisit may point into another volume of the
week, so the week is loaded as a whole. `collection.json` is what
ReplayWeb.page opens as one archive:

```json
{"profile":"multi-wacz-package","title":"Web archive 2026-W41","resources":[{"name":"2026-W41-0001.wacz","path":"2026-W41-0001.wacz","hash":"sha256:…","bytes":…}]}
```

Weeks sealed before volumes have a single package `weekly/2026/W41.wacz`
with `W41.wacz.sha256` and `W41.captures.jsonl` instead.

## Captures list

Next to each volume, `<year>-W<week>-<n>.captures.jsonl` lists the capture
files it holds. The first line describes the volume:

```json
{"format":"hoarfrost-captures-1","epoch":"2026-W41","package":"weekly/2026/W41/2026-W41-0001.wacz","sha256":"…","size":…,"created":"…","warcFiles":…,"records":…,"indexed":…,"revisits":…,"pages":…,"cdxjLines":…,"captures":…}
```

Each further line is one capture file: its `incoming/` key, the WARC file of
the volume holding it (`file`), the absolute `offset` of its first byte in
the volume, its `size`, the `sha256` of its bytes taken when it was read
from `incoming/`, and its counts of `records`, `indexed` records and
`pages`. Verification checks every capture against it; it also proves that a
file in `incoming/` was packaged before it is removed:

```sh
sha256sum incoming/2026/W41/<file>.warc.gz
grep '<file>.warc.gz' weekly/2026/W41/*.captures.jsonl
```

## WACZ volume

```
archive/<capture>.warc.gz      capture files (or <source>-<n>.warc.gz per source and volume with HOARFROST_CONCAT_BY_SOURCE)
indexes/index.cdxj             or indexes/index.idx + indexes/index.cdx.gz (ZipNum) above 200k lines
pages/pages.jsonl              {"format":"json-pages-1.0",…} then one page per line
datapackage.json               resources with sha256 and size, wacz_version 1.1.1
datapackage-digest.json        sha256 of datapackage.json
```

CDXJ lines carry the standard fields `url`, `mime`, `status`, `digest`,
`length`, `offset`, `filename` (offset relative to that WARC file) and the
extensions `rid` (record ID), `type` and `refersTo`. `pages.jsonl` lines add
`metadataId`, `filename`, `metaOffset` and `metaLength` to the standard
`url`, `ts`, `title`, `text`.

SURT keys follow the Python `surt` library (used by pywb, warcio and
py-wacz) exactly; a differential test compares them on thousands of URLs.

## Reading without hoarfrost

```sh
cd weekly/2026/W41 && sha256sum -c SHA256SUMS
unzip -l 2026-W41-0001.wacz
for v in *.wacz; do unzip -p $v pages/pages.jsonl | grep -i 'budget' && echo $v; done
unzip -p 2026-W41-0003.wacz indexes/index.cdxj | grep '"url":"https://news.example.com/article"'
unzip -p 2026-W41-0003.wacz archive/<filename> > f.warc.gz
dd if=f.warc.gz bs=1 skip=<offset> count=<length> | gunzip
```

Or open `collection.json` (or a single volume) in ReplayWeb.page, or load
the WARC files of all volumes of the week into one pywb collection:

```sh
for v in *.wacz; do unzip -o $v 'archive/*' -d w41; done
wb-manager init w41 && wb-manager add w41 w41/archive/*.warc.gz
```

`scripts/exit-test.sh` automates these steps.
