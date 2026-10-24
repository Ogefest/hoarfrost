package archive

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"

	"github.com/ogefest/hoarfrost/internal/index"
	"github.com/ogefest/hoarfrost/internal/recorder"
	"github.com/ogefest/hoarfrost/internal/storage"
	"github.com/ogefest/hoarfrost/internal/warcio"
)

// Library is the set of archives (one per configured storage) sharing one
// index. Every storage is a complete, independent archive; reads are routed
// to the storage a record belongs to.
type Library struct {
	Index    *index.DB
	archives map[string]*Archive
	names    []string
}

// NewLibrary creates one Archive per storage; each gets its own spool
// subdirectory.
func NewLibrary(idx *index.DB, storages map[string]storage.Storage, spool string, log *slog.Logger) *Library {
	l := &Library{Index: idx, archives: map[string]*Archive{}}
	for name, st := range storages {
		a := New(st, idx, filepath.Join(spool, name), log)
		a.Name = name
		l.archives[name] = a
		l.names = append(l.names, name)
	}
	sort.Slice(l.names, func(i, j int) bool {
		if l.names[i] == index.DefaultStorage || l.names[j] == index.DefaultStorage {
			return l.names[i] == index.DefaultStorage
		}
		return l.names[i] < l.names[j]
	})
	return l
}

// Single wraps one archive (named after it) as a library.
func Single(a *Archive) *Library {
	return &Library{Index: a.Index, archives: map[string]*Archive{a.name(): a}, names: []string{a.name()}}
}

// Names lists storages, the default one first.
func (l *Library) Names() []string { return append([]string(nil), l.names...) }

// All returns the archives in Names order.
func (l *Library) All() []*Archive {
	out := make([]*Archive, len(l.names))
	for i, n := range l.names {
		out[i] = l.archives[n]
	}
	return out
}

// Archive returns the archive of a storage ("" means the default one).
func (l *Library) Archive(name string) (*Archive, error) {
	if a, ok := l.archives[index.StorageOr(name)]; ok {
		return a, nil
	}
	return nil, fmt.Errorf("archive: unknown storage %q", name)
}

// Has reports whether a storage is configured.
func (l *Library) Has(name string) bool {
	_, ok := l.archives[index.StorageOr(name)]
	return ok
}

// Fetch reads a record from its storage, resolving revisits.
func (l *Library) Fetch(ctx context.Context, r index.Record) (*Resource, error) {
	a, err := l.Archive(r.Storage)
	if err != nil {
		return nil, err
	}
	return a.Fetch(ctx, r)
}

// ReadRecord reads a record from its storage.
func (l *Library) ReadRecord(ctx context.Context, r index.Record) (*warcio.Record, error) {
	a, err := l.Archive(r.Storage)
	if err != nil {
		return nil, err
	}
	return a.ReadRecord(ctx, r)
}

// RawRecord returns the stored bytes of a record.
func (l *Library) RawRecord(ctx context.Context, r index.Record) ([]byte, error) {
	a, err := l.Archive(r.Storage)
	if err != nil {
		return nil, err
	}
	return a.RawRecord(ctx, r)
}

// PageMetadata reads the metadata record of a page.
func (l *Library) PageMetadata(ctx context.Context, p index.Page) (*recorder.PageInfo, error) {
	a, err := l.Archive(p.Storage)
	if err != nil {
		return nil, err
	}
	return a.PageMetadata(ctx, p)
}

// Close releases cached readers of all archives.
func (l *Library) Close() {
	for _, a := range l.archives {
		a.Close()
	}
}
