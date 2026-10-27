package archive_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ogefest/hoarfrost/internal/pgdb/pgtest"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	"github.com/parquet-go/parquet-go"

	"github.com/ogefest/hoarfrost/internal/archive"
	"github.com/ogefest/hoarfrost/internal/cdxj"
	"github.com/ogefest/hoarfrost/internal/engine"
	"github.com/ogefest/hoarfrost/internal/engine/static"
	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/index"
	"github.com/ogefest/hoarfrost/internal/recorder"
	"github.com/ogefest/hoarfrost/internal/storage"
	"github.com/ogefest/hoarfrost/internal/testsite"
	"github.com/ogefest/hoarfrost/internal/wacz"
	"github.com/ogefest/hoarfrost/internal/warcio"
)

const ep = epoch.ID("2026-W41")

type env struct {
	t     *testing.T
	site  *testsite.Site
	store storage.Storage
	idx   *index.DB
	arc   *archive.Archive
	dir   string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := storage.NewFS(filepath.Join(dir, "storage"))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, site: testsite.New(), store: st, dir: dir}
	t.Cleanup(e.site.Close)
	e.openIndex()
	return e
}

func (e *env) openIndex() {
	indexURL, indexSchema := pgtest.Schema(e.t, "index")
	idx, err := index.Open(context.Background(), indexURL, indexSchema)
	if err != nil {
		e.t.Fatal(err)
	}
	e.idx = idx
	e.arc = archive.New(e.store, idx, filepath.Join(e.dir, "spool"), nil)
	e.arc.Now = func() time.Time { return time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC) }
	e.t.Cleanup(func() { e.arc.Close(); idx.Close() })
}

// capture runs the static engine and commits the result like a worker.
func (e *env) capture(path, source string, in epoch.ID) recorder.CaptureFile {
	e.t.Helper()
	ctx := context.Background()
	rec, err := recorder.New(recorder.Options{Epoch: in, Dedup: e.idx.Dedup(""), SpoolDir: filepath.Join(e.dir, "spool"), Source: source,
		Blocked: func(h string) bool { return static.HostMatches(h, []string{"doubleclick.net", "youtube.com"}) }})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := (static.Engine{}).Capture(ctx, engine.Job{URL: e.site.URL + path, Source: source,
		Options: engine.Options{SkipFrames: []string{"youtube.com"}}}, rec); err != nil {
		e.t.Fatal(err)
	}
	cf, err := rec.Close()
	if err != nil {
		e.t.Fatal(err)
	}
	f, _ := os.Open(cf.Path)
	defer f.Close()
	if err := e.store.Put(ctx, cf.Key(), f, cf.Size); err != nil {
		e.t.Fatal(err)
	}
	if err := e.idx.AddCapture(ctx, cf, cf.Key()); err != nil {
		e.t.Fatal(err)
	}
	return cf
}

func (e *env) populate() {
	e.capture("/news/article.html", "serwis-a", ep)
	e.capture("/news/article2.html", "serwis-a", ep)
	e.capture("/files/uchwala.pdf", "serwis-b", ep)
	e.capture("/news/article.html", "serwis-a", ep.Next())
}

