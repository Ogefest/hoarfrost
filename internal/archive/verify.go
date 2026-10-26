package archive

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/ogefest/hoarfrost/internal/cdxj"
	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/index"
	"github.com/ogefest/hoarfrost/internal/recorder"
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

// maxProblems bounds the problems kept in a verification result; the rest
// are only counted.
const maxProblems = 100

// checkBatch bounds the records and pages looked up in the index at once.
const checkBatch = 5000

// VerifyOptions configure VerifyWith.
type VerifyOptions struct {
	// Progress reports bytes of the package read so far.
	Progress func(done, total int64)
}

// Verify checks a sealed package in storage. See VerifyWith.
func (a *Archive) Verify(ctx context.Context, ep epoch.ID) (*Verification, error) {
	return a.VerifyWith(ctx, ep, VerifyOptions{})
}

// VerifyWith checks a sealed package in storage in one sequential read:
//
//   - the package's SHA-256 against the catalog and the .sha256 file, and
//     every entry against datapackage.json;
//   - every capture file of the captures list is in the package byte for
//     byte (its SHA-256 taken when it was read from incoming/), and every
//     capture the index knows of the week is on the list;
//   - every WARC record is readable with valid block and payload digests;
//   - every indexed record of the week is in the package at the position
//     the index gives, and no indexable record of the package is missing
//     from the index;
//   - every revisit points to a record of the package with the same digest;
//   - every CDXJ and pages.jsonl line points at its record.
//
// Memory use grows with the number of capture files and pages, not with
// the number of records. The result is written to the catalog and the
// index.
func (a *Archive) VerifyWith(ctx context.Context, ep epoch.ID, opts VerifyOptions) (*Verification, error) {
	v := &Verification{At: a.Now().UTC()}
	cat, err := a.ReadCatalog(ctx, ep)
	if err != nil {
		return nil, fmt.Errorf("archive: catalog: %w", err)
	}
	rd, err := a.Storage.Open(ctx, cat.Package)
	if err != nil {
		return nil, err
	}
	defer rd.Close()
	vr := &verifier{a: a, ctx: ctx, v: v, cat: cat, pkgKey: cat.Package, opts: opts, metadata: map[string]index.Location{}}
	if rd.Size() != cat.Size {
		vr.add(cat.Package, "%d bytes, catalog says %d", rd.Size(), cat.Size)
	}
	if cat.Parquet != nil {
		a.verifyParquet(ctx, cat.Parquet, vr.add)
	}
	if cat.Manifest != "" {
		if vr.manifest, err = a.ReadManifest(ctx, ep); err != nil {
			vr.add(cat.Manifest, "%v", err)
		}
	}
	vr.seenFiles = map[string]bool{}
	if vr.manifest != nil {
		vr.byFile = map[string][]ManifestCapture{}
		for _, c := range vr.manifest.Captures {
			vr.byFile[c.File] = append(vr.byFile[c.File], c)
		}
		for _, segs := range vr.byFile {
			sort.Slice(segs, func(i, j int) bool { return segs[i].Offset < segs[j].Offset })
		}
	}
	pkg, err := wacz.NewReader(rd, rd.Size())
	if err != nil {
		vr.add(cat.Package, "%v", err)
	} else if vr.offsets, err = pkg.WARCOffsets(); err != nil {
		vr.add(cat.Package, "%v", err)
		pkg = nil
	}
	sum, err := vr.scan(pkg, rd.Size())
	if err != nil {
		return nil, err
	}
	if sum != cat.SHA256 {
		vr.add(cat.Package, "sha256 %s, catalog says %s", sum, cat.SHA256)
	}
	if line, err := storage.ReadAll(ctx, a.Storage, ep.ChecksumKey()); err != nil {
		vr.add(ep.ChecksumKey(), "%v", err)
	} else if f := strings.Fields(string(line)); len(f) == 0 || f[0] != sum {
		vr.add(ep.ChecksumKey(), "does not match package")
	}
	if pkg != nil {
		for _, p := range pkg.CheckSums(vr.sums) {
			vr.add(p.Path, "%s", p.Message)
		}
		if err := vr.checkIndex(); err != nil {
			return nil, err
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
		if n := len(v.Problems) + v.MoreProblems; n > 1 {
			info.Error = fmt.Sprintf("%s (and %d more problems)", info.Error, n-1)
		}
		a.Log.Error("epoch verification failed", "epoch", cat.Epoch, "problems", len(v.Problems)+v.MoreProblems, "first", info.Error)
	}
	return v, a.Index.SetEpoch(ctx, info)
}

// verifier holds the state of one verification.
type verifier struct {
	a        *Archive
	ctx      context.Context
	v        *Verification
	cat      *Catalog
	pkgKey   string
	opts     VerifyOptions
	manifest *Manifest
	offsets  map[string]int64 // WARC file -> data offset
	// byFile groups the captures list by WARC file, by offset.
	byFile    map[string][]ManifestCapture
	seenFiles map[string]bool
	sums      map[string]wacz.EntrySum

	pending  []recordCheck
	revisits []recordCheck
	// metadata are the positions of page metadata records in the package.
	metadata  map[string]index.Location
	matched   int64 // indexed records found in the package at their place
	cdxjLines int64
	pageLines int64
	segments  int
}

// recordCheck is a record (or a CDXJ line) to compare with the index.
type recordCheck struct {
	where          string
	line           bool // a CDXJ line, not a record read from the package
	id             string
	offset, length int64
	digest         string // payload digest of a revisit
	refersTo       string
}

func (vr *verifier) add(p, f string, args ...any) {
	if len(vr.v.Problems) >= maxProblems {
		vr.v.MoreProblems++
		return
	}
	vr.v.Problems = append(vr.v.Problems, wacz.Problem{Path: p, Message: fmt.Sprintf(f, args...)})
}

// posReader counts and hashes everything read from the package.
type posReader struct {
	r   io.Reader
	h   hash.Hash
	pos int64
}

func (p *posReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.h.Write(b[:n])
	p.pos += int64(n)
	return n, err
}

// scan reads the package once from start to end and returns its SHA-256.
// With pkg nil (the zip directory is unreadable) it only hashes.
func (vr *verifier) scan(pkg *wacz.Reader, size int64) (string, error) {
	stream, _, err := storage.Stream(vr.ctx, vr.a.Storage, vr.pkgKey)
	if err != nil {
		return "", err
	}
	defer stream.Close()
	src := &posReader{r: bufio.NewReaderSize(stream, 1<<20), h: sha256.New()}
	vr.sums = map[string]wacz.EntrySum{}
	if pkg != nil {
		type span struct {
			name        string
			start, size int64
		}
		var spans []span
		for _, f := range pkg.Files() {
			off, err := f.DataOffset()
			if err != nil {
				vr.add(f.Name, "%v", err)
				continue
			}
			spans = append(spans, span{f.Name, off, int64(f.CompressedSize64)})
		}
		sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
		for _, e := range spans {
			if err := vr.ctx.Err(); err != nil {
				return "", err
			}
			if e.start < src.pos {
				vr.add(e.name, "overlaps the previous entry")
				continue
			}
			if _, err := io.CopyN(io.Discard, src, e.start-src.pos); err != nil {
				return "", fmt.Errorf("archive: read %s: %w", vr.pkgKey, err)
			}
			eh := sha256.New()
			body := &posReader{r: io.LimitReader(src, e.size), h: eh}
			var checkErr error
			switch {
			case strings.HasPrefix(e.name, wacz.ArchiveDir):
				checkErr = vr.checkWARC(body, strings.TrimPrefix(e.name, wacz.ArchiveDir), e.start, e.size)
			case e.name == wacz.PathCDXJ:
				checkErr = vr.checkCDXJ(body, e.name)
			case e.name == wacz.PathZipNumCDX:
				checkErr = vr.checkZipNum(body)
			case e.name == wacz.PathPages:
				checkErr = vr.checkPages(body)
			}
			if checkErr != nil {
				return "", checkErr
			}
			if _, err := io.Copy(io.Discard, body); err != nil {
				return "", fmt.Errorf("archive: read %s: %w", vr.pkgKey, err)
			}
			if body.pos != e.size {
				vr.add(e.name, "ends after %d of %d bytes", body.pos, e.size)
			}
			vr.sums[e.name] = wacz.EntrySum{SHA256: hex.EncodeToString(eh.Sum(nil)), Size: body.pos}
			if vr.opts.Progress != nil {
				vr.opts.Progress(src.pos, size)
			}
		}
	}
	if _, err := io.Copy(io.Discard, src); err != nil {
		return "", fmt.Errorf("archive: read %s: %w", vr.pkgKey, err)
	}
	if vr.opts.Progress != nil {
		vr.opts.Progress(src.pos, size)
	}
	return hex.EncodeToString(src.h.Sum(nil)), nil
}

// segmentHasher checks that the bytes of a WARC entry are exactly the
// capture files the captures list puts there, each with its SHA-256.
type segmentHasher struct {
	vr    *verifier
	file  string
	pos   int64 // absolute position of the next byte
	segs  []ManifestCapture
	cur   int
	h     hash.Hash
	stray int64
}

func (s *segmentHasher) Write(b []byte) (int, error) {
	n := len(b)
	for len(b) > 0 {
		if s.cur >= len(s.segs) || s.pos < s.segs[s.cur].Offset {
			// Bytes before the next capture belong to none.
			end := s.pos + int64(len(b))
			if s.cur < len(s.segs) {
				end = min(end, s.segs[s.cur].Offset)
			}
			s.stray += end - s.pos
			b = b[end-s.pos:]
			s.pos = end
			continue
		}
		c := s.segs[s.cur]
		if s.h == nil {
			s.h = sha256.New()
		}
		take := min(int64(len(b)), c.Offset+c.Size-s.pos)
		s.h.Write(b[:take])
		b, s.pos = b[take:], s.pos+take
		if s.pos == c.Offset+c.Size {
			if got := hex.EncodeToString(s.h.Sum(nil)); got != c.SHA256 {
				s.vr.add(wacz.ArchiveDir+s.file, "capture %s differs from the file packaged (sha256 %s, list says %s)", c.Key, got, c.SHA256)
			} else {
				s.vr.segments++
			}
			s.h = nil
			s.cur++
		}
	}
	return n, nil
}

func (s *segmentHasher) finish() {
	if s.stray > 0 {
		s.vr.add(wacz.ArchiveDir+s.file, "%d bytes belong to no capture of the list", s.stray)
	}
	for _, c := range s.segs[s.cur:] {
		s.vr.add(wacz.ArchiveDir+s.file, "capture %s (%d bytes at %d) is not in the package", c.Key, c.Size, c.Offset)
	}
}

// checkWARC reads the records of one WARC entry.
func (vr *verifier) checkWARC(body io.Reader, file string, start, size int64) error {
	where := wacz.ArchiveDir + file
	vr.seenFiles[file] = true
	if vr.manifest != nil {
		sh := &segmentHasher{vr: vr, file: file, pos: start, segs: vr.byFile[file]}
		defer sh.finish()
		body = io.TeeReader(body, sh)
	}
	rd := warcio.NewReader(body)
	for {
		rec, off, n, err := rd.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			vr.add(where, "unreadable after byte %d: %v", off, err)
			return nil
		}
		abs := start + off
		vr.v.Scanned++
		if ok, err := warcio.VerifyDigest(rec.Header.Get(warcio.HBlockDigest), rec.Block); !ok || err != nil {
			vr.add(where, "record %s: block digest mismatch", rec.ID())
		}
		pd := rec.Header.Get(warcio.HPayloadDigest)
		switch rec.Type() {
		case warcio.TypeResponse:
			if rec.Header.Get(warcio.HTruncated) == "" && pd != "" {
				payload := rec.Block[len(warcio.HTTPHeaderBytes(rec.Block)):]
				if ok, _ := warcio.VerifyDigest(pd, payload); !ok {
					vr.add(where, "record %s: payload digest mismatch", rec.ID())
				}
			}
		case warcio.TypeResource:
			if ok, _ := warcio.VerifyDigest(pd, rec.Block); pd != "" && !ok {
				vr.add(where, "record %s: payload digest mismatch", rec.ID())
			}
		case warcio.TypeMetadata:
			if recorder.IsMetadataContentType(rec.Header.Get(warcio.HContentType)) {
				vr.metadata[rec.ID()] = index.Location{Location: vr.pkgKey, Offset: abs, Length: n}
			}
		}
		switch rec.Type() {
		case warcio.TypeResponse, warcio.TypeRevisit, warcio.TypeResource:
			c := recordCheck{where: where, id: rec.ID(), offset: abs, length: n}
			if rec.Type() == warcio.TypeRevisit {
				c.digest, c.refersTo = pd, rec.Header.Get(warcio.HRefersTo)
			}
			if err := vr.check(c); err != nil {
				return err
			}
		}
	}
}

