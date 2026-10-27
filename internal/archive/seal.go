package archive

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// Seal packages all incoming captures of an epoch into WACZ volumes,
// stores them, verifies them and moves index entries to them. The caller
// must ensure no capture of the epoch is still running.
func (a *Archive) Seal(ctx context.Context, ep epoch.ID) (*Catalog, error) {
	return a.SealWith(ctx, ep, SealOptions{})
}

// SealWith is Seal with progress reporting. The week is split into volumes
// of about VolumeSize capture bytes; each is built in the spool directory,
// stored with one request and then removed locally, so neither local disk
// nor memory grows with the size of the week. A seal interrupted after
// some volumes were stored resumes with the next one.
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
		// the week keeps its packages and is verified again, not resealed.
		if c, cerr := a.ReadCatalog(context.WithoutCancel(ctx), ep); cerr == nil {
			info.WACZKey, info.SHA256, info.Size, info.SealedAt = c.Location(), c.SHA256, c.Size, c.SealedAt
		}
		if serr := a.Index.SetEpoch(context.WithoutCancel(ctx), info); serr != nil {
			log.Error("recording seal failure", "err", serr)
		}
		return nil, err
	}
	log.Info("epoch sealed", "location", cat.Location(), "volumes", len(cat.Packages()), "size", cat.Size, "captures", cat.Captures,
		"records", cat.Records, "pages", cat.Pages)
	return cat, nil
}

// alreadySealed handles a week whose catalog exists: the index takes its
// state from the catalog again (a seal attempt may have overwritten it),
// and packages never verified (the process stopped right after writing
// the catalog) are verified now. A verified week is ErrAlreadySealed.
func (a *Archive) alreadySealed(ctx context.Context, c *Catalog, opts SealOptions) (*Catalog, error) {
	info := c.epochInfo(a.name())
	if c.Verification == nil {
		info.State = index.StateSealed
	}
	if err := a.Index.SetEpoch(ctx, info); err != nil {
		return nil, err
	}
	if c.Verification != nil {
		return nil, fmt.Errorf("%w: %s exists", ErrAlreadySealed, c.Epoch.CatalogKey())
	}
	a.Log.Warn("verifying a week sealed without verification", "epoch", c.Epoch)
	v, err := a.VerifyWith(ctx, c.Epoch, VerifyOptions{Progress: func(done, total int64) { opts.progress(PhaseVerifying, done, total) }})
	if err != nil {
		return nil, err
	}
	c.Verification, c.State = v, stateFor(v)
	return c, nil
}

