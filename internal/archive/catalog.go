package archive

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/index"
	"github.com/ogefest/hoarfrost/internal/storage"
	"github.com/ogefest/hoarfrost/internal/wacz"
)

// Catalog is the status document of a sealed epoch, stored in catalog/.
type Catalog struct {
	Epoch epoch.ID `json:"epoch"`
	State string   `json:"state"`
	// Package is the single WACZ package of a week sealed before volumes;
	// later weeks list Volumes instead.
	Package string `json:"package,omitempty"`
	// SHA256 is the hash of the package, or of the week's SHA256SUMS file
	// (Sums); Size counts the bytes of the package or of all volumes.
	SHA256      string    `json:"sha256"`
	Size        int64     `json:"size"`
	SealedAt    time.Time `json:"sealedAt"`
	Software    string    `json:"software"`
	WACZVersion string    `json:"waczVersion"`
	// Volumes are the WACZ packages that together hold the week, each with
	// its captures list; Sums is the SHA256SUMS file of the volumes and
	// Collection the multi-WACZ collection listing them.
	Volumes    []Volume `json:"volumes,omitempty"`
	Sums       string   `json:"sums,omitempty"`
	Collection string   `json:"collection,omitempty"`
	// Manifest is the key of the captures list of a single package (see
	// Manifest); Captures and WARCFiles count the capture files and WARC
	// files of all packages.
	Manifest  string `json:"manifest,omitempty"`
	Captures  int    `json:"captures,omitempty"`
	WARCFiles int    `json:"warcFiles,omitempty"`
	// Incoming and Files list incoming keys and WARC files in catalogs
	// written before the captures list existed.
	Incoming            []string      `json:"incoming,omitempty"`
	Files               []string      `json:"files,omitempty"`
	Records             int           `json:"records"`
	Revisits            int           `json:"revisits"`
	Pages               int           `json:"pages"`
	IncomingDeleteAfter time.Time     `json:"incomingDeleteAfter"`
	IncomingDeleted     bool          `json:"incomingDeleted,omitempty"`
	Parquet             *ParquetInfo  `json:"parquet,omitempty"`
	Verification        *Verification `json:"verification,omitempty"`
}

// Volume is one WACZ package of a week.
type Volume struct {
	Key    string `json:"key"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	// CapturesList is the key of the volume's captures list.
	CapturesList string `json:"capturesList,omitempty"`
	Captures     int    `json:"captures"`
	WARCFiles    int    `json:"warcFiles"`
	Records      int    `json:"records"`
	Pages        int    `json:"pages"`
}

// Packages returns the week's WACZ packages: its volumes, or its single
// package.
func (c *Catalog) Packages() []Volume {
	if len(c.Volumes) > 0 || c.Package == "" {
		return c.Volumes
	}
	return []Volume{{Key: c.Package, SHA256: c.SHA256, Size: c.Size, CapturesList: c.Manifest,
		Captures: c.Captures, WARCFiles: c.WARCFiles, Records: c.Records, Pages: c.Pages}}
}

// Location is where the index says the week is stored: the directory of
// its volumes, or its single package.
func (c *Catalog) Location() string {
	if len(c.Volumes) > 0 {
		return c.Epoch.VolumeDir()
	}
	return c.Package
}

// epochInfo is the index state the catalog describes.
func (c *Catalog) epochInfo(storage string) index.EpochInfo {
	info := index.EpochInfo{Storage: storage, Epoch: c.Epoch, State: c.State, WACZKey: c.Location(), SHA256: c.SHA256, Size: c.Size, SealedAt: c.SealedAt}
	if c.Verification != nil {
		info.VerifiedAt = c.Verification.At
		if !c.Verification.OK && len(c.Verification.Problems) > 0 {
			info.Error = c.Verification.Problems[0].Path + ": " + c.Verification.Problems[0].Message
		}
	}
	return info
}

// ParquetInfo describes the page table stored next to the package.
type ParquetInfo struct {
	Key    string `json:"key"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Rows   int    `json:"rows"`
}

// Verification is the outcome of checking a sealed package.
type Verification struct {
	At time.Time `json:"at"`
	OK bool      `json:"ok"`
	// Records counts indexed records found at their place, Scanned all
	// WARC records read, Captures capture files found byte for byte.
	Records  int `json:"records"`
	Scanned  int `json:"scanned,omitempty"`
	Captures int `json:"captures,omitempty"`
	// Problems lists the first problems; MoreProblems counts the rest.
	Problems     []wacz.Problem `json:"problems,omitempty"`
	MoreProblems int            `json:"moreProblems,omitempty"`
}

// ReadCatalog loads the catalog document of an epoch.
func (a *Archive) ReadCatalog(ctx context.Context, ep epoch.ID) (*Catalog, error) {
	b, err := storage.ReadAll(ctx, a.Storage, ep.CatalogKey())
	if err != nil {
		return nil, err
	}
	var c Catalog
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// WriteCatalog stores the catalog document. Catalog files are the only
// archive objects that are rewritten (verification results, cleanup).
func (a *Archive) WriteCatalog(ctx context.Context, c *Catalog) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return storage.PutBytes(ctx, a.Storage, c.Epoch.CatalogKey(), append(b, '\n'))
}

// Catalogs lists catalog documents present in storage.
func (a *Archive) Catalogs(ctx context.Context) ([]*Catalog, error) {
	objs, err := a.Storage.List(ctx, "catalog/")
	if err != nil {
		return nil, err
	}
	var out []*Catalog
	for _, o := range objs {
		if !strings.HasSuffix(o.Key, ".json") {
			continue
		}
		ep, ok := epoch.FromKey(o.Key)
		if !ok {
			continue
		}
		c, err := a.ReadCatalog(ctx, ep)
		if err != nil {
			if errors.Is(err, storage.ErrNotExist) {
				continue
			}
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}
