package coordinator_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ogefest/hoarfrost/internal/pgdb/pgtest"

	"github.com/ogefest/hoarfrost/internal/appdb"
	"github.com/ogefest/hoarfrost/internal/archive"
	"github.com/ogefest/hoarfrost/internal/coordinator"
	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/index"
	"github.com/ogefest/hoarfrost/internal/queue"
	"github.com/ogefest/hoarfrost/internal/sourcestore"
	"github.com/ogefest/hoarfrost/internal/storage"
	"github.com/ogefest/hoarfrost/internal/testsite"
	"github.com/ogefest/hoarfrost/internal/warcio"
	"github.com/ogefest/hoarfrost/internal/worker"
)

const multiSources = `
defaults:
  skip_frames: [youtube.com]
  block: [doubleclick.net]
blocklists:
  urls: []
sources:
  - name: kraj
    urls: [SITE/news/article.html]
  - name: miasto
    storage: prasa
    urls: [SITE/news/article.html, SITE/news/article2.html]
`

type multiEnv struct {
	dir      string
	site     *testsite.Site
	storages map[string]storage.Storage
	idx      *index.DB
	lib      *archive.Library
	coord    *coordinator.Coordinator
	db       *sql.DB
}

func newMultiEnv(t *testing.T) *multiEnv {
	t.Helper()
	ctx := context.Background()
	m := &multiEnv{dir: t.TempDir(), site: testsite.New(), storages: map[string]storage.Storage{}}
	t.Cleanup(m.site.Close)
	for _, name := range []string{"default", "prasa"} {
		st, err := storage.NewFS(filepath.Join(m.dir, "storage-"+name))
		if err != nil {
			t.Fatal(err)
		}
		m.storages[name] = st
	}
	m.openIndex(t)
	appURL, appSchema := pgtest.Schema(t, "app")
	db, err := appdb.Open(ctx, appURL, appSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	m.db = db
	m.coord, err = coordinator.New(coordinator.Options{Library: m.lib, Queue: queue.New(db),
		Sources: sourcesIn(t, db, strings.ReplaceAll(multiSources, "SITE", m.site.URL)), LeaseDuration: time.Minute,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (m *multiEnv) openIndex(t *testing.T) {
	indexURL, indexSchema := pgtest.Schema(t, "index")
	idx, err := index.Open(context.Background(), indexURL, indexSchema)
	if err != nil {
		t.Fatal(err)
	}
	m.idx = idx
	m.lib = archive.NewLibrary(idx, m.storages, filepath.Join(m.dir, "spool"), nil)
	t.Cleanup(func() { m.lib.Close(); idx.Close() })
}

func (m *multiEnv) worker(name string, storages ...string) *worker.Worker {
	sts := map[string]storage.Storage{}
	for _, s := range storages {
		sts[s] = m.storages[s]
	}
	return &worker.Worker{Name: name, Coord: m.coord, Storages: sts, Engines: worker.DefaultEngines(),
		SpoolDir: filepath.Join(m.dir, "spool-"+name), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func drainWorker(t *testing.T, w *worker.Worker) int {
	n := 0
	for range 100 {
		ok, err := w.RunOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			return n
		}
		n++
	}
	t.Fatal("queue never drained")
	return n
}

func TestMultipleStorages(t *testing.T) {
	m := newMultiEnv(t)
	ctx := context.Background()
	if got := m.coord.Storages(); strings.Join(got, ",") != "default,prasa" {
		t.Fatalf("storages %v", got)
	}
	if err := m.coord.PollSources(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.coord.AddURL(ctx, coordinator.URLRequest{URL: m.site.URL + "/files/uchwala.pdf", Storage: "prasa"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.coord.AddURL(ctx, coordinator.URLRequest{URL: m.site.URL + "/x", Storage: "nope"}); err == nil {
		t.Fatal("unknown storage accepted")
	}

	defaultOnly := m.worker("default-only", "default")
	if n := drainWorker(t, defaultOnly); n != 2 {
		t.Fatalf("worker without access to prasa ran %d jobs, want 2 (page and its attachment)", n)
	}
	drainWorker(t, m.worker("both", "default", "prasa"))
	counts, _ := m.coord.Queue().Counts(ctx)
	if counts[queue.Queued]+counts[queue.Failed] != 0 {
		t.Fatalf("counts %v", counts)
	}

	ep := epoch.Of(time.Now())
	files := map[string]int{}
	for name, st := range m.storages {
		objs, _ := st.List(ctx, ep.IncomingPrefix())
		for _, o := range objs {
			if strings.HasSuffix(o.Key, ".warc.gz") {
				files[name]++
			}
			if name == "default" && !strings.Contains(o.Key, "-kraj-") {
				t.Errorf("capture %s in the default storage", o.Key)
			}
			if name == "prasa" && strings.Contains(o.Key, "-kraj-") {
				t.Errorf("capture %s in prasa", o.Key)
			}
		}
	}
	if files["default"] != 2 || files["prasa"] != 3 {
		t.Fatalf("files per storage %v", files)
	}

	recs, _ := m.idx.EpochRecords(ctx, "prasa", ep)
	var cssFull bool
	for _, r := range recs {
		if strings.HasSuffix(r.URI, "/css/main.css") && r.Type == warcio.TypeResponse {
			cssFull = true
		}
		if r.Type == warcio.TypeRevisit {
			orig, err := m.idx.Record(ctx, r.RefersTo)
			if err != nil || orig.Storage != "prasa" {
				t.Fatalf("revisit %s refers outside its storage", r.URI)
			}
		}
	}
	if !cssFull {
		t.Fatal("the first capture of a resource in a storage must be a full record")
	}
	pages, n, _ := m.idx.Search(ctx, index.SearchQuery{Storage: "prasa"})
	if n != 3 || pages[0].Storage != "prasa" {
		t.Fatalf("pages in prasa: %d", n)
	}
	if _, err := m.lib.PageMetadata(ctx, pages[0]); err != nil {
		t.Fatalf("read page from its storage: %v", err)
	}

	for _, name := range []string{"default", "prasa"} {
		cat, err := m.coord.Seal(ctx, name, ep, false)
		if err != nil {
			t.Fatal(err)
		}
		if cat.State != index.StateVerified {
			t.Fatalf("%s: %+v", name, cat.Verification)
		}
		if _, err := m.storages[name].Stat(ctx, ep.VolumeKey(1)); err != nil {
			t.Fatalf("%s package missing", name)
		}
		if _, err := m.storages[name].Stat(ctx, archive.ReadmeKey); err == nil {
			continue
		}
	}
	if info, _ := m.idx.Epoch(ctx, "prasa", ep); info.State != index.StateVerified {
		t.Fatalf("prasa epoch %+v", info)
	}

	want, _ := m.idx.Dump(ctx)
	m.lib.Close()
	m.idx.Close()
	m.openIndex(t)
	for _, arc := range m.lib.All() {
		if _, err := arc.Reindex(ctx, archive.ReindexOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := m.idx.Dump(ctx); got != want {
		t.Fatal("index rebuilt from two storages differs")
	}
}

func TestUnknownStorageInConfig(t *testing.T) {
	m := newMultiEnv(t)
	next := *m.coord.Sources()
	next.Sources = append(next.Sources, next.Sources[0])
	next.Sources[2].Name = "inne"
	next.Sources[2].Storage = "brak"
	if err := m.coord.UpdateSources(context.Background(), &next); err == nil || !strings.Contains(err.Error(), "brak") {
		t.Fatalf("unknown storage accepted: %v", err)
	}
	snaps, _ := m.storages["prasa"].List(context.Background(), "config/sources/")
	if len(snaps) != 0 {
		t.Fatal("rejected configuration archived")
	}
	if err := m.coord.SnapshotSources(context.Background()); err != nil {
		t.Fatal(err)
	}
	for name, st := range m.storages {
		if snaps, _ := st.List(context.Background(), "config/sources/"); len(snaps) != 1 {
			t.Fatalf("%s lacks the configuration snapshot", name)
		}
	}

	// A storage removed from the environment does not stop the
	// coordinator: its sources report an error and queue nothing.
	ctx := context.Background()
	stale := m.coord.Sources().Clone()
	stale.Sources[0].Storage = "usuniety"
	if err := sourcestore.New(m.db).Save(ctx, stale); err != nil {
		t.Fatal(err)
	}
	coord, err := coordinator.New(coordinator.Options{Library: m.lib, Queue: queue.New(m.db), Sources: sourcestore.New(m.db),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal("stored configuration with an unknown storage blocks the start:", err)
	}
	name := stale.Sources[0].Name
	if err := coord.PollSource(ctx, name); err == nil || !strings.Contains(coord.SourceStatuses(ctx)[name].Error, "usuniety") {
		t.Fatalf("poll with unknown storage: %v %+v", err, coord.SourceStatuses(ctx)[name])
	}
}
