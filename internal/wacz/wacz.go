// Package wacz builds and reads WACZ 1.1.1 packages. Every entry is stored
// uncompressed so records can be read with a single range request at an
// absolute offset.
package wacz

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/ogefest/hoarfrost/internal/cdxj"
	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/recorder"
)

// Version is the WACZ specification version produced.
const Version = "1.1.1"

// Entry paths.
const (
	PathCDXJ         = "indexes/index.cdxj"
	PathZipNumIdx    = "indexes/index.idx"
	PathZipNumCDX    = "indexes/index.cdx.gz"
	PathPages        = "pages/pages.jsonl"
	PathDatapackage  = "datapackage.json"
	PathDigest       = "datapackage-digest.json"
	ArchiveDir       = "archive/"
	DefaultZipNumMin = 200_000
	zipNumBlockLines = 3000
)

// Input is one capture file to package.
type Input struct {
	Capture recorder.CaptureFile
	// Open returns the WARC content of the capture file.
	Open func(ctx context.Context) (io.ReadCloser, error)
}

// Options configure Build.
type Options struct {
	Epoch    epoch.ID
	Title    string
	Software string
	Created  time.Time
	// ConcatBySource joins captures of the same source into one WARC file.
	ConcatBySource bool
	// ZipNumThreshold switches to a block index above this many CDXJ lines
	// (0 means DefaultZipNumMin, negative disables).
	ZipNumThreshold int
}

// RecordLocation is the absolute position of a record in the package.
type RecordLocation struct {
	Offset int64
	Length int64
}

// Result describes a built package.
type Result struct {
	Path   string
	Size   int64
	SHA256 string
	// Records holds positions of indexable and metadata records.
	Records   map[string]RecordLocation
	Files     []string
	Pages     int
	CDXJLines int
	ZipNum    bool
}

// Resource is a datapackage.json resource entry.
type Resource struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Hash  string `json:"hash"`
	Bytes int64  `json:"bytes"`
}

// Datapackage is the content of datapackage.json.
type Datapackage struct {
	Profile     string     `json:"profile"`
	WACZVersion string     `json:"wacz_version"`
	Title       string     `json:"title,omitempty"`
	Created     string     `json:"created"`
	Modified    string     `json:"modified,omitempty"`
	Software    string     `json:"software"`
	MainPageURL string     `json:"mainPageUrl,omitempty"`
	Resources   []Resource `json:"resources"`
	Epoch       string     `json:"epoch,omitempty"`
}

// DatapackageDigest is the content of datapackage-digest.json.
type DatapackageDigest struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
}

// PageLine is one line of pages.jsonl. MetaOffset and MetaLength locate the
// page's metadata record within Filename.
type PageLine struct {
	recorder.PageInfo
	MetadataID string `json:"metadataId,omitempty"`
	Filename   string `json:"filename,omitempty"`
	MetaOffset int64  `json:"metaOffset,omitempty"`
	MetaLength int64  `json:"metaLength,omitempty"`
}

type part struct {
	name   string
	inputs []Input
}

func plan(inputs []Input, concat bool) []part {
	sorted := append([]Input(nil), inputs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Capture.Name < sorted[j].Capture.Name })
	if !concat {
		out := make([]part, len(sorted))
		for i, in := range sorted {
			out[i] = part{name: in.Capture.Name, inputs: []Input{in}}
		}
		return out
	}
	bySource := map[string]*part{}
	var order []string
	for _, in := range sorted {
		src := SourceFromName(in.Capture.Name)
		p, ok := bySource[src]
		if !ok {
			p = &part{name: src + ".warc.gz"}
			bySource[src] = p
			order = append(order, src)
		}
		p.inputs = append(p.inputs, in)
	}
	sort.Strings(order)
	out := make([]part, len(order))
	for i, s := range order {
		out[i] = *bySource[s]
	}
	return out
}