// check queues a record found in the package or a CDXJ line for comparison
// with the index.
func (vr *verifier) check(c recordCheck) error {
	if c.refersTo != "" {
		vr.revisits = append(vr.revisits, c)
	}
	vr.pending = append(vr.pending, c)
	if len(vr.pending) >= checkBatch || len(vr.revisits) >= checkBatch {
		return vr.flush()
	}
	return nil
}

// flush compares queued records with the index.
func (vr *verifier) flush() error {
	if len(vr.pending) > 0 {
		ids := make([]string, len(vr.pending))
		for i, c := range vr.pending {
			ids[i] = c.id
		}
		got, err := vr.a.Index.RecordsByID(vr.ctx, ids)
		if err != nil {
			return err
		}
		for _, c := range vr.pending {
			r, ok := got[c.id]
			switch {
			case !ok && c.line:
				vr.add(c.where, "line for %s: record not in the index", c.id)
			case !ok:
				vr.add(c.where, "record %s at %d is not in the index", c.id, c.offset)
			case r.Location != vr.pkgKey || r.Offset != c.offset || r.Length != c.length:
				vr.add(c.where, "%s: index has it at %s:%d (%d bytes), package at %d (%d bytes)", c.id, r.Location, r.Offset, r.Length, c.offset, c.length)
			case !c.line:
				vr.matched++
			}
		}
		vr.pending = vr.pending[:0]
	}
	if len(vr.revisits) > 0 {
		ids := make([]string, len(vr.revisits))
		for i, c := range vr.revisits {
			ids[i] = c.refersTo
		}
		got, err := vr.a.Index.RecordsByID(vr.ctx, ids)
		if err != nil {
			return err
		}
		for _, c := range vr.revisits {
			orig, ok := got[c.refersTo]
			switch {
			case !ok || orig.Location != vr.pkgKey:
				vr.add(vr.pkgKey, "revisit %s refers to %s outside the package", c.id, c.refersTo)
			case orig.Digest != c.digest:
				vr.add(vr.pkgKey, "revisit %s digest differs from original %s", c.id, c.refersTo)
			}
		}
		vr.revisits = vr.revisits[:0]
	}
	return nil
}

