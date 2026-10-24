package archive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/index"
	"github.com/ogefest/hoarfrost/internal/recorder"
	"github.com/ogefest/hoarfrost/internal/storage"
	"github.com/ogefest/hoarfrost/internal/version"
	"github.com/ogefest/hoarfrost/internal/wacz"
	"github.com/ogefest/hoarfrost/internal/warcio"
)

// ErrAlreadySealed is returned when the epoch already has a package.
var ErrAlreadySealed = errors.New("archive: epoch already sealed")

// ErrEmptyEpoch is returned when there is nothing to package.
var ErrEmptyEpoch = errors.New("archive: epoch has no captures")

// Seal packages all incoming captures of an epoch into a WACZ file, stores
// it, verifies it and moves index entries to the package. The caller must
// ensure no capture of the epoch is still running.
func (a *Archive) Seal(ctx context.Context, ep epoch.ID) (*Catalog, error) {
	a.sealMu.Lock()
	defer a.sealMu.Unlock()
	log := a.Log.With("epoch", ep)

	if _, err := a.Storage.Stat(ctx, ep.WeeklyKey()); err == nil {
		return nil, fmt.Errorf("%w: %s exists", ErrAlreadySealed, ep.WeeklyKey())
	} else if !errors.Is(err, storage.ErrNotExist) {
		return nil, err
	}
	if err := a.Index.SetEpoch(ctx, index.EpochInfo{Storage: a.name(), Epoch: ep, State: index.StateSealing}); err != nil {
		return nil, err
	}
	cat, err := a.seal(ctx, ep)
	if err != nil {
		log.Error("seal failed", "err", err)
		state := index.StateFailed
		if errors.Is(err, ErrEmptyEpoch) {
			state = index.StateOpen
		}
		if serr := a.Index.SetEpoch(context.WithoutCancel(ctx), index.EpochInfo{Storage: a.name(), Epoch: ep, State: state, Error: err.Error()}); serr != nil {
			log.Error("recording seal failure", "err", serr)
		}
		return nil, err
	}
	log.Info("epoch sealed", "package", cat.Package, "size", cat.Size, "records", cat.Records, "pages", cat.Pages)
	return cat, nil
}

