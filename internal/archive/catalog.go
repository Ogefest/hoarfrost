package archive

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/storage"
	"github.com/ogefest/hoarfrost/internal/wacz"
)

// Catalog is the status document of a sealed epoch, stored in catalog/.
type Catalog struct {
	Epoch               epoch.ID      `json:"epoch"`
	State               string        `json:"state"`
	Package             string        `json:"package"`
	SHA256              string        `json:"sha256"`
	Size                int64         `json:"size"`
	SealedAt            time.Time     `json:"sealedAt"`
	Software            string        `json:"software"`
	WACZVersion         string        `json:"waczVersion"`
	Incoming            []string      `json:"incoming"`
	Files               []string      `json:"files"`
	Records             int           `json:"records"`
	Revisits            int           `json:"revisits"`
	Pages               int           `json:"pages"`
	IncomingDeleteAfter time.Time     `json:"incomingDeleteAfter"`
	IncomingDeleted     bool          `json:"incomingDeleted,omitempty"`
	Parquet             *ParquetInfo  `json:"parquet,omitempty"`
	Verification        *Verification `json:"verification,omitempty"`
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
	At       time.Time      `json:"at"`
	OK       bool           `json:"ok"`
	Records  int            `json:"records"`
	Problems []wacz.Problem `json:"problems,omitempty"`
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