func (e *env) dump() string {
	d, err := e.idx.Dump(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return d
}

func TestSealVerifyAndRead(t *testing.T) {
	e := newEnv(t)
	e.populate()
	ctx := context.Background()

	before, _ := e.idx.Closest(ctx, e.site.URL+"/css/main.css", time.Time{})
	if !strings.HasPrefix(before.Location, "incoming/") {
		t.Fatalf("open epoch record should be in incoming: %s", before.Location)
	}

	cat, err := e.arc.Seal(ctx, ep)
	if err != nil {
		t.Fatal(err)
	}
	if cat.State != index.StateVerified || cat.Verification == nil || !cat.Verification.OK || cat.Pages != 3 || cat.Revisits == 0 {
		t.Fatalf("catalog %+v", cat)
	}
	if len(cat.Volumes) != 1 || cat.Volumes[0].Key != "weekly/2026/W41/2026-W41-0001.wacz" || cat.Package != "" {
		t.Fatalf("volumes %+v", cat.Volumes)
	}
	sumFile, _ := storage.ReadAll(ctx, e.store, ep.SumsKey())
	if string(sumFile) != cat.Volumes[0].SHA256+"  2026-W41-0001.wacz\n" || sha256hex(sumFile) != cat.SHA256 {
		t.Fatalf("SHA256SUMS %q", sumFile)
	}
	stored, err := e.arc.ReadCatalog(ctx, ep)
	if err != nil || stored.State != index.StateVerified || stored.Captures != 3 || stored.Volumes[0].CapturesList != "weekly/2026/W41/2026-W41-0001.captures.jsonl" ||
		stored.Verification.Captures != 3 || stored.Verification.Records != stored.Records || stored.Verification.Scanned <= stored.Records {
		t.Fatalf("stored catalog %+v %v", stored, err)
	}
	m, err := e.arc.ReadManifest(ctx, ep.VolumeCapturesKey(1))
	if err != nil || len(m.Captures) != 3 || m.SHA256 != cat.Volumes[0].SHA256 || m.Indexed != cat.Records || m.Pages != 3 {
		t.Fatalf("captures list %+v %v", m, err)
	}
	info, _ := e.idx.Epoch(ctx, "", ep)
	if info.State != index.StateVerified || info.SHA256 != cat.SHA256 {
		t.Fatalf("index epoch %+v", info)
	}

	recs, _ := e.idx.EpochRecords(ctx, "", ep)
	for _, r := range recs {
		if r.Location != ep.VolumeKey(1) {
			t.Fatalf("record %s not relocated: %s", r.URI, r.Location)
		}
		rec, err := e.arc.ReadRecord(ctx, r)
		if err != nil || rec.ID() != r.ID {
			t.Fatalf("read %s: %v", r.URI, err)
		}
	}

	page, err := e.idx.Closest(ctx, e.site.URL+"/news/article2.html", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.arc.Fetch(ctx, page)
	if err != nil || !bytes.Contains(res.Body, []byte("Nowa linia tramwajowa")) || res.Status != 200 {
		t.Fatalf("fetch page: %v", err)
	}
	css, _ := e.idx.Timeline(ctx, e.site.URL+"/css/main.css")
	var revisit index.Record
	for _, r := range css {
		if r.Type == warcio.TypeRevisit && r.Epoch == ep {
			revisit = r
		}
	}
	if revisit.ID == "" {
		t.Fatal("expected a revisit for shared CSS")
	}
	res, err = e.arc.Fetch(ctx, revisit)
	if err != nil || !bytes.Contains(res.Body, []byte("@import")) || res.PayloadFrom.ID == revisit.ID {
		t.Fatalf("revisit fetch: %+v %v", res, err)
	}
	raw, err := e.arc.RawRecord(ctx, revisit)
	if err != nil || raw[0] != 0x1f || raw[1] != 0x8b {
		t.Fatalf("raw record: %v", err)
	}

	pages, _ := e.idx.PageCaptures(ctx, e.site.URL+"/news/article2.html")
	meta, err := e.arc.PageMetadata(ctx, pages[0])
	if err != nil || !strings.Contains(meta.Text, "tramwajowej") {
		t.Fatalf("page metadata after seal: %v", err)
	}

	if _, err := e.arc.Seal(ctx, ep); !errors.Is(err, archive.ErrAlreadySealed) {
		t.Fatalf("second seal: %v", err)
	}
	if _, err := e.arc.Seal(ctx, "2020-W01"); !errors.Is(err, archive.ErrEmptyEpoch) {
		t.Fatalf("empty seal: %v", err)
	}
}

// TestIndexReproducible checks that the index is rebuildable: dump, drop the index,
// reindex from storage, dump again; the results must be identical.
func TestIndexReproducible(t *testing.T) {
	e := newEnv(t)
	e.populate()
	ctx := context.Background()
	if _, err := e.arc.Seal(ctx, ep); err != nil {
		t.Fatal(err)
	}
	want := e.dump()
	wantHits, _ := e.idx.CountMatches(ctx, "tramwajowa")

	e.arc.Close()
	e.idx.Close()
	e.openIndex()
	var progress []epoch.ID
	rep, err := e.arc.Reindex(ctx, archive.ReindexOptions{Progress: func(p epoch.ID, done, total int) { progress = append(progress, p) }})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Epochs) != 2 || len(rep.Scanned) != 0 || len(progress) != 2 {
		t.Fatalf("report %+v progress %v", rep, progress)
	}
	if got := e.dump(); got != want {
		t.Fatalf("index differs after reindex:\n%s", diff(want, got))
	}
	if hits, _ := e.idx.CountMatches(ctx, "tramwajowa"); hits != wantHits || hits == 0 {
		t.Fatalf("search hits %d, want %d", hits, wantHits)
	}

	rep, err = e.arc.Reindex(ctx, archive.ReindexOptions{})
	if err != nil || len(rep.Epochs) != 0 {
		t.Fatalf("consistent index must not be rebuilt: %+v %v", rep, err)
	}
	rep, err = e.arc.Reindex(ctx, archive.ReindexOptions{Force: true, Epoch: ep})
	if err != nil || len(rep.Epochs) != 1 || e.dump() != want {
		t.Fatalf("forced reindex: %+v %v", rep, err)
	}
}

// TestReindexWithCorruptCDXJ checks the fallback to scanning WARC data.
func TestReindexWithCorruptCDXJ(t *testing.T) {
	e := newEnv(t)
	e.populate()
	ctx := context.Background()
	if _, err := e.arc.Seal(ctx, ep); err != nil {
		t.Fatal(err)
	}
	want := e.dump()

	pkg, _ := storage.ReadAll(ctx, e.store, ep.VolumeKey(1))
	broken := rewriteEntry(t, pkg, wacz.PathCDXJ, []byte("this is not cdxj\n"))
	fs := e.store.(*storage.FS)
	if err := storage.PutBytes(ctx, fs, ep.VolumeKey(1), broken); err != nil {
		t.Fatal(err)
	}
	e.arc.Close()

	e.idx.Close()
	e.openIndex()
	rep, err := e.arc.Reindex(ctx, archive.ReindexOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Scanned) != 1 || rep.Scanned[0] != ep {
		t.Fatalf("expected scan fallback: %+v", rep)
	}
	if got := e.dump(); got != want {
		t.Fatalf("index differs after scan reindex:\n%s", diff(want, got))
	}

	v, err := e.arc.Verify(ctx, ep)
	if err != nil {
		t.Fatal(err)
	}
	if v.OK {
		t.Fatal("verification must fail for a tampered package")
	}
	info, _ := e.idx.Epoch(ctx, "", ep)
	if info.State != index.StateFailed || info.Error == "" {
		t.Fatalf("epoch %+v", info)
	}
}

// rewriteEntry replaces one stored entry, keeping the order of the others.
func rewriteEntry(t *testing.T, pkg []byte, name string, content []byte) []byte {
	zr, err := zip.NewReader(bytes.NewReader(pkg), int64(len(pkg)))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	for _, f := range zr.File {
		if f.Name == name {
			w, _ := zw.CreateHeader(&zip.FileHeader{Name: f.Name, Method: zip.Store, Modified: f.Modified})
			w.Write(content)
			continue
		}
		w, _ := zw.CreateRaw(&f.FileHeader)
		rc, _ := f.OpenRaw()
		io.Copy(w, rc)
	}
	zw.Close()
	return out.Bytes()
}

func TestVerifyDetectsBitRot(t *testing.T) {
	e := newEnv(t)
	e.populate()
	ctx := context.Background()
	if _, err := e.arc.Seal(ctx, ep); err != nil {
		t.Fatal(err)
	}
	pkg, _ := storage.ReadAll(ctx, e.store, ep.VolumeKey(1))
	r, _ := e.idx.Closest(ctx, e.site.URL+"/news/article.html", e.arc.Now().AddDate(0, 0, -7))
	pkg[r.Offset+r.Length/2] ^= 0x01
	storage.PutBytes(ctx, e.store, ep.VolumeKey(1), pkg)
	e.arc.Close()
	v, err := e.arc.Verify(ctx, ep)
	if err != nil {
		t.Fatal(err)
	}
	if v.OK || len(v.Problems) < 2 {
		t.Fatalf("expected sha and record problems: %+v", v)
	}
	for _, p := range v.Problems {
		t.Logf("%s: %s", p.Path, p.Message)
	}
}

func TestCleanupAfterRetention(t *testing.T) {
	e := newEnv(t)
	e.populate()
	ctx := context.Background()
	if _, err := e.arc.Seal(ctx, ep); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.arc.Cleanup(ctx); n != 0 {
		t.Fatal("cleanup before retention")
	}
	e.arc.Now = func() time.Time { return time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC) }
	n, err := e.arc.Cleanup(ctx)
	if err != nil || n != 3 {
		t.Fatalf("cleanup %d %v", n, err)
	}
	left, _ := e.store.List(ctx, ep.IncomingPrefix())
	if len(left) != 0 {
		t.Fatal("incoming files left")
	}
	open, _ := e.store.List(ctx, ep.Next().IncomingPrefix())
	if len(open) != 1 {
		t.Fatal("open epoch files must stay")
	}
	cat, _ := e.arc.ReadCatalog(ctx, ep)
	if !cat.IncomingDeleted {
		t.Fatal("catalog not updated")
	}
	page, _ := e.idx.Closest(ctx, e.site.URL+"/news/article2.html", time.Time{})
	if _, err := e.arc.Fetch(ctx, page); err != nil {
		t.Fatalf("read after cleanup: %v", err)
	}
}

