package archive

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/index"
	"github.com/ogefest/hoarfrost/internal/storage"
	"github.com/ogefest/hoarfrost/internal/wacz"
	"github.com/ogefest/hoarfrost/internal/warcio"
)

func stateFor(v *Verification) string {
	if v.OK {
		return index.StateVerified
	}
	return index.StateFailed
}

// Verify checks a sealed package in storage: the sha256 file, the
// datapackage hashes, that every entry is stored, that every indexed record
// is readable at its absolute offset with valid digests, and that every
// revisit points to a record with the same digest inside the package.
// The result is written to the catalog and the index.
func (a *Archive) Verify(ctx context.Context, ep epoch.ID) (*Verification, error) {
	v := &Verification{At: a.Now().UTC()}
	add := func(p, f string, args ...any) {
		v.Problems = append(v.Problems, wacz.Problem{Path: p, Message: fmt.Sprintf(f, args...)})
	}
	cat, err := a.ReadCatalog(ctx, ep)
	if err != nil {
		return nil, fmt.Errorf("archive: catalog: %w", err)
	}
	rd, err := a.Storage.Open(ctx, ep.WeeklyKey())
	if err != nil {
		return nil, err
	}
	defer rd.Close()

	h := sha256.New()
	if _, err := io.Copy(h, bufio.NewReaderSize(io.NewSectionReader(rd, 0, rd.Size()), 1<<20)); err != nil {
		return nil, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if sum != cat.SHA256 {
		add(ep.WeeklyKey(), "sha256 %s, catalog says %s", sum, cat.SHA256)
	}
	if cat.Parquet != nil {
		a.verifyParquet(ctx, cat.Parquet, add)
	}
	if line, err := storage.ReadAll(ctx, a.Storage, ep.ChecksumKey()); err != nil {
		add(ep.ChecksumKey(), "%v", err)
	} else if f := strings.Fields(string(line)); len(f) == 0 || f[0] != sum {
		add(ep.ChecksumKey(), "does not match package")
	}

	pkg, err := wacz.NewReader(rd, rd.Size())
	if err != nil {
		add(ep.WeeklyKey(), "%v", err)
		return a.finishVerify(ctx, cat, v)
	}
	v.Problems = append(v.Problems, pkg.Verify()...)

	lines, err := pkg.CDXJ()
	if err != nil {
		add("indexes", "%v", err)
		return a.finishVerify(ctx, cat, v)
	}
	offsets, err := pkg.WARCOffsets()
	if err != nil {
		add("archive", "%v", err)
		return a.finishVerify(ctx, cat, v)
	}
	digests := map[string]string{}
	type revisit struct{ id, refers, digest string }
	var revisits []revisit
	for _, l := range lines {
		base, ok := offsets[l.Filename]
		if !ok {
			add(wacz.PathCDXJ, "%s: unknown file %s", l.URL, l.Filename)
			continue
		}
		off, _ := strconv.ParseInt(l.Offset, 10, 64)
		n, _ := strconv.ParseInt(l.Length, 10, 64)
		rec, err := pkg.ReadRecord(base+off, n)
		if err != nil {
			add(l.Filename, "record %s at %d: %v", l.RecordID, off, err)
			continue
		}
		v.Records++
		if l.RecordID != "" && rec.ID() != l.RecordID {
			add(l.Filename, "record at %d is %s, index says %s", off, rec.ID(), l.RecordID)
		}
		if ok, err := warcio.VerifyDigest(rec.Header.Get(warcio.HBlockDigest), rec.Block); !ok || err != nil {
			add(l.Filename, "record %s: block digest mismatch", rec.ID())
		}
		pd := rec.Header.Get(warcio.HPayloadDigest)
		switch rec.Type() {
		case warcio.TypeResponse:
			if rec.Header.Get(warcio.HTruncated) == "" && pd != "" {
				payload := rec.Block[len(warcio.HTTPHeaderBytes(rec.Block)):]
				if ok, _ := warcio.VerifyDigest(pd, payload); !ok {
					add(l.Filename, "record %s: payload digest mismatch", rec.ID())
				}
			}
			digests[rec.ID()] = pd
		case warcio.TypeResource:
			if ok, _ := warcio.VerifyDigest(pd, rec.Block); pd != "" && !ok {
				add(l.Filename, "record %s: payload digest mismatch", rec.ID())
			}
			digests[rec.ID()] = pd
		case warcio.TypeRevisit:
			if ref := rec.Header.Get(warcio.HRefersTo); ref != "" {
				revisits = append(revisits, revisit{rec.ID(), ref, pd})
			}
		}
	}
	for _, r := range revisits {
		d, ok := digests[r.refers]
		if !ok {
			add(ep.WeeklyKey(), "revisit %s refers to %s outside the package", r.id, r.refers)
		} else if d != r.digest {
			add(ep.WeeklyKey(), "revisit %s digest differs from original %s", r.id, r.refers)
		}
	}
	return a.finishVerify(ctx, cat, v)
}

func (a *Archive) finishVerify(ctx context.Context, cat *Catalog, v *Verification) (*Verification, error) {
	v.OK = len(v.Problems) == 0
	cat.Verification = v
	cat.State = stateFor(v)
	if err := a.WriteCatalog(ctx, cat); err != nil {
		return v, err
	}
	info := index.EpochInfo{Storage: a.name(), Epoch: cat.Epoch, State: cat.State, WACZKey: cat.Package, SHA256: cat.SHA256,
		Size: cat.Size, SealedAt: cat.SealedAt, VerifiedAt: v.At}
	if !v.OK {
		info.Error = v.Problems[0].Path + ": " + v.Problems[0].Message
		a.Log.Error("epoch verification failed", "epoch", cat.Epoch, "problems", len(v.Problems), "first", info.Error)
	}
	return v, a.Index.SetEpoch(ctx, info)
}