// checkCDXJ reads a plain CDXJ index.
func (vr *verifier) checkCDXJ(r io.Reader, where string) error {
	br := bufio.NewReaderSize(r, 1<<20)
	var prev string
	for {
		line, err := br.ReadString('\n')
		if line = strings.TrimRight(line, "\n"); line != "" {
			if line < prev {
				vr.add(where, "not sorted at line %d", vr.cdxjLines+1)
			} else if line == prev {
				vr.add(where, "line %d repeats the line before", vr.cdxjLines+1)
			}
			prev = line
			if cerr := vr.checkLine(line, where); cerr != nil {
				return cerr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			vr.add(where, "%v", err)
			return nil
		}
	}
}

// checkZipNum reads the gzipped blocks of a ZipNum index as one stream.
func (vr *verifier) checkZipNum(r io.Reader) error {
	gz, err := gzip.NewReader(bufio.NewReaderSize(r, 1<<20))
	if err != nil {
		vr.add(wacz.PathZipNumCDX, "%v", err)
		return nil
	}
	return vr.checkCDXJ(gz, wacz.PathZipNumCDX)
}

func (vr *verifier) checkLine(s, where string) error {
	vr.cdxjLines++
	l, err := cdxj.Parse(s)
	if err != nil {
		vr.add(where, "line %d: %v", vr.cdxjLines, err)
		return nil
	}
	base, ok := vr.offsets[l.Filename]
	off, err1 := strconv.ParseInt(l.Offset, 10, 64)
	n, err2 := strconv.ParseInt(l.Length, 10, 64)
	if !ok || l.RecordID == "" || err1 != nil || err2 != nil {
		vr.add(where, "line %d (%s) does not locate a record of the package", vr.cdxjLines, l.URL)
		return nil
	}
	return vr.check(recordCheck{where: where, line: true, id: l.RecordID, offset: base + off, length: n})
}

// checkPages reads pages.jsonl: each page must point at its metadata
// record, where the index has it too.
func (vr *verifier) checkPages(r io.Reader) error {
	br := bufio.NewReaderSize(r, 1<<20)
	var batch []wacz.PageLine
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		ids := make([]string, len(batch))
		for i, p := range batch {
			ids[i] = p.MetadataID
		}
		got, err := vr.a.Index.PageLocations(vr.ctx, ids)
		if err != nil {
			return err
		}
		for _, p := range batch {
			abs := vr.offsets[p.Filename] + p.MetaOffset
			if m, ok := vr.metadata[p.MetadataID]; !ok || m.Offset != abs || m.Length != p.MetaLength {
				vr.add(wacz.PathPages, "page %s: metadata record %s is not at %s:%d (or listed twice)", p.URL, p.MetadataID, p.Filename, p.MetaOffset)
			}
			delete(vr.metadata, p.MetadataID)
			if l, ok := got[p.MetadataID]; !ok || l.Location != vr.pkgKey || l.Offset != abs || l.Length != p.MetaLength {
				vr.add(wacz.PathPages, "page %s: the index does not place it at %d", p.URL, abs)
			}
		}
		batch = batch[:0]
		return nil
	}
	for n := 1; ; n++ {
		line, err := br.ReadBytes('\n')
		// The first line is the header of the pages list.
		if n > 1 && len(strings.TrimSpace(string(line))) > 0 {
			vr.pageLines++
			var p wacz.PageLine
			if jerr := json.Unmarshal(line, &p); jerr != nil {
				vr.add(wacz.PathPages, "line %d: %v", n, jerr)
			} else if batch = append(batch, p); len(batch) >= checkBatch {
				if ferr := flush(); ferr != nil {
					return ferr
				}
			}
		}
		if errors.Is(err, io.EOF) {
			return flush()
		}
		if err != nil {
			vr.add(wacz.PathPages, "%v", err)
			return flush()
		}
	}
}