// SourceFromName extracts the source from "<ts>-<source>-<id>.warc.gz".
func SourceFromName(name string) string {
	base := strings.TrimSuffix(name, ".warc.gz")
	first := strings.IndexByte(base, '-')
	last := strings.LastIndexByte(base, '-')
	if first < 0 || last <= first {
		return "capture"
	}
	return base[first+1 : last]
}

type hashingWriter struct {
	w io.Writer
	h hash.Hash
	n int64
}

func (hw *hashingWriter) Write(p []byte) (int, error) {
	n, err := hw.w.Write(p)
	hw.h.Write(p[:n])
	hw.n += int64(n)
	return n, err
}

// Build writes a WACZ package to path.
func Build(ctx context.Context, outPath string, inputs []Input, opts Options) (*Result, error) {
	if len(inputs) == 0 {
		return nil, errors.New("wacz: nothing to package")
	}
	if opts.Created.IsZero() {
		opts.Created = time.Now()
	}
	if opts.ZipNumThreshold == 0 {
		opts.ZipNumThreshold = DefaultZipNumMin
	}
	f, err := os.Create(outPath)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(outPath)
		}
	}()
	zw := zip.NewWriter(f)
	var resources []Resource
	addEntry := func(name string, write func(w io.Writer) error) error {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store, Modified: opts.Created.UTC()})
		if err != nil {
			return err
		}
		hw := &hashingWriter{w: w, h: sha256.New()}
		if err := write(hw); err != nil {
			return err
		}
		resources = append(resources, Resource{
			Name: strings.ToLower(path.Base(name)), Path: name, Hash: "sha256:" + hex.EncodeToString(hw.h.Sum(nil)), Bytes: hw.n,
		})
		return nil
	}

	type shift struct {
		file  string
		delta int64
	}
	shifts := map[string]shift{}
	var lines []cdxj.Line
	var pages []PageLine
	res := &Result{Path: outPath, Records: map[string]RecordLocation{}}
	for _, p := range plan(inputs, opts.ConcatBySource) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		res.Files = append(res.Files, p.name)
		err := addEntry(ArchiveDir+p.name, func(w io.Writer) error {
			var written int64
			for _, in := range p.inputs {
				rc, err := in.Open(ctx)
				if err != nil {
					return fmt.Errorf("wacz: open %s: %w", in.Capture.Name, err)
				}
				n, err := io.Copy(w, rc)
				rc.Close()
				if err != nil {
					return fmt.Errorf("wacz: copy %s: %w", in.Capture.Name, err)
				}
				if n != in.Capture.Size {
					return fmt.Errorf("wacz: %s has %d bytes, index says %d", in.Capture.Name, n, in.Capture.Size)
				}
				shifts[in.Capture.Name] = shift{file: p.name, delta: written}
				for _, r := range in.Capture.Records {
					if !r.Indexable() {
						continue
					}
					r.Offset += written
					lines = append(lines, r.CDXJ(p.name))
				}
				for _, pg := range in.Capture.Pages {
					pages = append(pages, PageLine{PageInfo: pg.PageInfo, MetadataID: pg.MetadataID, Filename: p.name,
						MetaOffset: pg.Offset + written, MetaLength: pg.Length})
				}
				written += n
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	res.CDXJLines = len(lines)
	if opts.ZipNumThreshold > 0 && len(lines) > opts.ZipNumThreshold {
		res.ZipNum = true
		idx, blocks, err := zipNum(lines)
		if err != nil {
			return nil, err
		}
		if err := addEntry(PathZipNumCDX, func(w io.Writer) error { _, err := w.Write(blocks); return err }); err != nil {
			return nil, err
		}
		if err := addEntry(PathZipNumIdx, func(w io.Writer) error { _, err := w.Write(idx); return err }); err != nil {
			return nil, err
		}
	} else if err := addEntry(PathCDXJ, func(w io.Writer) error { return cdxj.Write(w, lines) }); err != nil {
		return nil, err
	}

	sort.SliceStable(pages, func(i, j int) bool { return pages[i].Captured.Before(pages[j].Captured) })
	res.Pages = len(pages)
	if err := addEntry(PathPages, func(w io.Writer) error { return writePages(w, pages) }); err != nil {
		return nil, err
	}

	dp := Datapackage{
		Profile: "data-package", WACZVersion: Version, Title: opts.Title,
		Created: opts.Created.UTC().Format(time.RFC3339), Software: opts.Software,
		Resources: resources, Epoch: string(opts.Epoch),
	}
	dpBytes, _ := json.MarshalIndent(dp, "", "  ")
	sum := sha256.Sum256(dpBytes)
	if err := writeRaw(zw, PathDatapackage, dpBytes, opts.Created); err != nil {
		return nil, err
	}
	dd, _ := json.MarshalIndent(DatapackageDigest{Path: PathDatapackage, Hash: "sha256:" + hex.EncodeToString(sum[:])}, "", "  ")
	if err := writeRaw(zw, PathDigest, dd, opts.Created); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	if err := f.Sync(); err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	ok = true

	r, err := OpenFile(outPath)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	dataOffsets, err := r.WARCOffsets()
	if err != nil {
		return nil, err
	}
	for _, in := range inputs {
		sh := shifts[in.Capture.Name]
		base, ok := dataOffsets[sh.file]
		if !ok {
			return nil, fmt.Errorf("wacz: %s missing from package", sh.file)
		}
		for _, rec := range in.Capture.Records {
			if rec.Indexable() || rec.Type == "metadata" {
				res.Records[rec.ID] = RecordLocation{Offset: base + sh.delta + rec.Offset, Length: rec.Length}
			}
		}
	}
	st, err := os.Stat(outPath)
	if err != nil {
		return nil, err
	}
	res.Size = st.Size()
	if res.SHA256, err = FileSHA256(outPath); err != nil {
		return nil, err
	}
	return res, nil
}

func writeRaw(zw *zip.Writer, name string, b []byte, mod time.Time) error {
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store, Modified: mod.UTC()})
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

func writePages(w io.Writer, pages []PageLine) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(map[string]string{"format": "json-pages-1.0", "id": "pages", "title": "All Pages"}); err != nil {
		return err
	}
	for _, p := range pages {
		if err := enc.Encode(p); err != nil {
			return err
		}
	}
	return nil
}