func TestWriteMissingIndexes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.capture("/news/article.html", "serwis-a", ep)
	b := e.capture("/news/article2.html", "serwis-a", ep)
	if n, err := e.arc.WriteMissingIndexes(ctx); err != nil || n != 2 {
		t.Fatalf("written %d %v", n, err)
	}
	for _, cf := range []recorder.CaptureFile{a, b} {
		got, err := storage.ReadAll(ctx, e.store, archive.IndexKey(cf.Key()))
		if err != nil {
			t.Fatal(err)
		}
		var want bytes.Buffer
		cdxj.Write(&want, cf.CDXJ())
		if !bytes.Equal(got, want.Bytes()) {
			t.Fatalf("index of %s differs from the recorder's:\n%s", cf.Name, diff(want.String(), string(got)))
		}
	}
	if n, _ := e.arc.WriteMissingIndexes(ctx); n != 0 {
		t.Fatalf("rewrote %d existing indexes", n)
	}

	if _, err := e.arc.Seal(ctx, ep); err != nil {
		t.Fatal(err)
	}
	e.store.Delete(ctx, archive.IndexKey(a.Key()))
	c := e.capture("/news/article.html", "serwis-a", ep.Next())
	if n, _ := e.arc.WriteMissingIndexes(ctx); n != 1 {
		t.Fatalf("written %d, want only the open epoch", n)
	}
	if _, err := e.store.Stat(ctx, archive.IndexKey(c.Key())); err != nil {
		t.Fatal(err)
	}
}

func TestSealIncludesUnindexedCaptures(t *testing.T) {
	e := newEnv(t)
	cf := e.capture("/news/article.html", "serwis-a", ep)
	e.idx.DeleteEpoch(context.Background(), "", ep)
	cat, err := e.arc.Seal(context.Background(), ep)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := e.arc.ReadManifest(context.Background(), ep.VolumeCapturesKey(1))
	if cat.Pages != 1 || cat.State != index.StateVerified || m.Captures[0].Key != cf.Key() {
		t.Fatalf("catalog %+v", cat)
	}
}

