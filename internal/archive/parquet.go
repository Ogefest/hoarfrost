package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/compress/zstd"

	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/index"
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
	SchemaOrg  string `parquet:"schema_org,optional,json"`
	Source     string `parquet:"source"`
	Kind       string `parquet:"kind"`
	Mime       string `parquet:"mime"`
	Status     int32  `parquet:"status"`
	TextLength int32  `parquet:"text_length"`
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
		SchemaTypes: p.SchemaTypes, SchemaOrg: string(p.SchemaOrg), Source: p.Source, Kind: p.Kind, Mime: p.Mime,
		Status: int32(p.Status), TextLength: int32(p.TextLen), WARCFile: p.File, RecordID: p.RecordID, MetadataID: p.MetadataID,
	}
	if t, err := time.Parse(time.DateOnly, p.Published); err == nil {
		r.Published = t
	}
	return r
}

// writeParquet stores the page table of a sealed epoch from the index.
func (a *Archive) writeParquet(ctx context.Context, ep epoch.ID) (*ParquetInfo, error) {
	pages, err := a.Index.EpochPages(ctx, a.name(), ep)
	if err != nil {
		return nil, err
	}
	rows := make([]PageRow, len(pages))
	for i, p := range pages {
		rows[i] = pageRow(p)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].URL != rows[j].URL {
			return rows[i].URL < rows[j].URL
		}
		return rows[i].Captured.Before(rows[j].Captured)
	})
	if err := os.MkdirAll(a.SpoolDir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(a.SpoolDir, "pages-"+string(ep)+"-*.parquet")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	h := sha256.New()
	w := parquet.NewGenericWriter[PageRow](io.MultiWriter(f, h),
		parquet.Compression(&zstd.Codec{}),
		parquet.MaxRowsPerRowGroup(parquetRowGroup),
		parquet.SortingWriterConfig(parquet.SortingColumns(parquet.Ascending("url"), parquet.Ascending("captured"))),
		parquet.BloomFilters(parquet.SplitBlockFilter(10, "url")),
		parquet.KeyValueMetadata("hoarfrost.epoch", string(ep)),
	)
	for start := 0; start < len(rows); start += parquetRowGroup {
		if _, err := w.Write(rows[start:min(start+parquetRowGroup, len(rows))]); err != nil {
			return nil, err
		}
		if err := w.Flush(); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	size, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if err := a.Storage.Put(ctx, ep.ParquetKey(), f, size); err != nil {
		return nil, err
	}
	return &ParquetInfo{Key: ep.ParquetKey(), SHA256: hex.EncodeToString(h.Sum(nil)), Size: size, Rows: len(rows)}, nil
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
	if err != nil && err != io.EOF {
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
	rows, err := ReadPageTable(rd, rd.Size())
	if err != nil {
		add(p.Key, "unreadable: %v", err)
	} else if len(rows) != p.Rows {
		add(p.Key, "%d rows, catalog says %d", len(rows), p.Rows)
	}
}
