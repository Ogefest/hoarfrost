// Package wacz builds and reads WACZ 1.1.1 packages. Every entry is stored
// uncompressed so records can be read with a single range request at an
// absolute offset.
package wacz

import (
	"archive/zip"
	"bufio"
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
	// Name is the capture file name, <ts>-<source>-<id>.warc.gz.
	Name string
	// Size is the size the file must have.
	Size int64
	// Open returns the WARC content of the capture file.
	Open func(ctx context.Context) (io.ReadCloser, error)
}

// Options configure Build.
type Options struct {
	Epoch    epoch.ID
	Title    string
	Software string
	Created  time.Time
	// ConcatBySource joins captures of the same source into one WARC file,
	// named <source><FileSuffix>.warc.gz.
	ConcatBySource bool
	// FileSuffix keeps the names of joined WARC files unique across the
	// volumes of a week (e.g. "-0003").
	FileSuffix string
	// ZipNumThreshold switches to a block index above this many CDXJ lines
	// (0 means DefaultZipNumMin, negative disables).
	ZipNumThreshold int
	// SpoolDir holds temporary sort files of the index and the pages list
	// (os.TempDir() when empty); they need a small fraction of the package
	// size.
	SpoolDir string
	// SortMemory bounds the index and page lines kept in memory
	// (DefaultSortMemory when 0).
	SortMemory int
	// OnCapture is called after each capture file is written, with its
	// description read from the copied bytes (record offsets relative to
	// the capture file) and its place in the package.
	OnCapture func(cf recorder.CaptureFile, p Placement) error
	// Progress is called after each capture file with the bytes of capture
	// files written so far.
	Progress func(done int64)
}

// Placement is where a capture file was written in the package.
type Placement struct {
	// Name is the capture file name.
	Name string
	// File is the WARC file in archive/ that holds it.
	File string
	// Offset is the absolute offset of its first byte in the package.
	Offset int64
	Size   int64
	// SHA256 is the hex digest of its bytes.
	SHA256 string
}