func TestReadme(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.arc.WriteReadme(ctx); err != nil {
		t.Fatal(err)
	}
	b, err := storage.ReadAll(ctx, e.store, archive.ReadmeKey)
	if err != nil || !bytes.Contains(b, []byte("WACZ 1.1.1")) || !bytes.Contains(b, []byte("revisit")) {
		t.Fatalf("readme: %v", err)
	}
	if err := e.arc.WriteReadme(ctx); err != nil {
		t.Fatal(err)
	}
}

func diff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	var out []string
	for i := 0; i < max(len(al), len(bl)); i++ {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			out = append(out, "- "+x, "+ "+y)
		}
		if len(out) > 20 {
			break
		}
	}
	return strings.Join(out, "\n")
}

func TestSealWritesPageTable(t *testing.T) {
	e := newEnv(t)
	e.populate()
	ctx := context.Background()
	cat, err := e.arc.Seal(ctx, ep)
	if err != nil {
		t.Fatal(err)
	}
	if cat.Parquet == nil || cat.Parquet.Key != "weekly/2026/W41.parquet" || cat.Parquet.Rows != cat.Pages || cat.State != index.StateVerified {
		t.Fatalf("catalog %+v / %+v", cat, cat.Parquet)
	}
	raw, err := storage.ReadAll(ctx, e.store, cat.Parquet.Key)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := archive.ReadPageTable(bytes.NewReader(raw), int64(len(raw)))
	if err != nil || len(rows) != cat.Pages {
		t.Fatalf("%d rows, %v", len(rows), err)
	}
	for i := 1; i < len(rows); i++ {
		if rows[i-1].URL > rows[i].URL {
			t.Fatalf("rows not sorted by url: %s > %s", rows[i-1].URL, rows[i].URL)
		}
	}
	var art2 *archive.PageRow
	for i := range rows {
		if strings.HasSuffix(rows[i].URL, "/news/article2.html") {
			art2 = &rows[i]
		}
	}
	if art2 == nil {
		t.Fatal("article2 missing")
		return
	}
	if art2.Description != "Miasto ogłosiło przetarg na nową linię tramwajową." || strings.Join(art2.SchemaTypes, ",") != "NewsArticle" ||
		strings.Join(art2.Authors, ",") != "Anna Kowalska" || art2.Published.Format(time.DateOnly) != "2026-10-08" ||
		!strings.Contains(art2.SchemaOrg, `"headline": "Nowa linia tramwajowa"`) || art2.WARCFile == "" || art2.RecordID == "" {
		t.Errorf("article2 row %+v", *art2)
	}

	f, err := parquet.OpenFile(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	for _, rg := range f.RowGroups() {
		sc := rg.SortingColumns()
		if len(sc) == 0 || strings.Join(sc[0].Path(), ".") != "url" {
			t.Errorf("row group sorting %v", sc)
		}
	}
	urlCol, _ := f.Schema().Lookup("url")
	if f.RowGroups()[0].ColumnChunks()[urlCol.ColumnIndex].BloomFilter() == nil {
		t.Error("no bloom filter on url")
	}
	if v, ok := f.Lookup("hoarfrost.epoch"); !ok || v != string(ep) {
		t.Errorf("epoch metadata %q", v)
	}

	// Damage is found by verification; a missing table is written again.
	if err := storage.PutBytes(ctx, e.store, cat.Parquet.Key, raw[:len(raw)/2]); err != nil {
		t.Fatal(err)
	}
	if v, _ := e.arc.Verify(ctx, ep); v.OK {
		t.Fatal("damaged page table passed verification")
	}
	cat, _ = e.arc.ReadCatalog(ctx, ep)
	cat.Parquet = nil
	e.arc.WriteCatalog(ctx, cat)
	e.idx.SetEpoch(ctx, index.EpochInfo{Epoch: ep, State: index.StateVerified, WACZKey: cat.Location(), SHA256: cat.SHA256, Size: cat.Size, SealedAt: cat.SealedAt})
	done, err := e.arc.WriteMissingParquet(ctx)
	if err != nil || len(done) != 1 || done[0] != ep {
		t.Fatalf("written %v %v", done, err)
	}
	if v, _ := e.arc.Verify(ctx, ep); !v.OK {
		t.Fatalf("after rewrite: %+v", v.Problems)
	}
	if done, _ := e.arc.WriteMissingParquet(ctx); len(done) != 0 {
		t.Fatalf("rewrote %v", done)
	}
}

// flakyStore fails the first write of keys with the given suffixes, like a
// process stopped at that step of sealing, and counts package uploads.
type flakyStore struct {
	storage.Storage
	fail     map[string]bool
	failRead map[string]bool
	packages int
}

func (f *flakyStore) Open(ctx context.Context, key string) (storage.Reader, error) {
	for suffix := range f.failRead {
		if strings.HasSuffix(key, suffix) {
			delete(f.failRead, suffix)
			return nil, errors.New("connection reset")
		}
	}
	return f.Storage.Open(ctx, key)
}

func (f *flakyStore) failing(key string) bool {
	for suffix := range f.fail {
		if strings.HasSuffix(key, suffix) {
			delete(f.fail, suffix)
			return true
		}
	}
	return false
}

func (f *flakyStore) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if f.failing(key) {
		return errors.New("killed")
	}
	if strings.HasSuffix(key, ".wacz") {
		f.packages++
	}
	return f.Storage.Put(ctx, key, r, size)
}

