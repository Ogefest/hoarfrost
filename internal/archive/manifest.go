package archive

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/storage"
)

// ManifestFormat identifies the captures list format.
const ManifestFormat = "hoarfrost-captures-1"

// Manifest lists the capture files packaged into a WACZ package (a volume
// of a week, or the single package of an older week) and where each one is
// inside it. The first line describes the package, each further line one
// capture file. It lets verification prove that every incoming file is in
// the package byte for byte, and lets an interrupted seal resume after the
// package was stored.
type Manifest struct {
	Format    string    `json:"format"`
	Epoch     epoch.ID  `json:"epoch"`
	Package   string    `json:"package"`
	SHA256    string    `json:"sha256"`
	Size      int64     `json:"size"`
	Created   time.Time `json:"created"`
	WARCFiles int       `json:"warcFiles"`
	// Records counts all WARC records, Indexed those in the index.
	Records   int `json:"records"`
	Indexed   int `json:"indexed"`
	Revisits  int `json:"revisits"`
	Pages     int `json:"pages"`
	CDXJLines int `json:"cdxjLines"`
	// CaptureCount is the number of capture lines that follow.
	CaptureCount int               `json:"captures"`
	Captures     []ManifestCapture `json:"-"`
}

// volume describes the package of a captures list in the catalog.
func (m *Manifest) volume(key string) Volume {
	return Volume{Key: m.Package, SHA256: m.SHA256, Size: m.Size, CapturesList: key, Captures: len(m.Captures),
		WARCFiles: m.WARCFiles, Records: m.Indexed, Pages: m.Pages}
}

// ManifestCapture is one packaged capture file.
type ManifestCapture struct {
	// Key is the capture's incoming/ key.
	Key string `json:"key"`
	// File is the WARC file in the package's archive/ directory holding it.
	File string `json:"file"`
	// Offset is the absolute offset of its first byte in the package.
	Offset  int64  `json:"offset"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
	Records int    `json:"records"`
	Indexed int    `json:"indexed"`
	Pages   int    `json:"pages"`
}

// WriteManifest stores the captures list of a package at key.
func (a *Archive) WriteManifest(ctx context.Context, key string, m *Manifest) error {
	m.Format, m.CaptureCount = ManifestFormat, len(m.Captures)
	w, err := storage.Create(ctx, a.Storage, key, int64(len(m.Captures))*300)
	if err != nil {
		return err
	}
	defer w.Abort()
	bw := bufio.NewWriterSize(w, 1<<20)
	enc := json.NewEncoder(bw)
	if err := enc.Encode(m); err != nil {
		return err
	}
	for _, c := range m.Captures {
		if err := enc.Encode(c); err != nil {
			return err
		}
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	_, err = w.Commit()
	return err
}

// ReadManifest loads the captures list stored at key.
func (a *Archive) ReadManifest(ctx context.Context, key string) (*Manifest, error) {
	rc, _, err := storage.Stream(ctx, a.Storage, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	dec := json.NewDecoder(bufio.NewReaderSize(rc, 1<<20))
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("archive: captures list: %w", err)
	}
	if m.Format != ManifestFormat {
		return nil, fmt.Errorf("archive: captures list has format %q", m.Format)
	}
	m.Captures = make([]ManifestCapture, 0, m.CaptureCount)
	for {
		var c ManifestCapture
		if err := dec.Decode(&c); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("archive: captures list line %d: %w", len(m.Captures)+2, err)
		}
		m.Captures = append(m.Captures, c)
	}
	if len(m.Captures) != m.CaptureCount {
		return nil, fmt.Errorf("archive: captures list has %d of %d captures", len(m.Captures), m.CaptureCount)
	}
	return &m, nil
}
