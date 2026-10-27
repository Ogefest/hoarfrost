package archive

import (
	"bytes"
	"context"
	"errors"
	"text/template"

	"github.com/ogefest/hoarfrost/internal/storage"
	"github.com/ogefest/hoarfrost/internal/version"
	"github.com/ogefest/hoarfrost/internal/wacz"
)

// ReadmeKey is the self-description of the archive layout.
const ReadmeKey = "README.md"

var readmeTmpl = template.Must(template.New("readme").Parse(`# Press and web archive

This storage holds a long-term web archive written by {{.Software}}. Everything
needed to read it is described here; the application is not required.

## Layout

    README.md                                this file
    config/sources/<timestamp>.yaml          history of capture configuration
    incoming/<year>/W<week>/<ts>-<source>-<id>.warc.gz
                                             one WARC file per captured page
                                             (removed some time after the week is packaged)
    incoming/<year>/W<week>/<ts>-<source>-<id>.cdxj
                                             CDXJ index of the WARC file next to it
    weekly/<year>/W<week>/<year>-W<week>-<n>.wacz
                                             the week's packages (volumes of a bounded size, permanent)
    weekly/<year>/W<week>/<year>-W<week>-<n>.captures.jsonl
                                             capture files in the volume: offset, size, SHA-256
    weekly/<year>/W<week>/SHA256SUMS         SHA-256 of every volume (sha256sum format)
    weekly/<year>/W<week>/collection.json    multi-WACZ collection of the volumes (ReplayWeb.page)
    weekly/<year>/W<week>.parquet            page table of the week (Apache Parquet)
    catalog/<year>/W<week>.json              week status: volumes, counts, hashes, verification

Weeks sealed before volumes have one package instead:
weekly/<year>/W<week>.wacz with W<week>.wacz.sha256 and W<week>.captures.jsonl.

A week is the unit: load all its volumes together (see "Epochs and
deduplication"). Every volume is a complete WACZ package with its own
indexes and pages list; a capture file is never split between volumes.

## Formats

- WARC 1.1 (ISO 28500:2017), gzip compressed per record (` + "`.warc.gz`" + `).
  Records hold raw HTTP traffic as received from the server.
- WACZ {{.WACZ}} (ZIP, every entry stored without compression) containing:
  ` + "`archive/*.warc.gz`" + `, ` + "`indexes/index.cdxj`" + ` (or a ZipNum block index
  ` + "`indexes/index.idx`" + ` + ` + "`indexes/index.cdx.gz`" + ` for large weeks),
  ` + "`pages/pages.jsonl`" + ` with extracted article text, ` + "`datapackage.json`" + `
  (SHA-256 of every entry) and ` + "`datapackage-digest.json`" + `.
- CDXJ: "<SURT key> <14 digit timestamp> <JSON>", sorted bytewise (LC_ALL=C).
  Offsets are relative to the WARC file named in "filename". Extra fields
  "rid" (record id), "type" and "refersTo" are hoarfrost extensions.

## Page table

weekly/<year>/W<week>.parquet lists every page of the week, one row per
capture, sorted by url and capture time (row groups of 10000 rows carry
min/max statistics and a bloom filter on url): url, captured, title,
description, published, authors, schema_types, schema_org (JSON array of the
schema.org objects found as JSON-LD, microdata or RDFa), open_graph (JSON
object of Open Graph and Twitter Card properties), microformats (JSON array
of microformats2 items), keywords, source, kind, mime,
status, text_length, warc_file (file in a volume's archive/ directory),
record_id and metadata_id. Its SHA-256 is in the catalog. Any Parquet reader
works, e.g. DuckDB:

    SELECT url, title, published FROM 'W41.parquet' WHERE url LIKE 'https://example.org/%';

## Epochs and deduplication

An epoch is one ISO week. Identical payloads within a week are stored once;
later copies are WARC "revisit" records (profile identical-payload-digest)
that point to the original by WARC-Refers-To (record id),
WARC-Refers-To-Target-URI and WARC-Refers-To-Date. A revisit never points
outside its week, but it may point into another volume of the week: the
volumes of a week together are self-contained. For pywb, add the WARC
files of all volumes of the week to one collection:

    for v in 2026-W41-*.wacz; do unzip -o "$v" 'archive/*' -d w41; done
    wb-manager init w41 && wb-manager add w41 w41/archive/*.warc.gz

Until a week is packaged, its WARC files and their .cdxj indexes in
incoming/<year>/W<week>/ together form a complete collection: a revisit is
resolved by looking up its digest in the indexes of the whole directory.
For pywb, point both index_paths and archive_paths of a collection at that
directory.

## Capture metadata

Each capture file contains a "warcinfo" record (software, engine, worker,
epoch) and a "metadata" record per page with Content-Type
application/json; its fields are url, ts, title, text, published, source,
engine and resource counts.

## Verifying integrity

    cd weekly/2026/W41 && sha256sum -c SHA256SUMS
    unzip -l 2026-W41-0001.wacz                  # every entry must be "Stored"
    unzip -p 2026-W41-0001.wacz datapackage.json # SHA-256 of each entry

2026-W41-0001.captures.jsonl lists every capture file of the volume with
the absolute offset of its first byte in the volume, its size and its
SHA-256, taken when it was read from incoming/: the bytes at that offset
must hash to it. Together the lists name every capture file of the week.

Hashes are for fixity checks only and are not legal evidence.

## Reading an article without the application

    for v in *.wacz; do unzip -p $v pages/pages.jsonl | grep -i 'keyword' && echo $v; done   # find the page
    unzip -p 2026-W41-0003.wacz indexes/index.cdxj | grep '<SURT of URL>'  # locate the record
    unzip -l -v 2026-W41-0003.wacz                                         # data offset of archive/<file>
    dd if=2026-W41-0003.wacz bs=1 skip=$((DATA_OFFSET + OFFSET)) count=LENGTH | gunzip

Any WARC tool works as well: "warcio extract", pywb ("wb-manager add" with
the WARC files of all volumes of the week), ReplayWeb.page (load
collection.json of the week, or a single .wacz file).
`))

// WriteReadme stores README.md when its content changed.
func (a *Archive) WriteReadme(ctx context.Context) error {
	var b bytes.Buffer
	if err := readmeTmpl.Execute(&b, map[string]string{"Software": version.Software(), "WACZ": wacz.Version}); err != nil {
		return err
	}
	cur, err := storage.ReadAll(ctx, a.Storage, ReadmeKey)
	if err == nil && bytes.Equal(cur, b.Bytes()) {
		return nil
	}
	if err != nil && !errors.Is(err, storage.ErrNotExist) {
		return err
	}
	return storage.PutBytes(ctx, a.Storage, ReadmeKey, b.Bytes())
}
