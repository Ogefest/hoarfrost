package archive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
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

// Phases of sealing reported to SealOptions.Progress.
const (
	PhasePackaging  = "packaging"
	PhaseRelocating = "relocating index"
	PhasePageTable  = "page table"
	PhaseVerifying  = "verifying"
)

// SealOptions configure Seal.
type SealOptions struct {
	// Progress reports the current phase; done and total count bytes in
	// the packaging and verification phases.
	Progress func(phase string, done, total int64)
}

func (o SealOptions) progress(phase string, done, total int64) {
	if o.Progress != nil {
		o.Progress(phase, done, total)
	}
}

// Seal packages all incoming captures of an epoch into a WACZ file, stores
// it, verifies it and moves index entries to the package. The caller must
// ensure no capture of the epoch is still running.
func (a *Archive) Seal(ctx context.Context, ep epoch.ID) (*Catalog, error) {
	return a.SealWith(ctx, ep, SealOptions{})
}

// SealWith is Seal with progress reporting. The package is streamed to
// storage while it is built, so neither local disk nor memory grows with
// the size of the week. A seal interrupted after the package was stored
// resumes from the captures list; one interrupted earlier starts over.
func (a *Archive) SealWith(ctx context.Context, ep epoch.ID, opts SealOptions) (*Catalog, error) {
	a.sealMu.Lock()
	defer a.sealMu.Unlock()
	log := a.Log.With("epoch", ep)

	if c, err := a.ReadCatalog(ctx, ep); err == nil {
		return a.alreadySealed(ctx, c, opts)
	} else if !errors.Is(err, storage.ErrNotExist) {
		return nil, err
	}
	if err := a.Index.SetEpoch(ctx, index.EpochInfo{Storage: a.name(), Epoch: ep, State: index.StateSealing}); err != nil {
		return nil, err
	}
	cat, err := a.resumeOrSeal(ctx, ep, opts)
	if err != nil {
		log.Error("seal failed", "err", err)
		info := index.EpochInfo{Storage: a.name(), Epoch: ep, State: index.StateFailed, Error: err.Error()}
		if errors.Is(err, ErrEmptyEpoch) {
			info.State = index.StateOpen
		}
		// With the catalog written only verification could not finish:
		// the week keeps its package and is verified again, not resealed.
		if c, cerr := a.ReadCatalog(context.WithoutCancel(ctx), ep); cerr == nil {
			info.WACZKey, info.SHA256, info.Size, info.SealedAt = c.Package, c.SHA256, c.Size, c.SealedAt
		}
		if serr := a.Index.SetEpoch(context.WithoutCancel(ctx), info); serr != nil {
			log.Error("recording seal failure", "err", serr)
		}
		return nil, err
	}
	log.Info("epoch sealed", "package", cat.Package, "size", cat.Size, "captures", cat.Captures, "records", cat.Records, "pages", cat.Pages)
	return cat, nil
}

// alreadySealed handles a week whose catalog exists: the index takes its
// state from the catalog again (a seal attempt may have overwritten it),
// and a package never verified (the process stopped right after writing
// the catalog) is verified now. A verified package is ErrAlreadySealed.
func (a *Archive) alreadySealed(ctx context.Context, c *Catalog, opts SealOptions) (*Catalog, error) {
	info := index.EpochInfo{Storage: a.name(), Epoch: c.Epoch, State: c.State, WACZKey: c.Package, SHA256: c.SHA256, Size: c.Size, SealedAt: c.SealedAt}
	if c.Verification == nil {
		info.State = index.StateSealed
	} else {
		info.VerifiedAt = c.Verification.At
		if !c.Verification.OK && len(c.Verification.Problems) > 0 {
			info.Error = c.Verification.Problems[0].Path + ": " + c.Verification.Problems[0].Message
		}
	}
	if err := a.Index.SetEpoch(ctx, info); err != nil {
		return nil, err
	}
	if c.Verification != nil {
		return nil, fmt.Errorf("%w: %s exists", ErrAlreadySealed, c.Epoch.CatalogKey())
	}
	a.Log.Warn("verifying a package sealed without verification", "epoch", c.Epoch)
	v, err := a.VerifyWith(ctx, c.Epoch, VerifyOptions{Progress: func(done, total int64) { opts.progress(PhaseVerifying, done, total) }})
	if err != nil {
		return nil, err
	}
	c.Verification, c.State = v, stateFor(v)
	return c, nil
}