func (f *flakyStore) Create(ctx context.Context, key string, hint int64) (storage.Writer, error) {
	if f.failing(key) {
		return nil, errors.New("killed")
	}
	return storage.Create(ctx, f.Storage, key, hint)
}

// TestSealResumesAfterPackageStored: a seal stopped after the volume and
// its captures list were stored (here: writing the page table) continues
// from there without packaging again.
func TestSealResumesAfterPackageStored(t *testing.T) {
	e := newEnv(t)
	e.populate()
	ctx := context.Background()
	flaky := &flakyStore{Storage: e.store, fail: map[string]bool{".parquet": true}}
	e.arc.Storage = flaky
	if _, err := e.arc.Seal(ctx, ep); err == nil {
		t.Fatal("seal should fail")
	}
	if info, _ := e.idx.Epoch(ctx, "", ep); info.State != index.StateFailed || !strings.Contains(info.Error, "killed") {
		t.Fatalf("epoch %+v", info)
	}
	cat, err := e.arc.Seal(ctx, ep)
	if err != nil {
		t.Fatal(err)
	}
	if flaky.packages != 1 || cat.State != index.StateVerified || cat.Pages != 3 {
		t.Fatalf("packages built %d, catalog %+v", flaky.packages, cat)
	}
}

// TestSealStartsOverWithoutCapturesList: a volume stored without its
// captures list cannot be trusted and is built again.
func TestSealStartsOverWithoutCapturesList(t *testing.T) {
	e := newEnv(t)
	e.populate()
	ctx := context.Background()
	flaky := &flakyStore{Storage: e.store, fail: map[string]bool{".captures.jsonl": true}}
	e.arc.Storage = flaky
	if _, err := e.arc.Seal(ctx, ep); err == nil {
		t.Fatal("seal should fail")
	}
	if _, err := e.store.Stat(ctx, ep.VolumeKey(1)); err != nil {
		t.Fatalf("volume of the failed seal: %v", err)
	}
	cat, err := e.arc.Seal(ctx, ep)
	if err != nil {
		t.Fatal(err)
	}
	if flaky.packages != 2 || cat.State != index.StateVerified {
		t.Fatalf("packages built %d, catalog %+v", flaky.packages, cat)
	}
}

// TestVerifyFindsCaptureMissingFromPackage: a capture the index knows of
// the week but which is not in the package fails verification.
func TestVerifyFindsCaptureMissingFromPackage(t *testing.T) {
	e := newEnv(t)
	e.populate()
	ctx := context.Background()
	if _, err := e.arc.Seal(ctx, ep); err != nil {
		t.Fatal(err)
	}
	lost := recorder.CaptureFile{Name: "20261012T080000Z-serwis_a-lost.warc.gz", Size: 1000, Epoch: ep,
		Records: []recorder.RecordInfo{{ID: "<urn:uuid:lost>", Type: warcio.TypeResponse, URI: "https://lost.example/", Date: e.arc.Now(), Offset: 0, Length: 1000}}}
	if err := e.idx.AddCapture(ctx, lost, lost.Key()); err != nil {
		t.Fatal(err)
	}
	v, err := e.arc.Verify(ctx, ep)
	if err != nil {
		t.Fatal(err)
	}
	var msgs []string
	for _, p := range v.Problems {
		msgs = append(msgs, p.Message)
	}
	all := strings.Join(msgs, "\n")
	if v.OK || !strings.Contains(all, "capture "+lost.Name+" of the week is not in its packages") ||
		!strings.Contains(all, "1 indexed record(s) of the week are not in its packages") {
		t.Fatalf("problems:\n%s", all)
	}
	if info, _ := e.idx.Epoch(ctx, "", ep); info.State != index.StateFailed || !strings.Contains(info.Error, "more problems") {
		t.Fatalf("epoch %+v", info)
	}
}

