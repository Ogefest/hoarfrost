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
    weekly/<year>/W<week>.wacz               weekly package (permanent)
    weekly/<year>/W<week>.wacz.sha256        SHA-256 of the package (sha256sum format)
    weekly/<year>/W<week>.parquet            page table of the week (Apache Parquet)
    catalog/<year>/W<week>.json              package status: counts, hash, verification

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

weekly/<year>/W<week>.parquet lists every page of the package, one row per
capture, sorted by url and capture time (row groups of 10000 rows carry
min/max statistics and a bloom filter on url): url, captured, title,
description, published, authors, schema_types, schema_org (JSON array of the
schema.org objects found as JSON-LD, microdata or RDFa), source, kind, mime,
status, text_length, warc_file (file in the package's archive/ directory),
record_id and metadata_id. Its SHA-256 is in the catalog. Any Parquet reader
works, e.g. DuckDB:

    SELECT url, title, published FROM 'W41.parquet' WHERE url LIKE 'https://example.org/%';

## Epochs and deduplication

An epoch is one ISO week. Identical payloads within a week are stored once;
later copies are WARC "revisit" records (profile identical-payload-digest)
that point to the original by WARC-Refers-To (record id),
WARC-Refers-To-Target-URI and WARC-Refers-To-Date. A revisit never points
outside its week, so every weekly package is self-contained.

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

    sha256sum -c W41.wacz.sha256
    unzip -l W41.wacz                  # every entry must be "Stored"
    unzip -p W41.wacz datapackage.json # SHA-256 of each entry

Hashes are for fixity checks only and are not legal evidence.

## Reading an article without the application

    unzip -p W41.wacz pages/pages.jsonl | grep -i 'keyword'      # find the page
    unzip -p W41.wacz indexes/index.cdxj | grep '<SURT of URL>'  # locate the record
    unzip -l -v W41.wacz                                         # data offset of archive/<file>
    dd if=W41.wacz bs=1 skip=$((DATA_OFFSET + OFFSET)) count=LENGTH | gunzip

Any WARC tool works as well: "warcio extract", pywb ("wb-manager add"),
ReplayWeb.page (load the .wacz file directly).
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