// Result describes a built package.
type Result struct {
	Size   int64
	SHA256 string
	// Files are the WARC files in archive/.
	Files     []string
	Captures  int
	Records   int
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

// Order returns inputs in the order Build writes them, so that a week can
// be split into volumes along it.
func Order(inputs []Input, concat bool) []Input {
	var out []Input
	for _, p := range plan(inputs, concat, "") {
		out = append(out, p.inputs...)
	}
	return out
}

func plan(inputs []Input, concat bool, suffix string) []part {
	sorted := append([]Input(nil), inputs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	if !concat {
		out := make([]part, len(sorted))
		for i, in := range sorted {
			out[i] = part{name: in.Name, inputs: []Input{in}}
		}
		return out
	}
	bySource := map[string]*part{}
	var order []string
	for _, in := range sorted {
		src := SourceFromName(in.Name)
		p, ok := bySource[src]
		if !ok {
			p = &part{name: src + suffix + ".warc.gz"}
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

// Build streams a WACZ package to w. Every capture file is read once: its
// bytes are copied into the package and scanned for the index on the way.
// Memory use does not grow with the number of records; index and page
// lines are sorted through temporary files in opts.SpoolDir.
func Build(ctx context.Context, w io.Writer, inputs []Input, opts Options) (*Result, error) {
	if len(inputs) == 0 {
		return nil, errors.New("wacz: nothing to package")
	}
	if opts.Created.IsZero() {
		opts.Created = time.Now()
	}
	if opts.ZipNumThreshold == 0 {
		opts.ZipNumThreshold = DefaultZipNumMin
	}
	if opts.SortMemory == 0 {
		opts.SortMemory = DefaultSortMemory
	}
	out := &hashingWriter{w: w, h: sha256.New()}
	zw := zip.NewWriter(out)
	cdx := newLineSorter(opts.SpoolDir, opts.SortMemory/2)
	defer cdx.close()
	pages := newLineSorter(opts.SpoolDir, opts.SortMemory/2)
	defer pages.close()

	var resources []Resource
	addEntry := func(name string, write func(w io.Writer, start int64) error) error {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store, Modified: opts.Created.UTC()})
		if err != nil {
			return err
		}
		// The local header is written; what follows is the entry's data.
		if err := zw.Flush(); err != nil {
			return err
		}
		hw := &hashingWriter{w: w, h: sha256.New()}
		if err := write(hw, out.n); err != nil {
			return err
		}
		resources = append(resources, Resource{
			Name: strings.ToLower(path.Base(name)), Path: name, Hash: "sha256:" + hex.EncodeToString(hw.h.Sum(nil)), Bytes: hw.n,
		})
		return nil
	}

	res := &Result{}
	var done int64
	for _, p := range plan(inputs, opts.ConcatBySource, opts.FileSuffix) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		res.Files = append(res.Files, p.name)
		err := addEntry(ArchiveDir+p.name, func(w io.Writer, start int64) error {
			var written int64
			for _, in := range p.inputs {
				cf, sum, err := copyCapture(ctx, w, in, opts.Epoch)
				if err != nil {
					return err
				}
				for _, r := range cf.Records {
					res.Records++
					if !r.Indexable() {
						continue
					}
					r.Offset += written
					if err := cdx.add(r.CDXJ(p.name).String()); err != nil {
						return err
					}
				}
				for _, pg := range cf.Pages {
					line, err := pageSortLine(PageLine{PageInfo: pg.PageInfo, MetadataID: pg.MetadataID, Filename: p.name,
						MetaOffset: pg.Offset + written, MetaLength: pg.Length}, pages.n)
					if err != nil {
						return err
					}
					if err := pages.add(line); err != nil {
						return err
					}
				}
				res.Captures++
				if opts.OnCapture != nil {
					if err := opts.OnCapture(cf, Placement{Name: in.Name, File: p.name, Offset: start + written, Size: cf.Size, SHA256: sum}); err != nil {
						return err
					}
				}
				written += cf.Size
				done += cf.Size
				if opts.Progress != nil {
					opts.Progress(done)
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	res.CDXJLines = cdx.n
	if opts.ZipNumThreshold > 0 && cdx.n > opts.ZipNumThreshold {
		res.ZipNum = true
		var idx bytes.Buffer
		if err := addEntry(PathZipNumCDX, func(w io.Writer, _ int64) error { return writeZipNum(w, cdx, &idx) }); err != nil {
			return nil, err
		}
		if err := addEntry(PathZipNumIdx, func(w io.Writer, _ int64) error { _, err := w.Write(idx.Bytes()); return err }); err != nil {
			return nil, err
		}
	} else if err := addEntry(PathCDXJ, func(w io.Writer, _ int64) error { return writeLines(w, cdx, "", false) }); err != nil {
		return nil, err
	}

	res.Pages = pages.n
	if err := addEntry(PathPages, func(w io.Writer, _ int64) error { return writeLines(w, pages, pagesHeader, true) }); err != nil {
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
	res.Size, res.SHA256 = out.n, hex.EncodeToString(out.h.Sum(nil))
	return res, nil
}

// copyCapture copies a capture file to w and describes it from the copied
// bytes. It checks that the file has the expected size and ends with its
// last record.
func copyCapture(ctx context.Context, w io.Writer, in Input, ep epoch.ID) (recorder.CaptureFile, string, error) {
	rc, err := in.Open(ctx)
	if err != nil {
		return recorder.CaptureFile{}, "", fmt.Errorf("wacz: open %s: %w", in.Name, err)
	}
	defer rc.Close()
	hw := &hashingWriter{w: w, h: sha256.New()}
	tee := io.TeeReader(rc, hw)
	cf, err := recorder.Scan(tee, in.Name, ep)
	if err != nil {
		return cf, "", fmt.Errorf("wacz: %s: %w", in.Name, err)
	}
	// A valid file is read to its end by the scan; copy anything left.
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return cf, "", fmt.Errorf("wacz: copy %s: %w", in.Name, err)
	}
	if hw.n != in.Size {
		return cf, "", fmt.Errorf("wacz: %s has %d bytes, expected %d", in.Name, hw.n, in.Size)
	}
	if cf.Size != hw.n {
		return cf, "", fmt.Errorf("wacz: %s: records end at byte %d of %d", in.Name, cf.Size, hw.n)
	}
	return cf, hex.EncodeToString(hw.h.Sum(nil)), nil
}

func writeRaw(zw *zip.Writer, name string, b []byte, mod time.Time) error {
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store, Modified: mod.UTC()})
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

const pagesHeader = `{"format":"json-pages-1.0","id":"pages","title":"All Pages"}`

// pageSortLine makes a sortable line of a page: capture time and sequence
// number (keeping the input order of pages captured at the same time),
// then the JSON line after a NUL byte, which JSON never contains.
func pageSortLine(p PageLine, seq int) (string, error) {
	var b bytes.Buffer
	b.WriteString(p.Captured.UTC().Format("20060102T150405.000000000"))
	fmt.Fprintf(&b, "%012d\x00", seq)
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(p); err != nil {
		return "", err
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// writeLines writes the sorted lines after an optional header line;
// keyed lines lose their sort key (up to the NUL byte).
func writeLines(w io.Writer, s *lineSorter, header string, keyed bool) error {
	bw := bufio.NewWriterSize(w, 1<<20)
	if header != "" {
		bw.WriteString(header + "\n")
	}
	err := s.each(func(l string) error {
		if keyed {
			_, l, _ = strings.Cut(l, "\x00")
		}
		bw.WriteString(l)
		return bw.WriteByte('\n')
	})
	if err != nil {
		return err
	}
	return bw.Flush()
}

// writeZipNum writes sorted CDXJ lines gzipped in blocks and collects the
// ZipNum index with the first key of each block in idx.
func writeZipNum(w io.Writer, s *lineSorter, idx *bytes.Buffer) error {
	idx.WriteString(`!meta 0 {"format":"cdxj-gzip-1.0","filename":"index.cdx.gz"}` + "\n")
	cw := &hashingWriter{w: w, h: sha256.New()}
	var gz *gzip.Writer
	var first string
	var start int64
	inBlock, block := 0, 0
	closeBlock := func() error {
		if err := gz.Close(); err != nil {
			return err
		}
		block++
		key, rest, _ := strings.Cut(first, " ")
		tsPart, _, _ := strings.Cut(rest, " ")
		js, _ := json.Marshal(map[string]any{"offset": start, "length": cw.n - start, "lineno": block})
		idx.WriteString(key + " " + tsPart + " " + string(js) + "\n")
		inBlock = 0
		return nil
	}
	err := s.each(func(l string) error {
		if inBlock == 0 {
			first, start = l, cw.n
			if gz == nil {
				gz = gzip.NewWriter(cw)
			} else {
				gz.Reset(cw)
			}
		}
		if _, err := io.WriteString(gz, l+"\n"); err != nil {
			return err
		}
		if inBlock++; inBlock == zipNumBlockLines {
			return closeBlock()
		}
		return nil
	})
	if err == nil && inBlock > 0 {
		err = closeBlock()
	}
	return err
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
