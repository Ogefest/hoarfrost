// Package conformance checks archives written by hoarfrost with independent
// implementations of the standards (warcio, cdxj-indexer, py-wacz, pywb).
// The archive must be usable by these tools without hoarfrost; this is the
// project's main compatibility guarantee.
//
// Tests are skipped when a tool is missing, unless HOARFROST_CONFORMANCE is
// set to "required" (as in CI), in which case a missing tool fails the test.
package conformance

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ogefest/hoarfrost/internal/pgdb/pgtest"

	"github.com/ogefest/hoarfrost/internal/archive"
	"github.com/ogefest/hoarfrost/internal/engine"
	"github.com/ogefest/hoarfrost/internal/engine/static"
	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/index"
	"github.com/ogefest/hoarfrost/internal/recorder"
	"github.com/ogefest/hoarfrost/internal/storage"
	"github.com/ogefest/hoarfrost/internal/testsite"
	"github.com/ogefest/hoarfrost/internal/upload"
	"github.com/ogefest/hoarfrost/internal/wacz"
)

// requireTool returns the path of an external tool or skips (fails when
// conformance is required).
func requireTool(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err == nil {
		return p
	}
	if os.Getenv("HOARFROST_CONFORMANCE") == "required" {
		t.Fatalf("%s is required for conformance tests (pip install warcio wacz pywb)", name)
	}
	t.Skipf("%s not installed", name)
	return ""
}

func run(t *testing.T, name string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(requireTool(t, name), args...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %v: %v\n%s\n%s", name, args, err, out.String(), errOut.String())
	}
	return out.Bytes()
}

// fixture is a sealed week produced by the real capture pipeline: pages with
// shared resources (revisits), a PDF attachment, a redirect, a 404, a
// page saved in a browser and capture metadata.
type fixture struct {
	site    *testsite.Site
	root    string // storage directory
	store   storage.Storage
	idx     *index.DB
	arc     *archive.Archive
	epoch   epoch.ID
	pkg     string   // local path of the WACZ
	warcs   []string // WARC files extracted from the package
	sealed  *archive.Catalog
	cssBody []byte
}

var (
	fixOnce   sync.Once
	fix       *fixture
	fixErr    error
	fixDir    string
	fixDB     string
	fixSchema string
)

func TestMain(m *testing.M) {
	code := m.Run()
	if fix != nil {
		fix.site.Close()
		fix.arc.Close()
		fix.idx.Close()
	}
	if fixDir != "" {
		os.RemoveAll(fixDir)
	}
	if fixSchema != "" {
		pgtest.DropSchema(fixDB, fixSchema)
	}
	os.Exit(code)
}

func getFixture(t *testing.T) *fixture {
	t.Helper()
	db := pgtest.URL(t)
	fixOnce.Do(func() { fix, fixErr = build(db) })
	if fixErr != nil {
		t.Fatal(fixErr)
	}
	return fix
}

var blocked = []string{"doubleclick.net", "youtube.com"}

func build(db string) (*fixture, error) {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "hoarfrost-conformance-")
	if err != nil {
		return nil, err
	}
	fixDir = dir
	f := &fixture{site: testsite.New(), root: filepath.Join(dir, "storage"), epoch: epoch.Of(time.Now())}
	if f.store, err = storage.NewFS(f.root); err != nil {
		return nil, err
	}
	fixDB, fixSchema = db, pgtest.NewSchemaName("conformance")
	if f.idx, err = index.Open(ctx, fixDB, fixSchema); err != nil {
		return nil, err
	}
	f.arc = archive.New(f.store, f.idx, filepath.Join(dir, "spool"), nil)

	commit := func(rec *recorder.Recorder) error {
		cf, err := rec.Close()
		if err != nil {
			return err
		}
		file, err := os.Open(cf.Path)
		if err != nil {
			return err
		}
		defer file.Close()
		if err := f.store.Put(ctx, cf.Key(), file, cf.Size); err != nil {
			return err
		}
		return f.idx.AddCapture(ctx, cf, cf.Key())
	}
	newRec := func(source, eng string) (*recorder.Recorder, error) {
		return recorder.New(recorder.Options{Epoch: f.epoch, Dedup: f.idx.Dedup(""), SpoolDir: filepath.Join(dir, "spool"), Source: source, Engine: eng,
			Blocked: func(h string) bool { return static.HostMatches(h, blocked) }})
	}
	jobs := []struct {
		path string
		eng  engine.Engine
	}{
		{"/news/article.html", static.Engine{}},
		{"/news/article2.html", static.Engine{}},
		{"/redirect", static.Engine{}},
		{"/files/uchwala.pdf", engine.HTTPEngine{}},
		{"/missing-page", static.Engine{}},
	}
	for _, j := range jobs {
		rec, err := newRec("serwis", j.eng.Name())
		if err != nil {
			return nil, err
		}
		_, _ = j.eng.Capture(ctx, engine.Job{URL: f.site.URL + j.path, Source: "serwis",
			Options: engine.Options{SkipFrames: blocked}}, rec)
		if err := commit(rec); err != nil {
			return nil, err
		}
	}
	rec, err := newRec("upload", "upload")
	if err != nil {
		return nil, err
	}
	html := []byte(`<!DOCTYPE html><html><!-- Page saved with SingleFile url: https://example.org/z-przegladarki --><head><title>Zapis z przeglądarki</title></head><body><p>Strona zapisana rozszerzeniem.</p></body></html>`)
	if _, err := upload.Store(ctx, rec, upload.Upload{Body: html}); err != nil {
		return nil, err
	}
	if err := commit(rec); err != nil {
		return nil, err
	}

	if f.sealed, err = f.arc.Seal(ctx, f.epoch); err != nil {
		return nil, err
	}
	data, err := storage.ReadAll(ctx, f.store, f.epoch.WeeklyKey())
	if err != nil {
		return nil, err
	}
	f.pkg = filepath.Join(dir, "week.wacz")
	if err := os.WriteFile(f.pkg, data, 0o644); err != nil {
		return nil, err
	}
	r, err := wacz.OpenFile(f.pkg)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	offsets, err := r.WARCOffsets()
	if err != nil {
		return nil, err
	}
	for name := range offsets {
		b, err := r.ReadEntry(wacz.ArchiveDir + name)
		if err != nil {
			return nil, err
		}
		p := filepath.Join(dir, "warc", name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return nil, err
		}
		f.warcs = append(f.warcs, p)
	}
	css, err := os.ReadFile("../internal/testsite/site/css/main.css")
	f.cssBody = css
	return f, err
}

func newRecorder(t *testing.T) *recorder.Recorder {
	r, err := recorder.New(recorder.Options{Epoch: "2026-W41", SpoolDir: t.TempDir(), Source: "upload", Engine: "upload"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Abort)
	return r
}