func (a *Archive) seal(ctx context.Context, ep epoch.ID) (*Catalog, error) {
	objs, err := a.Storage.List(ctx, ep.IncomingPrefix())
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, o := range objs {
		if strings.HasSuffix(o.Key, ".warc.gz") {
			keys = append(keys, o.Key)
		}
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return nil, ErrEmptyEpoch
	}
	indexed, err := a.Index.Files(ctx, a.name(), ep)
	if err != nil {
		return nil, err
	}
	inStorage := map[string]bool{}
	for _, k := range keys {
		inStorage[k] = true
	}
	for loc := range indexed {
		if strings.HasPrefix(loc, "incoming/") && !inStorage[loc] {
			return nil, fmt.Errorf("archive: indexed capture %s missing from storage", loc)
		}
	}

	if err := os.MkdirAll(a.SpoolDir, 0o755); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(a.SpoolDir, "seal-"+string(ep)+"-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	var inputs []wacz.Input
	for _, key := range keys {
		local := path.Join(dir, path.Base(key))
		if err := a.download(ctx, key, local); err != nil {
			return nil, err
		}
		f, err := os.Open(local)
		if err != nil {
			return nil, err
		}
		cf, err := recorder.Scan(f, path.Base(key), ep)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("archive: scan %s: %w", key, err)
		}
		cf.Storage = a.name()
		if _, ok := indexed[key]; !ok {
			if err := a.Index.AddCapture(ctx, cf, key); err != nil {
				return nil, err
			}
		}
		inputs = append(inputs, wacz.Input{Capture: cf, Open: func(context.Context) (io.ReadCloser, error) { return os.Open(local) }})
	}

	pkgPath := path.Join(dir, "package.wacz")
	res, err := wacz.Build(ctx, pkgPath, inputs, wacz.Options{
		Epoch: ep, Title: "Web archive " + string(ep), Software: version.Software(),
		Created: a.Now(), ConcatBySource: a.ConcatBySource,
	})
	if err != nil {
		return nil, err
	}
	if err := a.verifyLocal(ctx, pkgPath, res); err != nil {
		return nil, err
	}

	f, err := os.Open(pkgPath)
	if err != nil {
		return nil, err
	}
	err = a.Storage.Put(ctx, ep.WeeklyKey(), f, res.Size)
	f.Close()
	if err != nil {
		return nil, err
	}
	sumLine := wacz.ChecksumLine(res.SHA256, path.Base(ep.WeeklyKey()))
	if err := storage.PutBytes(ctx, a.Storage, ep.ChecksumKey(), []byte(sumLine)); err != nil {
		return nil, err
	}
	st, err := a.Storage.Stat(ctx, ep.WeeklyKey())
	if err != nil {
		return nil, fmt.Errorf("archive: uploaded package check failed: %w", err)
	}
	if st.Size != res.Size {
		return nil, fmt.Errorf("archive: uploaded package has %d bytes, expected %d", st.Size, res.Size)
	}

	locs := map[string]index.Location{}
	pageLocs := map[string]index.Location{}
	fileMap := map[string]string{}
	revisits := 0
	for _, in := range inputs {
		target := in.Capture.Name
		if a.ConcatBySource {
			target = wacz.SourceFromName(in.Capture.Name) + ".warc.gz"
		}
		fileMap[in.Capture.Name] = target
		for _, p := range in.Capture.Pages {
			l := res.Records[p.MetadataID]
			pageLocs[p.MetadataID] = index.Location{Location: ep.WeeklyKey(), Offset: l.Offset, Length: l.Length}
		}
		for _, r := range in.Capture.Records {
			if !r.Indexable() {
				continue
			}
			if r.Type == warcio.TypeRevisit {
				revisits++
			}
			l := res.Records[r.ID]
			locs[r.ID] = index.Location{Location: ep.WeeklyKey(), Offset: l.Offset, Length: l.Length, File: target}
		}
	}
	if err := a.Index.Relocate(ctx, locs, pageLocs, fileMap); err != nil {
		return nil, err
	}
	parquetInfo, err := a.writeParquet(ctx, ep)
	if err != nil {
		return nil, fmt.Errorf("archive: page table: %w", err)
	}

	now := a.Now().UTC()
	cat := &Catalog{
		Epoch: ep, State: index.StateSealed, Package: ep.WeeklyKey(), SHA256: res.SHA256, Size: res.Size,
		SealedAt: now, Software: version.Software(), WACZVersion: wacz.Version, Incoming: keys, Files: res.Files,
		Records: len(locs), Revisits: revisits, Pages: res.Pages,
		IncomingDeleteAfter: now.Add(a.IncomingRetention), Parquet: parquetInfo,
	}
	for _, k := range keys {
		a.forget(k)
	}
	if err := a.WriteCatalog(ctx, cat); err != nil {
		return nil, err
	}
	if err := a.Index.SetEpoch(ctx, index.EpochInfo{Storage: a.name(), Epoch: ep, State: index.StateSealed, WACZKey: cat.Package,
		SHA256: cat.SHA256, Size: cat.Size, SealedAt: now}); err != nil {
		return nil, err
	}
	if v, err := a.Verify(ctx, ep); err != nil {
		return cat, err
	} else {
		cat.Verification = v
		cat.State = stateFor(v)
	}
	return cat, nil
}

func (a *Archive) download(ctx context.Context, key, local string) error {
	r, err := a.Storage.Open(ctx, key)
	if err != nil {
		return err
	}
	defer r.Close()
	f, err := os.Create(local)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, io.NewSectionReader(r, 0, r.Size())); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// verifyLocal checks the freshly built package before it is uploaded.
func (a *Archive) verifyLocal(ctx context.Context, pkgPath string, res *wacz.Result) error {
	r, err := wacz.OpenFile(pkgPath)
	if err != nil {
		return err
	}
	defer r.Close()
	if probs := r.Verify(); len(probs) > 0 {
		return fmt.Errorf("archive: built package invalid: %s: %s", probs[0].Path, probs[0].Message)
	}
	for id, loc := range res.Records {
		rec, err := r.ReadRecord(loc.Offset, loc.Length)
		if err != nil {
			return fmt.Errorf("archive: record %s not readable at %d: %w", id, loc.Offset, err)
		}
		if rec.ID() != id {
			return fmt.Errorf("archive: record at %d is %s, expected %s", loc.Offset, rec.ID(), id)
		}
	}
	return nil
}

// Cleanup deletes incoming files of verified epochs whose retention passed.
func (a *Archive) Cleanup(ctx context.Context) (int, error) {
	cats, err := a.Catalogs(ctx)
	if err != nil {
		return 0, err
	}
	deleted := 0
	now := a.Now()
	for _, c := range cats {
		if c.IncomingDeleted || c.State != index.StateVerified || now.Before(c.IncomingDeleteAfter) {
			continue
		}
		objs, err := a.Storage.List(ctx, c.Epoch.IncomingPrefix())
		if err != nil {
			return deleted, err
		}
		for _, o := range objs {
			a.forget(o.Key)
			if err := a.Storage.Delete(ctx, o.Key); err != nil {
				return deleted, err
			}
			deleted++
		}
		c.IncomingDeleted = true
		if err := a.WriteCatalog(ctx, c); err != nil {
			return deleted, err
		}
		a.Log.Info("incoming files removed", "epoch", c.Epoch, "files", len(objs))
	}
	return deleted, nil
}
