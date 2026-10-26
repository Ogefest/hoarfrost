package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/compress/zstd"

	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/index"
	"github.com/ogefest/hoarfrost/internal/storage"
)

// PageRow is one row of the weekly page table (weekly/<year>/W<week>.parquet):
// what is known about every captured page without reading the package.
// Rows are sorted by url and capture time.
type PageRow struct {
	URL         string    `parquet:"url"`
	Captured    time.Time `parquet:"captured,timestamp(millisecond)"`
	Title       string    `parquet:"title"`
	Description string    `parquet:"description"`
	Published   time.Time `parquet:"published,optional,timestamp(millisecond)"` // zero: unknown
	Authors     []string  `parquet:"authors,list"`
	SchemaTypes []string  `parquet:"schema_types,list"`
	// SchemaOrg is the JSON array of schema.org objects of the page.
	SchemaOrg string `parquet:"schema_org,optional,json"`
	// OpenGraph is the JSON object of Open Graph and Twitter Card properties.
	OpenGraph string `parquet:"open_graph,optional,json"`
	// Microformats is the JSON array of microformats2 items.
	Microformats string   `parquet:"microformats,optional,json"`
	Keywords     []string `parquet:"keywords,list"`
	Source       string   `parquet:"source"`
	Kind         string   `parquet:"kind"`
	Mime         string   `parquet:"mime"`
	Status       int32    `parquet:"status"`
	TextLength   int32    `parquet:"text_length"`
	// WARCFile is the WARC file in the package's archive/ directory that
	// holds the page; RecordID and MetadataID are its records there.
	WARCFile   string `parquet:"warc_file"`
	RecordID   string `parquet:"record_id"`
	MetadataID string `parquet:"metadata_id"`
}

// parquetRowGroup bounds rows per row group; with rows sorted by URL the
// min/max statistics and bloom filter of a group locate a URL quickly.
const parquetRowGroup = 10000

func pageRow(p index.Page) PageRow {
	r := PageRow{
		URL: p.URI, Captured: p.Captured.UTC(), Title: p.Title, Description: p.Description, Authors: p.Authors,
		SchemaTypes: p.SchemaTypes, SchemaOrg: string(p.SchemaOrg),
		OpenGraph: string(p.OpenGraph), Microformats: string(p.Microformats), Keywords: p.Keywords, Source: p.Source, Kind: p.Kind, Mime: p.Mime,
		Status: int32(p.Status), TextLength: int32(p.TextLen), WARCFile: p.File, RecordID: p.RecordID, MetadataID: p.MetadataID,
	}
	if t, err := time.Parse(time.DateOnly, p.Published); err == nil {
		r.Published = t
	}
	return r
}

// writeParquet streams the page table of a sealed epoch from the index to
// storage, reading pages already sorted by URL and capture time.
func (a *Archive) writeParquet(ctx context.Context, ep epoch.ID) (*ParquetInfo, error) {
	// A page table is far smaller than its package: small upload parts.
	out, err := storage.Create(ctx, a.Storage, ep.ParquetKey(), 1<<30)
	if err != nil {
		return nil, err
	}
	defer out.Abort()
	h := sha256.New()
	w := parquet.NewGenericWriter[PageRow](io.MultiWriter(out, h),
		parquet.Compression(&zstd.Codec{}),
		parquet.MaxRowsPerRowGroup(parquetRowGroup),
		parquet.SortingWriterConfig(parquet.SortingColumns(parquet.Ascending("url"), parquet.Ascending("captured"))),
		parquet.BloomFilters(parquet.SplitBlockFilter(10, "url")),
		parquet.KeyValueMetadata("hoarfrost.epoch", string(ep)),
	)
	rows := make([]PageRow, 0, parquetRowGroup)
	total := 0
	writeGroup := func() error {
		if len(rows) == 0 {
			return nil
		}
		if _, err := w.Write(rows); err != nil {
			return err
		}
		total += len(rows)
		rows = rows[:0]
		return w.Flush()
	}
	err = a.Index.EachEpochPage(ctx, a.name(), ep, func(p index.Page) error {
		if rows = append(rows, pageRow(p)); len(rows) == parquetRowGroup {
			return writeGroup()
		}
		return nil
	})
	if err == nil {
		err = writeGroup()
	}
	if err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	size, err := out.Commit()
	if err != nil {
		return nil, err
	}
	return &ParquetInfo{Key: ep.ParquetKey(), SHA256: hex.EncodeToString(h.Sum(nil)), Size: size, Rows: total}, nil
}

// WriteMissingParquet adds the page table to sealed epochs packaged before
// page tables existed; it returns the epochs written.
func (a *Archive) WriteMissingParquet(ctx context.Context) ([]epoch.ID, error) {
	cats, err := a.Catalogs(ctx)
	if err != nil {
		return nil, err
	}
	var done []epoch.ID
	for _, c := range cats {
		if c.Parquet != nil {
			continue
		}
		info, err := a.Index.Epoch(ctx, a.name(), c.Epoch)
		if err != nil {
			return done, err
		}
		if info.State != index.StateSealed && info.State != index.StateVerified {
			continue
		}
		if c.Parquet, err = a.writeParquet(ctx, c.Epoch); err != nil {
			return done, fmt.Errorf("archive: page table of %s: %w", c.Epoch, err)
		}
		if err := a.WriteCatalog(ctx, c); err != nil {
			return done, err
		}
		done = append(done, c.Epoch)
	}
	return done, nil
}

// ReadPageTable reads a page table (for verification and tests).
func ReadPageTable(r io.ReaderAt, size int64) ([]PageRow, error) {
	f, err := parquet.OpenFile(r, size)
	if err != nil {
		return nil, err
	}
	rd := parquet.NewGenericReader[PageRow](f)
	defer rd.Close()
	rows := make([]PageRow, f.NumRows())
	n, err := rd.Read(rows)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return rows[:n], nil
}

// verifyParquet checks the hash and the row count of a page table.
func (a *Archive) verifyParquet(ctx context.Context, p *ParquetInfo, add func(p, f string, args ...any)) {
	rd, err := a.Storage.Open(ctx, p.Key)
	if err != nil {
		add(p.Key, "%v", err)
		return
	}
	defer rd.Close()
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(rd, 0, rd.Size())); err != nil {
		add(p.Key, "%v", err)
		return
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != p.SHA256 {
		add(p.Key, "sha256 %s, catalog says %s", sum, p.SHA256)
	}
	f, err := parquet.OpenFile(rd, rd.Size())
	if err != nil {
		add(p.Key, "unreadable: %v", err)
	} else if f.NumRows() != int64(p.Rows) {
		add(p.Key, "%d rows, catalog says %d", f.NumRows(), p.Rows)
	}
}
