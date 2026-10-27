package archive

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/index"
	"github.com/ogefest/hoarfrost/internal/recorder"
	"github.com/ogefest/hoarfrost/internal/storage"
	"github.com/ogefest/hoarfrost/internal/wacz"
)

// ReindexOptions select what to rebuild.
type ReindexOptions struct {
	// Epoch limits the rebuild to one epoch.
	Epoch epoch.ID
	// Force rebuilds epochs even when the index looks consistent.
	Force bool
	// Progress is called after each epoch.
	Progress func(ep epoch.ID, done, total int)
}

// ReindexReport summarizes a rebuild.
type ReindexReport struct {
	Epochs   []epoch.ID
	Captures int
	Scanned  []epoch.ID // sealed epochs rebuilt by scanning WARC data
	// Missing are sealed epochs with a package missing from storage; they
	// are marked failed, not rebuilt.
	Missing []epoch.ID
}

// Reindex rebuilds missing or inconsistent parts of the index from storage.
// Sealed epochs are read from the package index and pages list; open epochs
// from incoming files; damaged package indexes fall back to a WARC scan.
func (a *Archive) Reindex(ctx context.Context, opts ReindexOptions) (*ReindexReport, error) {
	cats, err := a.Catalogs(ctx)
	if err != nil {
		return nil, err
	}
	sealed := map[epoch.ID]*Catalog{}
	rep := &ReindexReport{}
	for _, c := range cats {
		missing, err := a.missingPackage(ctx, c)
		if err != nil {
			return nil, err
		}
		if missing == "" {
			sealed[c.Epoch] = c
			continue
		}
		// A sealed week whose package is gone is not rebuilt from incoming/
		// as if it were open: it is reported as failed.
		a.Log.Error("package of a sealed week missing from storage", "epoch", c.Epoch, "package", missing)
		rep.Missing = append(rep.Missing, c.Epoch)
		info := c.epochInfo(a.name())
		info.State, info.Error = index.StateFailed, missing+": missing from storage"
		if err := a.Index.SetEpoch(ctx, info); err != nil {
			return nil, err
		}
	}
	missingWeek := map[epoch.ID]bool{}
	for _, ep := range rep.Missing {
		missingWeek[ep] = true
	}
	incoming, err := a.Storage.List(ctx, "incoming/")
	if err != nil {
		return nil, err
	}
	open := map[epoch.ID][]string{}
	for _, o := range incoming {
		ep, ok := epoch.FromKey(o.Key)
		if !ok || !strings.HasSuffix(o.Key, ".warc.gz") || sealed[ep] != nil || missingWeek[ep] {
			continue
		}
		open[ep] = append(open[ep], o.Key)
	}

	var eps []epoch.ID
	for ep := range sealed {
		eps = append(eps, ep)
	}
	for ep := range open {
		eps = append(eps, ep)
	}
	sort.Slice(eps, func(i, j int) bool { return eps[i] < eps[j] })
	if opts.Epoch != "" {
		eps = filterEpoch(eps, opts.Epoch)
	}

	for i, ep := range eps {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		if c := sealed[ep]; c != nil {
			info, err := a.Index.Epoch(ctx, a.name(), ep)
			if err != nil {
				return rep, err
			}
			if opts.Force || info.SHA256 != c.SHA256 || info.WACZKey != c.Location() {
				scanned, err := a.reindexSealed(ctx, c)
				if err != nil {
					return rep, fmt.Errorf("archive: reindex %s: %w", ep, err)
				}
				rep.Epochs = append(rep.Epochs, ep)
				if scanned {
					rep.Scanned = append(rep.Scanned, ep)
				}
			}
		} else {
			n, err := a.reindexOpen(ctx, ep, open[ep], opts.Force)
			if err != nil {
				return rep, fmt.Errorf("archive: reindex %s: %w", ep, err)
			}
			if n > 0 {
				rep.Epochs = append(rep.Epochs, ep)
				rep.Captures += n
			}
		}
		if opts.Progress != nil {
			opts.Progress(ep, i+1, len(eps))
		}
	}
	return rep, nil
}

func filterEpoch(eps []epoch.ID, want epoch.ID) []epoch.ID {
	for _, ep := range eps {
		if ep == want {
			return []epoch.ID{ep}
		}
	}
	return nil
}

func (a *Archive) reindexOpen(ctx context.Context, ep epoch.ID, keys []string, force bool) (int, error) {
	if force {
		if err := a.Index.DeleteEpoch(ctx, a.name(), ep); err != nil {
			return 0, err
		}
	}
	known, err := a.Index.Files(ctx, a.name(), ep)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, key := range keys {
		if _, ok := known[key]; ok {
			continue
		}
		r, err := a.Storage.Open(ctx, key)
		if err != nil {
			return n, err
		}
		cf, err := recorder.Scan(bufio.NewReaderSize(io.NewSectionReader(r, 0, r.Size()), 1<<20), path.Base(key), ep)
		r.Close()
		if err != nil {
			return n, fmt.Errorf("scan %s: %w", key, err)
		}
		cf.Storage = a.name()
		if err := a.Index.AddCapture(ctx, cf, key); err != nil {
			return n, err
		}
		n++
	}
	if n > 0 {
		// Files are read in name order, which may put a revisit before
		// the capture holding its original.
		if err := a.Index.RecomputeStats(ctx, a.name(), ep); err != nil {
			return n, err
		}
	}
	return n, nil
}