// checkIndex compares totals once the package is read: every indexed
// record and page of the week was found in the package, and every capture
// the index knows is on the captures list (an index rebuilt from the
// package knows no captures).
func (vr *verifier) checkIndex() error {
	if err := vr.flush(); err != nil {
		return err
	}
	ep := vr.cat.Epoch
	cont, err := vr.a.Index.Contents(vr.ctx, vr.a.name(), ep, vr.pkgKey)
	if err != nil {
		return err
	}
	vr.v.Records = int(vr.matched)
	if cont.RecordsElsewhere > 0 {
		vr.add(vr.pkgKey, "%d indexed record(s) of the week are not in the package", cont.RecordsElsewhere)
	}
	if vr.matched != cont.Records {
		vr.add(vr.pkgKey, "found %d of the %d indexed records of the week", vr.matched, cont.Records)
	}
	if vr.cdxjLines != cont.Records {
		vr.add("indexes", "%d lines for %d indexed records", vr.cdxjLines, cont.Records)
	}
	if cont.PagesElsewhere > 0 || vr.pageLines != cont.Pages || int64(vr.cat.Pages) != cont.Pages {
		vr.add(wacz.PathPages, "%d pages listed, %d in the catalog, %d indexed (%d elsewhere)", vr.pageLines, vr.cat.Pages, cont.Pages, cont.PagesElsewhere)
	}
	if vr.manifest == nil {
		return nil
	}
	vr.v.Captures = vr.segments
	for file, segs := range vr.byFile {
		if !vr.seenFiles[file] {
			vr.add(vr.cat.Manifest, "%d capture(s) listed in %s, which is not in the package", len(segs), file)
		}
	}
	files, err := vr.a.Index.Files(vr.ctx, vr.a.name(), ep)
	if err != nil {
		return err
	}
	listed := make(map[string]int64, len(vr.manifest.Captures))
	for _, c := range vr.manifest.Captures {
		listed[c.Key] = c.Size
	}
	for loc, size := range files {
		if s, ok := listed[loc]; !ok {
			vr.add(vr.cat.Manifest, "capture %s of the week is not in the package", path.Base(loc))
		} else if s != size {
			vr.add(vr.cat.Manifest, "capture %s has %d bytes in the package, %d when it was stored", path.Base(loc), s, size)
		}
	}
	return nil
}