// zipNum builds a ZipNum block index: sorted CDXJ lines gzipped in blocks
// plus an index with the first key of each block.
func zipNum(entries []cdxj.Line) (idx, blocks []byte, err error) {
	lines := make([]string, len(entries))
	for i, e := range entries {
		lines[i] = e.String()
	}
	cdxj.Sort(lines)
	var out, ib bytes.Buffer
	ib.WriteString(`!meta 0 {"format":"cdxj-gzip-1.0","filename":"index.cdx.gz"}` + "\n")
	for start := 0; start < len(lines); start += zipNumBlockLines {
		end := min(start+zipNumBlockLines, len(lines))
		offset := out.Len()
		gz := gzip.NewWriter(&out)
		for _, l := range lines[start:end] {
			if _, err := gz.Write([]byte(l + "\n")); err != nil {
				return nil, nil, err
			}
		}
		if err := gz.Close(); err != nil {
			return nil, nil, err
		}
		key, rest, _ := strings.Cut(lines[start], " ")
		tsPart, _, _ := strings.Cut(rest, " ")
		js, _ := json.Marshal(map[string]any{"offset": offset, "length": out.Len() - offset, "lineno": start/zipNumBlockLines + 1})
		ib.WriteString(key + " " + tsPart + " " + string(js) + "\n")
	}
	return ib.Bytes(), out.Bytes(), nil
}

// FileSHA256 hashes a local file.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ChecksumLine formats a sha256sum-compatible line.
func ChecksumLine(sum, name string) string { return sum + "  " + name + "\n" }