// missingPackage returns a package of a sealed week that is not in
// storage, or "" when all are.
func (a *Archive) missingPackage(ctx context.Context, c *Catalog) (string, error) {
	pkgs := c.Packages()
	if len(pkgs) == 0 {
		return c.Epoch.CatalogKey() + " lists no package", nil
	}
	for _, p := range pkgs {
		if _, err := a.Storage.Stat(ctx, p.Key); errors.Is(err, storage.ErrNotExist) {
			return p.Key, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", nil
}

// reindexSealed rebuilds the index of a sealed week package by package,
// so memory holds the records of one package at a time. The week's state
// is set last: an interrupted rebuild is repeated on the next run.
func (a *Archive) reindexSealed(ctx context.Context, c *Catalog) (scanned bool, err error) {
	if err := a.Index.DeleteEpoch(ctx, a.name(), c.Epoch); err != nil {
		return false, err
	}
	for _, p := range c.Packages() {
		s, err := a.reindexPackage(ctx, c.Epoch, p.Key)
		scanned = scanned || s
		if err != nil {
			return scanned, fmt.Errorf("%s: %w", p.Key, err)
		}
	}
	return scanned, a.Index.SetEpoch(ctx, c.epochInfo(a.name()))
}

func (a *Archive) reindexPackage(ctx context.Context, ep epoch.ID, key string) (scanned bool, err error) {
	rd, err := a.Storage.Open(ctx, key)
	if err != nil {
		return false, err
	}
	defer rd.Close()
	pkg, err := wacz.NewReader(rd, rd.Size())
	if err != nil {
		return false, err
	}
	recs, pages, err := fromPackageIndex(pkg, ep, key, a.name())
	if err != nil {
		a.Log.Warn("package index unusable, scanning WARC data", "epoch", ep, "package", key, "err", err)
		scanned = true
		if recs, pages, err = fromPackageScan(pkg, ep, key, a.name()); err != nil {
			return true, err
		}
	}
	return scanned, a.Index.AddRecords(ctx, recs, pages)
}

var errNoRecordIDs = errors.New("index lacks record ids")

func fromPackageIndex(pkg *wacz.Reader, ep epoch.ID, key, storageName string) ([]index.Record, []index.PageRow, error) {
	lines, err := pkg.CDXJ()
	if err != nil {
		return nil, nil, err
	}
	offsets, err := pkg.WARCOffsets()
	if err != nil {
		return nil, nil, err
	}
	pageLines, err := pkg.Pages()
	if err != nil {
		return nil, nil, err
	}
	if len(pageLines) == 0 {
		return nil, nil, errors.New("package has no pages list")
	}
	recs := make([]index.Record, 0, len(lines))
	for _, l := range lines {
		base, ok := offsets[l.Filename]
		if !ok {
			return nil, nil, fmt.Errorf("cdxj references unknown file %s", l.Filename)
		}
		if l.RecordID == "" || l.Type == "" {
			return nil, nil, errNoRecordIDs
		}
		off, err1 := strconv.ParseInt(l.Offset, 10, 64)
		n, err2 := strconv.ParseInt(l.Length, 10, 64)
		if err1 != nil || err2 != nil {
			return nil, nil, fmt.Errorf("bad offset in cdxj for %s", l.URL)
		}
		status, _ := strconv.Atoi(l.Status)
		recs = append(recs, index.Record{
			ID: l.RecordID, Storage: storageName, Epoch: ep, URI: l.URL, Date: l.Timestamp, Type: l.Type, Mime: l.Mime,
			Status: status, Digest: l.Digest, RefersTo: l.RefersTo,
			Location: key, Offset: base + off, Length: n, File: l.Filename,
		})
	}
	pages := make([]index.PageRow, 0, len(pageLines))
	for _, p := range pageLines {
		base, ok := offsets[p.Filename]
		if p.MetadataID == "" || p.MetaLength == 0 || !ok {
			return nil, nil, errors.New("pages lack metadata record positions")
		}
		pages = append(pages, index.PageRow{Storage: storageName, Epoch: ep, File: p.Filename, Location: key,
			Page: recorder.CapturedPage{MetadataID: p.MetadataID, Offset: base + p.MetaOffset, Length: p.MetaLength, PageInfo: p.PageInfo}})
	}
	return recs, pages, nil
}

func fromPackageScan(pkg *wacz.Reader, ep epoch.ID, key, storageName string) ([]index.Record, []index.PageRow, error) {
	offsets, err := pkg.WARCOffsets()
	if err != nil {
		return nil, nil, err
	}
	names := make([]string, 0, len(offsets))
	for n := range offsets {
		names = append(names, n)
	}
	sort.Strings(names)
	var recs []index.Record
	var pages []index.PageRow
	for _, name := range names {
		sec, err := pkg.Entry(wacz.ArchiveDir + name)
		if err != nil {
			return nil, nil, err
		}
		cf, err := recorder.Scan(sec, name, ep)
		if err != nil {
			return nil, nil, fmt.Errorf("scan %s: %w", name, err)
		}
		for _, r := range cf.Records {
			if !r.Indexable() {
				continue
			}
			mime := r.Mime
			if r.Type == "revisit" {
				mime = index.RevisitMime
			}
			recs = append(recs, index.Record{
				ID: r.ID, Storage: storageName, Epoch: ep, URI: r.URI, Date: r.Date, Type: r.Type, Mime: mime, Status: r.Status,
				Digest: r.Digest, RefersTo: r.RefersTo, Location: key, Offset: offsets[name] + r.Offset,
				Length: r.Length, File: name,
			})
		}
		for _, p := range cf.Pages {
			p.Offset += offsets[name]
			pages = append(pages, index.PageRow{Storage: storageName, Epoch: ep, File: name, Location: key, Page: p})
		}
	}
	return recs, pages, nil
}