// resumeOrSeal finishes a single package stored by an older version
// together with its captures list (the index may point into it already),
// removes one stored without the list, and seals the week into volumes.
func (a *Archive) resumeOrSeal(ctx context.Context, ep epoch.ID, opts SealOptions) (*Catalog, error) {
	st, err := a.Storage.Stat(ctx, ep.WeeklyKey())
	if errors.Is(err, storage.ErrNotExist) {
		return a.sealVolumes(ctx, ep, opts)
	}
	if err != nil {
		return nil, err
	}
	m, err := a.ReadManifest(ctx, ep.CapturesKey())
	switch {
	case err == nil && m.Package == ep.WeeklyKey() && m.Size == st.Size:
		a.Log.Warn("resuming an interrupted seal", "epoch", ep, "package", m.Package)
		return a.finishSeal(ctx, ep, []*Manifest{m}, true, opts)
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
	return a.sealVolumes(ctx, ep, opts)
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

// storedVolumes returns the captures lists of the volumes a previous seal
// of the week stored completely: volume 1, 2, ... up to the first one
// without its list, which is written last. Other files in the volume
// directory (a volume stored without its list, files of the last step) are
// removed, unless the index points into them or a later volume has a list:
// then the week is left for a person to look at.
func (a *Archive) storedVolumes(ctx context.Context, ep epoch.ID) ([]*Manifest, error) {
	objs, err := a.Storage.List(ctx, ep.VolumeDir())
	if err != nil {
		return nil, err
	}
	sizes := map[string]int64{}
	for _, o := range objs {
		sizes[o.Key] = o.Size
	}
	var done []*Manifest
	keep := map[string]bool{}
	for n := 1; ; n++ {
		key, list := ep.VolumeKey(n), ep.VolumeCapturesKey(n)
		if _, ok := sizes[list]; !ok {
			break
		}
		m, err := a.ReadManifest(ctx, list)
		if err != nil {
			return nil, err
		}
		if size, ok := sizes[key]; !ok || m.Package != key || m.Size != size {
			return nil, fmt.Errorf("archive: %s does not describe %s (%d bytes); resolve by hand", list, key, size)
		}
		done = append(done, m)
		keep[key], keep[list] = true, true
	}
	var remove []string
	for _, o := range objs {
		if keep[o.Key] {
			continue
		}
		if strings.HasSuffix(o.Key, ".captures.jsonl") {
			return nil, fmt.Errorf("archive: %s is stored but an earlier volume of %s is incomplete; resolve by hand", o.Key, ep)
		}
		if strings.HasSuffix(o.Key, ".wacz") {
			cont, err := a.Index.Contents(ctx, a.name(), ep, []string{o.Key})
			if err != nil {
				return nil, err
			}
			if cont.Records > cont.RecordsElsewhere || cont.Pages > cont.PagesElsewhere {
				return nil, fmt.Errorf("archive: the index points into %s, which has no captures list; resolve by hand", o.Key)
			}
		}
		remove = append(remove, o.Key)
	}
	for _, k := range remove {
		a.Log.Warn("removing a file of an interrupted seal", "epoch", ep, "key", k)
		if err := a.Storage.Delete(ctx, k); err != nil {
			return nil, err
		}
	}
	return done, nil
}

// maxSinglePut is the largest object S3 stores with one request.
const maxSinglePut = 5 << 30

// splitVolumes cuts inputs, in package order, into volumes of at most
// limit bytes (a larger capture file gets a volume of its own).
func splitVolumes(inputs []wacz.Input, limit int64) [][]wacz.Input {
	var out [][]wacz.Input
	var cur []wacz.Input
	var size int64
	for _, in := range inputs {
		if len(cur) > 0 && size+in.Size > limit {
			out = append(out, cur)
			cur, size = nil, 0
		}
		cur = append(cur, in)
		size += in.Size
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

func (a *Archive) sealVolumes(ctx context.Context, ep epoch.ID, opts SealOptions) (*Catalog, error) {
	done, err := a.storedVolumes(ctx, ep)
	if err != nil {
		return nil, err
	}
	packaged := map[string]bool{}
	var total int64
	for _, m := range done {
		for _, c := range m.Captures {
			packaged[c.Key] = true
			total += c.Size
		}
	}
	if len(done) > 0 {
		a.Log.Warn("resuming an interrupted seal", "epoch", ep, "volumes", len(done))
	}
	objs, err := a.Storage.List(ctx, ep.IncomingPrefix())
	if err != nil {
		return nil, err
	}
	keys := map[string]string{} // capture name -> incoming key
	var inputs []wacz.Input
	for _, o := range objs {
		if !strings.HasSuffix(o.Key, ".warc.gz") {
			continue
		}
		key := o.Key
		keys[path.Base(key)] = key
		if packaged[key] {
			continue
		}
		total += o.Size
		inputs = append(inputs, wacz.Input{Name: path.Base(key), Size: o.Size, Open: func(ctx context.Context) (io.ReadCloser, error) {
			rc, _, err := storage.Stream(ctx, a.Storage, key)
			return rc, err
		}})
	}
	if len(inputs) == 0 && len(done) == 0 {
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

	// Volumes and sort files live in a directory of the week, emptied
	// first: a process killed while sealing leaves its files there (only
	// one seal of a week runs at a time, see coordinator.Seal).
	spool, err := a.sealSpool(ep)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(spool)

	var written int64
	for _, m := range done {
		for _, c := range m.Captures {
			written += c.Size
		}
	}
	for _, group := range splitVolumes(wacz.Order(inputs, a.ConcatBySource), a.volumeSize()) {
		n := len(done) + 1
		base := written
		m, err := a.packVolume(ctx, ep, n, group, keys, indexed, spool, func(d int64) {
			written = base + d
			opts.progress(PhasePackaging, written, total)
		})
		if err != nil {
			return nil, fmt.Errorf("archive: volume %d: %w", n, err)
		}
		done = append(done, m)
	}
	return a.finishSeal(ctx, ep, done, false, opts)
}

// packVolume builds volume n in the spool directory, stores it with one
// request and then stores its captures list, which marks it complete.
func (a *Archive) packVolume(ctx context.Context, ep epoch.ID, n int, inputs []wacz.Input, keys map[string]string,
	indexed map[string]int64, spool string, progress func(done int64)) (*Manifest, error) {
	key := ep.VolumeKey(n)
	m := &Manifest{Epoch: ep, Package: key, Created: a.Now().UTC()}
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
	local := filepath.Join(spool, path.Base(key))
	f, err := os.Create(local)
	if err != nil {
		return nil, err
	}
	defer os.Remove(local)
	defer f.Close()
	bw := bufio.NewWriterSize(f, 1<<20)
	res, err := wacz.Build(ctx, bw, inputs, wacz.Options{
		Epoch: ep, Title: fmt.Sprintf("Web archive %s, volume %d", ep, n), Software: version.Software(),
		Created: m.Created, ConcatBySource: a.ConcatBySource, FileSuffix: fmt.Sprintf("-%04d", n), SpoolDir: spool,
		OnCapture: onCapture, Progress: progress,
	})
	if err != nil {
		return nil, err
	}
	if err := bw.Flush(); err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if res.Size > maxSinglePut {
		// A capture file larger than a volume: some S3 servers cannot
		// assemble the multipart upload this needs.
		a.Log.Warn("volume too large for one upload request, storing it in parts", "epoch", ep, "volume", key, "size", res.Size)
	}
	if err := a.Storage.Put(ctx, key, f, res.Size); err != nil {
		return nil, err
	}
	m.SHA256, m.Size, m.WARCFiles = res.SHA256, res.Size, len(res.Files)
	m.Records, m.Pages, m.CDXJLines = res.Records, res.Pages, res.CDXJLines
	if err := a.WriteManifest(ctx, ep.VolumeCapturesKey(n), m); err != nil {
		return nil, err
	}
	a.Log.Info("volume stored", "epoch", ep, "volume", key, "size", m.Size, "captures", len(m.Captures))
	return m, nil
}

// finishSeal moves the index to the stored packages, writes the page
// table, the checksums and the catalog, and verifies the packages. Every
// step can be repeated. single marks the one package of an older seal.
func (a *Archive) finishSeal(ctx context.Context, ep epoch.ID, packages []*Manifest, single bool, opts SealOptions) (*Catalog, error) {
	now := a.Now().UTC()
	cat := &Catalog{
		Epoch: ep, State: index.StateSealed, SealedAt: now, Software: version.Software(), WACZVersion: wacz.Version,
		IncomingDeleteAfter: now.Add(a.IncomingRetention),
	}
	var moves []index.CaptureMove
	for _, m := range packages {
		for _, c := range m.Captures {
			moves = append(moves, index.CaptureMove{From: c.Key, To: m.Package, Name: path.Base(c.Key), Shift: c.Offset, File: c.File})
		}
		cat.Size += m.Size
		cat.Captures += len(m.Captures)
		cat.WARCFiles += m.WARCFiles
		cat.Records += m.Indexed
		cat.Revisits += m.Revisits
		cat.Pages += m.Pages
	}
	opts.progress(PhaseRelocating, 0, int64(len(moves)))
	if err := a.Index.RelocateCaptures(ctx, a.name(), ep, moves); err != nil {
		return nil, err
	}
	opts.progress(PhasePageTable, 0, int64(cat.Pages))
	parquetInfo, err := a.writeParquet(ctx, ep)
	if err != nil {
		return nil, fmt.Errorf("archive: page table: %w", err)
	}
	cat.Parquet = parquetInfo

	if single {
		m := packages[0]
		cat.Package, cat.SHA256, cat.Manifest = m.Package, m.SHA256, ep.CapturesKey()
	} else {
		for i, m := range packages {
			cat.Volumes = append(cat.Volumes, m.volume(ep.VolumeCapturesKey(i+1)))
		}
		sums := volumeSums(cat.Volumes)
		if err := storage.PutBytes(ctx, a.Storage, ep.SumsKey(), sums); err != nil {
			return nil, err
		}
		coll, err := volumeCollection(ep, cat.Volumes)
		if err != nil {
			return nil, err
		}
		if err := storage.PutBytes(ctx, a.Storage, ep.CollectionKey(), coll); err != nil {
			return nil, err
		}
		h := sha256.Sum256(sums)
		cat.SHA256, cat.Sums, cat.Collection = hex.EncodeToString(h[:]), ep.SumsKey(), ep.CollectionKey()
	}
	for _, c := range moves {
		a.forget(c.From)
	}
	if err := a.WriteCatalog(ctx, cat); err != nil {
		return nil, err
	}
	if err := a.Index.SetEpoch(ctx, cat.epochInfo(a.name())); err != nil {
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

// volumeSums is the SHA256SUMS file of the volumes ("sha256sum -c" in the
// volume directory checks them).
func volumeSums(vols []Volume) []byte {
	var b strings.Builder
	for _, v := range vols {
		b.WriteString(wacz.ChecksumLine(v.SHA256, path.Base(v.Key)))
	}
	return []byte(b.String())
}

// Collection is a multi-WACZ collection: ReplayWeb.page opens it as one
// archive of all the WACZ files it lists (paths relative to the file).
type Collection struct {
	Profile   string          `json:"profile"`
	Title     string          `json:"title"`
	Resources []wacz.Resource `json:"resources"`
}

func volumeCollection(ep epoch.ID, vols []Volume) ([]byte, error) {
	c := Collection{Profile: "multi-wacz-package", Title: "Web archive " + string(ep)}
	for _, v := range vols {
		name := path.Base(v.Key)
		c.Resources = append(c.Resources, wacz.Resource{Name: name, Path: name, Hash: "sha256:" + v.SHA256, Bytes: v.Size})
	}
	b, err := json.MarshalIndent(c, "", "  ")
	return append(b, '\n'), err
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
// week, from the captures lists of its packages (or the list in older
// catalogs); nil means unknown.
func (a *Archive) packagedKeys(ctx context.Context, c *Catalog) (map[string]bool, error) {
	var keys []string
	if pkgs := c.Packages(); len(pkgs) > 0 && pkgs[0].CapturesList != "" {
		for _, p := range pkgs {
			m, err := a.ReadManifest(ctx, p.CapturesList)
			if err != nil {
				return nil, err
			}
			for _, mc := range m.Captures {
				keys = append(keys, mc.Key)
			}
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