// TestSealToS3 seals into an S3 bucket: each volume is uploaded with one
// request and verified by reading it back.
func TestSealToS3(t *testing.T) {
	e := newEnv(t)
	backend := s3mem.New()
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(srv.Close)
	backend.CreateBucket("archive")
	u, _ := url.Parse(srv.URL)
	s3, err := storage.NewS3(context.Background(), storage.S3Config{Endpoint: u.Host, Bucket: "archive", Prefix: "press",
		Region: "us-east-1", AccessKey: "key", SecretKey: "secret", PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	e.store = s3
	e.arc.Storage = s3
	e.populate()
	ctx := context.Background()
	var phases []string
	cat, err := e.arc.SealWith(ctx, ep, archive.SealOptions{Progress: func(phase string, done, total int64) {
		if len(phases) == 0 || phases[len(phases)-1] != phase {
			phases = append(phases, phase)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if cat.State != index.StateVerified || cat.Captures != 3 {
		t.Fatalf("catalog %+v", cat)
	}
	want := []string{archive.PhasePackaging, archive.PhaseRelocating, archive.PhasePageTable, archive.PhaseVerifying}
	if strings.Join(phases, ",") != strings.Join(want, ",") {
		t.Fatalf("phases %v", phases)
	}
	page, _ := e.idx.Closest(ctx, e.site.URL+"/news/article2.html", time.Time{})
	if res, err := e.arc.Fetch(ctx, page); err != nil || !bytes.Contains(res.Body, []byte("Nowa linia tramwajowa")) {
		t.Fatalf("fetch from S3 package: %v", err)
	}
}

// TestVerifyAfterReindex: an index rebuilt from the package (without the
// capture files of the week) still verifies.
func TestVerifyAfterReindex(t *testing.T) {
	e := newEnv(t)
	e.populate()
	ctx := context.Background()
	if _, err := e.arc.Seal(ctx, ep); err != nil {
		t.Fatal(err)
	}
	e.arc.Close()
	e.idx.Close()
	e.openIndex()
	if _, err := e.arc.Reindex(ctx, archive.ReindexOptions{}); err != nil {
		t.Fatal(err)
	}
	v, err := e.arc.Verify(ctx, ep)
	if err != nil || !v.OK || v.Captures != 3 {
		t.Fatalf("verification after reindex: %+v %v", v, err)
	}
}

// TestSealKeepsPackageOnReadError: a transient error reading the captures
// list of a stored package (which the index may already point to) fails
// the attempt instead of replacing the package.
func TestSealKeepsPackageOnReadError(t *testing.T) {
	e := newEnv(t)
	e.populate()
	ctx := context.Background()
	flaky := &flakyStore{Storage: e.store, fail: map[string]bool{".parquet": true}, failRead: map[string]bool{}}
	e.arc.Storage = flaky
	if _, err := e.arc.Seal(ctx, ep); err == nil {
		t.Fatal("seal should fail")
	}
	flaky.failRead[".captures.jsonl"] = true
	if _, err := e.arc.Seal(ctx, ep); err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("seal with unreadable captures list: %v", err)
	}
	cat, err := e.arc.Seal(ctx, ep)
	if err != nil || flaky.packages != 1 || cat.State != index.StateVerified {
		t.Fatalf("packages built %d, %+v %v", flaky.packages, cat, err)
	}
}

// TestSealOfSealedWeek: sealing a sealed week again keeps its state, and a
// package whose catalog was written by a process stopped before
// verification is verified.
func TestSealOfSealedWeek(t *testing.T) {
	e := newEnv(t)
	e.populate()
	ctx := context.Background()
	cat, err := e.arc.Seal(ctx, ep)
	if err != nil {
		t.Fatal(err)
	}
	e.idx.SetEpoch(ctx, index.EpochInfo{Epoch: ep, State: index.StateSealing})
	if _, err := e.arc.Seal(ctx, ep); !errors.Is(err, archive.ErrAlreadySealed) {
		t.Fatal(err)
	}
	if info, _ := e.idx.Epoch(ctx, "", ep); info.State != index.StateVerified || info.WACZKey != cat.Location() || info.SHA256 != cat.SHA256 {
		t.Fatalf("state not restored: %+v", info)
	}
	cat.Verification, cat.State = nil, index.StateSealed
	e.arc.WriteCatalog(ctx, cat)
	got, err := e.arc.Seal(ctx, ep)
	if err != nil || got.State != index.StateVerified || got.Verification == nil {
		t.Fatalf("unverified catalog: %+v %v", got, err)
	}
}

// TestCleanupKeepsUnpackagedFiles: only capture files on the captures list
// are removed from incoming/.
func TestCleanupKeepsUnpackagedFiles(t *testing.T) {
	e := newEnv(t)
	e.populate()
	ctx := context.Background()
	if _, err := e.arc.Seal(ctx, ep); err != nil {
		t.Fatal(err)
	}
	late := ep.IncomingPrefix() + "20261012T080000Z-serwis_a-late.warc.gz"
	storage.PutBytes(ctx, e.store, late, []byte("late"))
	e.arc.Now = func() time.Time { return time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC) }
	if n, err := e.arc.Cleanup(ctx); err != nil || n != 3 {
		t.Fatalf("cleanup %d %v", n, err)
	}
	left, _ := e.store.List(ctx, ep.IncomingPrefix())
	if len(left) != 1 || left[0].Key != late {
		t.Fatalf("left %+v", left)
	}
}

func sha256hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// TestSealIntoVolumes splits a week into one volume per capture file: the
// volumes, SHA256SUMS and the collection are stored, a revisit in one
// volume points to its original in another, and the index is rebuilt from
// the volumes.
func TestSealIntoVolumes(t *testing.T) {
	e := newEnv(t)
	e.arc.VolumeSize = 1
	e.arc.ConcatBySource = true
	e.populate()
	ctx := context.Background()
	cat, err := e.arc.Seal(ctx, ep)
	if err != nil {
		t.Fatal(err)
	}
	if cat.State != index.StateVerified || len(cat.Volumes) != 3 || cat.Captures != 3 || cat.Location() != "weekly/2026/W41/" {
		t.Fatalf("catalog %+v", cat)
	}
	var size int64
	var sums strings.Builder
	files := map[string]bool{}
	for i, v := range cat.Volumes {
		if v.Key != ep.VolumeKey(i+1) || v.Captures != 1 {
			t.Fatalf("volume %d: %+v", i+1, v)
		}
		size += v.Size
		sums.WriteString(v.SHA256 + "  " + strings.TrimPrefix(v.Key, ep.VolumeDir()) + "\n")
		m, err := e.arc.ReadManifest(ctx, v.CapturesList)
		if err != nil {
			t.Fatal(err)
		}
		if files[m.Captures[0].File] {
			t.Fatalf("WARC file %s in two volumes", m.Captures[0].File)
		}
		files[m.Captures[0].File] = true
	}
	if size != cat.Size || !files["serwis_a-0001.warc.gz"] || !files["serwis_a-0002.warc.gz"] {
		t.Fatalf("size %d of %d, files %v", size, cat.Size, files)
	}
	got, _ := storage.ReadAll(ctx, e.store, ep.SumsKey())
	if string(got) != sums.String() || sha256hex(got) != cat.SHA256 {
		t.Fatalf("SHA256SUMS %q", got)
	}
	var coll archive.Collection
	raw, _ := storage.ReadAll(ctx, e.store, ep.CollectionKey())
	if err := json.Unmarshal(raw, &coll); err != nil || coll.Profile != "multi-wacz-package" || len(coll.Resources) != 3 ||
		coll.Resources[1].Path != "2026-W41-0002.wacz" || coll.Resources[1].Hash != "sha256:"+cat.Volumes[1].SHA256 {
		t.Fatalf("collection %s %v", raw, err)
	}

	css, _ := e.idx.Timeline(ctx, e.site.URL+"/css/main.css")
	var revisit index.Record
	for _, r := range css {
		if r.Type == warcio.TypeRevisit && r.Epoch == ep {
			revisit = r
		}
	}
	orig, err := e.idx.Record(ctx, revisit.RefersTo)
	if err != nil || revisit.Location == orig.Location || !strings.HasPrefix(revisit.Location, ep.VolumeDir()) {
		t.Fatalf("revisit in %s, original in %s: %v", revisit.Location, orig.Location, err)
	}
	if res, err := e.arc.Fetch(ctx, revisit); err != nil || !bytes.Contains(res.Body, []byte("@import")) {
		t.Fatalf("revisit across volumes: %v", err)
	}

	want := e.dump()
	e.arc.Close()
	e.idx.Close()
	e.openIndex()
	e.arc.VolumeSize = 1
	if _, err := e.arc.Reindex(ctx, archive.ReindexOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := e.dump(); got != want {
		t.Fatalf("index differs after reindex:\n%s", diff(want, got))
	}
	if v, err := e.arc.Verify(ctx, ep); err != nil || !v.OK || v.Captures != 3 {
		t.Fatalf("verify after reindex: %+v %v", v, err)
	}

	// A file left in the volume directory would be loaded with the week.
	storage.PutBytes(ctx, e.store, ep.VolumeDir()+"2026-W41-0004.wacz", []byte("stray"))
	v, err := e.arc.Verify(ctx, ep)
	if err != nil || v.OK || v.Problems[0].Path != ep.VolumeDir()+"2026-W41-0004.wacz" || v.Problems[0].Message != "is not part of the week" {
		t.Fatalf("stray volume: %+v %v", v, err)
	}
}

// TestSealResumesWithNextVolume: a seal stopped while storing a volume
// keeps the volumes stored before and continues with that one.
func TestSealResumesWithNextVolume(t *testing.T) {
	e := newEnv(t)
	e.arc.VolumeSize = 1
	e.populate()
	ctx := context.Background()
	flaky := &flakyStore{Storage: e.store, fail: map[string]bool{"-0002.wacz": true}}
	e.arc.Storage = flaky
	if _, err := e.arc.Seal(ctx, ep); err == nil || !strings.Contains(err.Error(), "volume 2") {
		t.Fatalf("seal should fail at volume 2: %v", err)
	}
	if flaky.packages != 1 {
		t.Fatalf("volumes stored %d", flaky.packages)
	}
	first, _ := e.store.Stat(ctx, ep.VolumeKey(1))
	cat, err := e.arc.Seal(ctx, ep)
	if err != nil {
		t.Fatal(err)
	}
	if flaky.packages != 3 || cat.State != index.StateVerified || len(cat.Volumes) != 3 || cat.Volumes[0].Size != first.Size {
		t.Fatalf("volumes stored %d, catalog %+v", flaky.packages, cat)
	}
}

// TestSealFinishesSinglePackage: a week whose single package and captures
// list were stored by an older version is finished as it is, verified and
// rebuilt from storage.
func TestSealFinishesSinglePackage(t *testing.T) {
	e := newEnv(t)
	e.populate()
	ctx := context.Background()
	objs, _ := e.store.List(ctx, ep.IncomingPrefix())
	keys := map[string]string{}
	var inputs []wacz.Input
	for _, o := range objs {
		if !strings.HasSuffix(o.Key, ".warc.gz") {
			continue
		}
		key := o.Key
		keys[filepath.Base(key)] = key
		inputs = append(inputs, wacz.Input{Name: filepath.Base(key), Size: o.Size, Open: func(ctx context.Context) (io.ReadCloser, error) {
			rc, _, err := storage.Stream(ctx, e.store, key)
			return rc, err
		}})
	}
	m := &archive.Manifest{Epoch: ep, Package: ep.WeeklyKey(), Created: e.arc.Now()}
	var buf bytes.Buffer
	res, err := wacz.Build(ctx, &buf, inputs, wacz.Options{Epoch: ep, Created: m.Created, OnCapture: func(cf recorder.CaptureFile, p wacz.Placement) error {
		c := archive.ManifestCapture{Key: keys[cf.Name], File: p.File, Offset: p.Offset, Size: p.Size, SHA256: p.SHA256, Records: len(cf.Records), Pages: len(cf.Pages)}
		for _, r := range cf.Records {
			if r.Indexable() {
				c.Indexed++
			}
		}
		m.Indexed += c.Indexed
		m.Captures = append(m.Captures, c)
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	storage.PutBytes(ctx, e.store, ep.WeeklyKey(), buf.Bytes())
	storage.PutBytes(ctx, e.store, ep.ChecksumKey(), []byte(wacz.ChecksumLine(res.SHA256, "W41.wacz")))
	m.SHA256, m.Size, m.WARCFiles, m.Pages = res.SHA256, res.Size, len(res.Files), res.Pages
	if err := e.arc.WriteManifest(ctx, ep.CapturesKey(), m); err != nil {
		t.Fatal(err)
	}

	cat, err := e.arc.Seal(ctx, ep)
	if err != nil {
		t.Fatal(err)
	}
	if cat.Package != ep.WeeklyKey() || len(cat.Volumes) != 0 || cat.Location() != ep.WeeklyKey() || cat.State != index.StateVerified || cat.Verification.Captures != 3 {
		t.Fatalf("catalog %+v %+v", cat, cat.Verification)
	}
	if vols, _ := e.store.List(ctx, ep.VolumeDir()); len(vols) != 0 {
		t.Fatalf("volumes written: %+v", vols)
	}
	want := e.dump()
	e.arc.Close()
	e.idx.Close()
	e.openIndex()
	if _, err := e.arc.Reindex(ctx, archive.ReindexOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := e.dump(); got != want {
		t.Fatalf("index differs after reindex:\n%s", diff(want, got))
	}
	if v, err := e.arc.Verify(ctx, ep); err != nil || !v.OK {
		t.Fatalf("verify: %+v %v", v, err)
	}
}

// TestReindexReportsMissingVolume: a sealed week with a volume missing
// from storage is marked failed, not rebuilt from incoming/ as open.
func TestReindexReportsMissingVolume(t *testing.T) {
	e := newEnv(t)
	e.arc.VolumeSize = 1
	e.populate()
	ctx := context.Background()
	if _, err := e.arc.Seal(ctx, ep); err != nil {
		t.Fatal(err)
	}
	e.store.Delete(ctx, ep.VolumeKey(2))
	e.arc.Close()
	e.idx.Close()
	e.openIndex()
	rep, err := e.arc.Reindex(ctx, archive.ReindexOptions{})
	if err != nil || len(rep.Missing) != 1 || rep.Missing[0] != ep {
		t.Fatalf("report %+v %v", rep, err)
	}
	info, _ := e.idx.Epoch(ctx, "", ep)
	if info.State != index.StateFailed || !strings.Contains(info.Error, ep.VolumeKey(2)) {
		t.Fatalf("epoch %+v", info)
	}
	if files, _ := e.idx.Files(ctx, "", ep); len(files) != 0 {
		t.Fatalf("week indexed from incoming/: %v", files)
	}
}

// TestSealKeepsVolumesTheIndexPointsTo: a resumed seal never removes a
// volume without its captures list when the index already points into it.
func TestSealKeepsVolumesTheIndexPointsTo(t *testing.T) {
	e := newEnv(t)
	e.arc.VolumeSize = 1
	e.populate()
	ctx := context.Background()
	flaky := &flakyStore{Storage: e.store, fail: map[string]bool{".parquet": true}}
	e.arc.Storage = flaky
	if _, err := e.arc.Seal(ctx, ep); err == nil {
		t.Fatal("seal should fail")
	}
	// The index points into the volumes now; the list of volume 3 is lost.
	e.store.Delete(ctx, ep.VolumeCapturesKey(3))
	_, err := e.arc.Seal(ctx, ep)
	if err == nil || !strings.Contains(err.Error(), "resolve by hand") {
		t.Fatalf("seal: %v", err)
	}
	if _, err := e.store.Stat(ctx, ep.VolumeKey(3)); err != nil {
		t.Fatalf("volume removed: %v", err)
	}
	// A later list after a missing one is not removed either.
	e.store.Delete(ctx, ep.VolumeCapturesKey(2))
	storage.PutBytes(ctx, e.store, ep.VolumeCapturesKey(3), []byte("x"))
	if _, err := e.arc.Seal(ctx, ep); err == nil || !strings.Contains(err.Error(), "resolve by hand") {
		t.Fatalf("seal: %v", err)
	}
}
