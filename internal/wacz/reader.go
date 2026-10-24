package wacz

import (
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ogefest/hoarfrost/internal/cdxj"
	"github.com/ogefest/hoarfrost/internal/warcio"
)

// Reader gives access to a WACZ package through an io.ReaderAt, so remote
// packages are read with range requests only.
type Reader struct {
	ra     io.ReaderAt
	size   int64
	zr     *zip.Reader
	closer io.Closer
}

// NewReader opens a package of the given size.
func NewReader(ra io.ReaderAt, size int64) (*Reader, error) {
	zr, err := zip.NewReader(ra, size)
	if err != nil {
		return nil, fmt.Errorf("wacz: %w", err)
	}
	return &Reader{ra: ra, size: size, zr: zr}, nil
}

// OpenFile opens a local package.
func OpenFile(path string) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	r, err := NewReader(f, st.Size())
	if err != nil {
		f.Close()
		return nil, err
	}
	r.closer = f
	return r, nil
}

// Close releases the underlying file when opened with OpenFile.
func (r *Reader) Close() error {
	if r.closer != nil {
		return r.closer.Close()
	}
	return nil
}

// Files returns the zip entries.
func (r *Reader) Files() []*zip.File { return r.zr.File }

func (r *Reader) file(name string) (*zip.File, error) {
	for _, f := range r.zr.File {
		if f.Name == name {
			return f, nil
		}
	}
	return nil, fmt.Errorf("wacz: entry %s not found", name)
}

func (r *Reader) has(name string) bool {
	_, err := r.file(name)
	return err == nil
}

// ReadEntry returns the content of an entry.
func (r *Reader) ReadEntry(name string) ([]byte, error) {
	f, err := r.file(name)
	if err != nil {
		return nil, err
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// Entry returns a section reader over a stored entry's data.
func (r *Reader) Entry(name string) (*io.SectionReader, error) {
	f, err := r.file(name)
	if err != nil {
		return nil, err
	}
	if f.Method != zip.Store {
		return nil, fmt.Errorf("wacz: entry %s is compressed", name)
	}
	off, err := f.DataOffset()
	if err != nil {
		return nil, err
	}
	return io.NewSectionReader(r.ra, off, int64(f.UncompressedSize64)), nil
}

// WARCOffsets maps archive file names to the absolute offset of their data.
func (r *Reader) WARCOffsets() (map[string]int64, error) {
	out := map[string]int64{}
	for _, f := range r.zr.File {
		if !strings.HasPrefix(f.Name, ArchiveDir) {
			continue
		}
		off, err := f.DataOffset()
		if err != nil {
			return nil, err
		}
		out[strings.TrimPrefix(f.Name, ArchiveDir)] = off
	}
	return out, nil
}

// Datapackage parses datapackage.json.
func (r *Reader) Datapackage() (Datapackage, error) {
	var dp Datapackage
	b, err := r.ReadEntry(PathDatapackage)
	if err != nil {
		return dp, err
	}
	err = json.Unmarshal(b, &dp)
	return dp, err
}

// CDXJ returns the index lines from either the plain or the ZipNum index.
func (r *Reader) CDXJ() ([]cdxj.Line, error) {
	if b, err := r.ReadEntry(PathCDXJ); err == nil {
		return cdxj.ReadAll(bytes.NewReader(b))
	}
	b, err := r.ReadEntry(PathZipNumCDX)
	if err != nil {
		return nil, errors.New("wacz: no index in package")
	}
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	return cdxj.ReadAll(gz)
}

// Pages parses pages/pages.jsonl, skipping the header line. Packages made
// by other tools may omit the file; they yield no pages.
func (r *Reader) Pages() ([]PageLine, error) {
	if !r.has(PathPages) {
		return nil, nil
	}
	b, err := r.ReadEntry(PathPages)
	if err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	var out []PageLine
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(line, &probe); err != nil {
			return nil, err
		}
		if _, isHeader := probe["format"]; isHeader {
			continue
		}
		var p PageLine
		if err := json.Unmarshal(line, &p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, sc.Err()
}

// ReadRecord reads one record at an absolute offset.
func (r *Reader) ReadRecord(offset, length int64) (*warcio.Record, error) {
	return warcio.ReadRecordAt(r.ra, offset, length)
}

// Problem is a verification failure.
type Problem struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// Verify checks that every entry is stored, that datapackage.json lists
// every resource with a matching SHA-256 and size, and that the digest file
// matches datapackage.json.
func (r *Reader) Verify() []Problem {
	var probs []Problem
	add := func(p, f string, args ...any) { probs = append(probs, Problem{p, fmt.Sprintf(f, args...)}) }
	for _, f := range r.zr.File {
		if f.Method != zip.Store {
			add(f.Name, "entry is compressed (method %d), must be stored", f.Method)
		}
	}
	dpRaw, err := r.ReadEntry(PathDatapackage)
	if err != nil {
		add(PathDatapackage, "%v", err)
		return probs
	}
	var dp Datapackage
	if err := json.Unmarshal(dpRaw, &dp); err != nil {
		add(PathDatapackage, "invalid JSON: %v", err)
		return probs
	}
	if dp.WACZVersion == "" {
		add(PathDatapackage, "missing wacz_version")
	}
	listed := map[string]bool{PathDatapackage: true, PathDigest: true}
	for _, res := range dp.Resources {
		listed[res.Path] = true
		sec, err := r.Entry(res.Path)
		if err != nil {
			add(res.Path, "%v", err)
			continue
		}
		h := sha256.New()
		n, err := io.Copy(h, sec)
		if err != nil {
			add(res.Path, "read: %v", err)
			continue
		}
		if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != res.Hash {
			add(res.Path, "hash mismatch: %s, datapackage says %s", got, res.Hash)
		}
		if n != res.Bytes {
			add(res.Path, "size %d, datapackage says %d", n, res.Bytes)
		}
	}
	for _, f := range r.zr.File {
		if !listed[f.Name] {
			add(f.Name, "entry not listed in datapackage.json")
		}
	}
	ddRaw, err := r.ReadEntry(PathDigest)
	if err != nil {
		add(PathDigest, "%v", err)
		return probs
	}
	var dd DatapackageDigest
	if err := json.Unmarshal(ddRaw, &dd); err != nil {
		add(PathDigest, "invalid JSON: %v", err)
		return probs
	}
	sum := sha256.Sum256(dpRaw)
	if dd.Hash != "sha256:"+hex.EncodeToString(sum[:]) {
		add(PathDigest, "datapackage.json hash mismatch")
	}
	return probs
}