// resumeOrSeal continues a seal whose package and captures list are
// stored, removes the package of one interrupted before its captures list
// was written (the index was not moved to it yet), and seals.
func (a *Archive) resumeOrSeal(ctx context.Context, ep epoch.ID, opts SealOptions) (*Catalog, error) {
	st, err := a.Storage.Stat(ctx, ep.WeeklyKey())
	if errors.Is(err, storage.ErrNotExist) {
		return a.seal(ctx, ep, opts)
	}
	if err != nil {
		return nil, err
	}
	m, err := a.ReadManifest(ctx, ep)
	switch {
	case err == nil && m.Package == ep.WeeklyKey() && m.Size == st.Size:
		a.Log.Warn("resuming an interrupted seal", "epoch", ep, "package", m.Package)
		return a.finishSeal(ctx, m, opts)
	case err == nil:
		// The index may already point into this package: never replace it.
		return nil, fmt.Errorf("archive: %s does not describe %s (%d bytes); resolve by hand", ep.CapturesKey(), ep.WeeklyKey(), st.Size)
	case !errors.Is(err, storage.ErrNotExist):
		return nil, err
	}
	a.Log.Warn("removing the package of an interrupted seal", "epoch", ep, "package", ep.WeeklyKey())
	for _, k := range []string{ep.ChecksumKey(), ep.WeeklyKey()} {
		if err := a.Storage.Delete(ctx, k); err != nil {
			return nil, err
		}
	}
	return a.seal(ctx, ep, opts)
}

// sealSpool returns an empty spool directory for sealing ep, removing
// what earlier seals of the week left behind (also "seal-<week>-*"
// directories of versions that downloaded the whole week).
func (a *Archive) sealSpool(ep epoch.ID) (string, error) {
	old, _ := filepath.Glob(filepath.Join(a.SpoolDir, "seal-"+string(ep)+"*"))
	for _, p := range old {
		if err := os.RemoveAll(p); err != nil {
			return "", err
		}
	}
	dir := filepath.Join(a.SpoolDir, "seal-"+string(ep))
	return dir, os.MkdirAll(dir, 0o755)
}

func (a *Archive) seal(ctx context.Context, ep epoch.ID, opts SealOptions) (*Catalog, error) {
	objs, err := a.Storage.List(ctx, ep.IncomingPrefix())
	if err != nil {
		return nil, err
	}
	keys := map[string]string{} // capture name -> incoming key
	var inputs []wacz.Input
	var total int64
	for _, o := range objs {
		if !strings.HasSuffix(o.Key, ".warc.gz") {
			continue
		}
		key := o.Key
		keys[path.Base(key)] = key
		total += o.Size
		inputs = append(inputs, wacz.Input{Name: path.Base(key), Size: o.Size, Open: func(ctx context.Context) (io.ReadCloser, error) {
			rc, _, err := storage.Stream(ctx, a.Storage, key)
			return rc, err
		}})
	}
	if len(inputs) == 0 {
		return nil, ErrEmptyEpoch
	}
	indexed, err := a.Index.Files(ctx, a.name(), ep)
	if err != nil {
		return nil, err
	}
	for loc := range indexed {
		if strings.HasPrefix(loc, "incoming/") && keys[path.Base(loc)] != loc {
			return nil, fmt.Errorf("archive: indexed capture %s missing from storage", loc)
		}
	}

	// Sort files live in a directory of the week, emptied first: a process
	// killed while sealing leaves its files there (only one seal of a week
	// runs at a time, see coordinator.Seal).
	spool, err := a.sealSpool(ep)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(spool)

	m := &Manifest{Epoch: ep, Package: ep.WeeklyKey(), Created: a.Now().UTC()}
	onCapture := func(cf recorder.CaptureFile, p wacz.Placement) error {
		key := keys[cf.Name]
		if _, ok := indexed[key]; !ok {
			cf.Storage = a.name()
			if err := a.Index.AddCapture(ctx, cf, key); err != nil {
				return err
			}
		}
		c := ManifestCapture{Key: key, File: p.File, Offset: p.Offset, Size: p.Size, SHA256: p.SHA256, Records: len(cf.Records), Pages: len(cf.Pages)}
		for _, r := range cf.Records {
			if r.Indexable() {
				c.Indexed++
				if r.Type == warcio.TypeRevisit {
					m.Revisits++
				}
			}
		}
		m.Indexed += c.Indexed
		m.Captures = append(m.Captures, c)
		return nil
	}
	// Index and page entries and the zip directory add a little to the
	// captures; the hint only sizes upload parts.
	w, err := storage.Create(ctx, a.Storage, ep.WeeklyKey(), total+total/20+1<<20)
	if err != nil {
		return nil, err
	}
	defer w.Abort()
	res, err := wacz.Build(ctx, w, inputs, wacz.Options{
		Epoch: ep, Title: "Web archive " + string(ep), Software: version.Software(),
		Created: m.Created, ConcatBySource: a.ConcatBySource, SpoolDir: spool,
		OnCapture: onCapture,
		Progress:  func(done int64) { opts.progress(PhasePackaging, done, total) },
	})
	if err != nil {
		return nil, err
	}
	n, err := w.Commit()
	if err != nil {
		return nil, err
	}
	if n != res.Size {
		return nil, fmt.Errorf("archive: stored package has %d bytes, built %d", n, res.Size)
	}
	if err := storage.PutBytes(ctx, a.Storage, ep.ChecksumKey(), []byte(wacz.ChecksumLine(res.SHA256, path.Base(ep.WeeklyKey())))); err != nil {
		return nil, err
	}
	m.SHA256, m.Size, m.WARCFiles = res.SHA256, res.Size, len(res.Files)
	m.Records, m.Pages, m.CDXJLines = res.Records, res.Pages, res.CDXJLines
	if err := a.WriteManifest(ctx, m); err != nil {
		return nil, err
	}
	return a.finishSeal(ctx, m, opts)
}

