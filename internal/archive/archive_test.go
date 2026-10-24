package archive_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ogefest/hoarfrost/internal/pgdb/pgtest"

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
	sumFile, _ := storage.ReadAll(ctx, e.store, ep.ChecksumKey())
	if string(sumFile) != cat.SHA256+"  W41.wacz\n" {
		t.Fatalf("sha256 file %q", sumFile)
	}
	stored, err := e.arc.ReadCatalog(ctx, ep)
	if err != nil || stored.State != index.StateVerified || len(stored.Incoming) != 3 {
		t.Fatalf("stored catalog %+v %v", stored, err)
	}
	info, _ := e.idx.Epoch(ctx, "", ep)
	if info.State != index.StateVerified || info.SHA256 != cat.SHA256 {
		t.Fatalf("index epoch %+v", info)
	}

	recs, _ := e.idx.EpochRecords(ctx, "", ep)
	for _, r := range recs {
		if r.Location != ep.WeeklyKey() {
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

	pkg, _ := storage.ReadAll(ctx, e.store, ep.WeeklyKey())
	broken := rewriteEntry(t, pkg, wacz.PathCDXJ, []byte("this is not cdxj\n"))
	fs := e.store.(*storage.FS)
	if err := storage.PutBytes(ctx, fs, ep.WeeklyKey(), broken); err != nil {
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
	pkg, _ := storage.ReadAll(ctx, e.store, ep.WeeklyKey())
	r, _ := e.idx.Closest(ctx, e.site.URL+"/news/article.html", e.arc.Now().AddDate(0, 0, -7))
	pkg[r.Offset+r.Length/2] ^= 0x01
	storage.PutBytes(ctx, e.store, ep.WeeklyKey(), pkg)
	e.arc.Close()
	v, err := e.arc.Verify(ctx, ep)
	if err != nil {
		t.Fatal(err)
	}
	if v.OK || len(v.Problems) < 2 {
		t.Fatalf("expected sha and record problems: %+v", v)
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
	if cat.Pages != 1 || cat.Incoming[0] != cf.Key() {
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
	e.idx.SetEpoch(ctx, index.EpochInfo{Epoch: ep, State: index.StateVerified, WACZKey: cat.Package, SHA256: cat.SHA256, Size: cat.Size, SealedAt: cat.SealedAt})
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