// finishSeal moves the index to a stored package, writes the page table
// and the catalog, and verifies the package. Every step can be repeated.
func (a *Archive) finishSeal(ctx context.Context, m *Manifest, opts SealOptions) (*Catalog, error) {
	ep := m.Epoch
	opts.progress(PhaseRelocating, 0, int64(len(m.Captures)))
	moves := make([]index.CaptureMove, len(m.Captures))
	for i, c := range m.Captures {
		moves[i] = index.CaptureMove{From: c.Key, Name: path.Base(c.Key), Shift: c.Offset, File: c.File}
	}
	if err := a.Index.RelocateCaptures(ctx, a.name(), ep, m.Package, moves); err != nil {
		return nil, err
	}
	opts.progress(PhasePageTable, 0, int64(m.Pages))
	parquetInfo, err := a.writeParquet(ctx, ep)
	if err != nil {
		return nil, fmt.Errorf("archive: page table: %w", err)
	}

	now := a.Now().UTC()
	cat := &Catalog{
		Epoch: ep, State: index.StateSealed, Package: m.Package, SHA256: m.SHA256, Size: m.Size,
		SealedAt: now, Software: version.Software(), WACZVersion: wacz.Version,
		Manifest: ep.CapturesKey(), Captures: len(m.Captures), WARCFiles: m.WARCFiles,
		Records: m.Indexed, Revisits: m.Revisits, Pages: m.Pages,
		IncomingDeleteAfter: now.Add(a.IncomingRetention), Parquet: parquetInfo,
	}
	for _, c := range m.Captures {
		a.forget(c.Key)
	}
	if err := a.WriteCatalog(ctx, cat); err != nil {
		return nil, err
	}
	if err := a.Index.SetEpoch(ctx, index.EpochInfo{Storage: a.name(), Epoch: ep, State: index.StateSealed, WACZKey: cat.Package,
		SHA256: cat.SHA256, Size: cat.Size, SealedAt: now}); err != nil {
		return nil, err
	}
	v, err := a.VerifyWith(ctx, ep, VerifyOptions{Progress: func(done, total int64) { opts.progress(PhaseVerifying, done, total) }})
	if err != nil {
		return cat, err
	}
	cat.Verification = v
	cat.State = stateFor(v)
	return cat, nil
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
		// Only capture files verified to be in the package are removed
		// (with their .cdxj); others, e.g. stored by a worker whose
		// completion came too late, stay for a person to look at.
		packaged, err := a.packagedKeys(ctx, c)
		if err != nil {
			return deleted, err
		}
		var kept []string
		for _, o := range objs {
			if packaged != nil && !packaged[strings.TrimSuffix(o.Key, ".cdxj")+".warc.gz"] && !packaged[o.Key] {
				kept = append(kept, o.Key)
				continue
			}
			a.forget(o.Key)
			if err := a.Storage.Delete(ctx, o.Key); err != nil {
				return deleted, err
			}
			deleted++
		}
		if len(kept) > 0 {
			a.Log.Warn("incoming files not in the package kept", "epoch", c.Epoch, "files", len(kept), "first", kept[0])
		}
		c.IncomingDeleted = true
		if err := a.WriteCatalog(ctx, c); err != nil {
			return deleted, err
		}
		a.Log.Info("incoming files removed", "epoch", c.Epoch, "files", len(objs)-len(kept))
	}
	return deleted, nil
}

// packagedKeys returns the incoming keys of the capture files in a sealed
// package, from its captures list (or the list in older catalogs); nil
// means unknown.
func (a *Archive) packagedKeys(ctx context.Context, c *Catalog) (map[string]bool, error) {
	var keys []string
	if c.Manifest != "" {
		m, err := a.ReadManifest(ctx, c.Epoch)
		if err != nil {
			return nil, err
		}
		for _, mc := range m.Captures {
			keys = append(keys, mc.Key)
		}
	} else if len(c.Incoming) > 0 {
		keys = c.Incoming
	} else {
		return nil, nil
	}
	out := make(map[string]bool, len(keys))
	for _, k := range keys {
		out[k] = true
	}
	return out, nil
}
