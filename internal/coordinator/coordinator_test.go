package coordinator_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ogefest/hoarfrost/internal/pgdb"
	"github.com/ogefest/hoarfrost/internal/pgdb/pgtest"

	"github.com/ogefest/hoarfrost/internal/appdb"
	"github.com/ogefest/hoarfrost/internal/archive"
	"github.com/ogefest/hoarfrost/internal/cdxj"
	"github.com/ogefest/hoarfrost/internal/config"
	"github.com/ogefest/hoarfrost/internal/coordinator"
	"github.com/ogefest/hoarfrost/internal/engine"
	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/index"
	"github.com/ogefest/hoarfrost/internal/queue"
	"github.com/ogefest/hoarfrost/internal/recorder"
	"github.com/ogefest/hoarfrost/internal/sourcestore"
	"github.com/ogefest/hoarfrost/internal/storage"
	"github.com/ogefest/hoarfrost/internal/testsite"
	"github.com/ogefest/hoarfrost/internal/warcio"
	"github.com/ogefest/hoarfrost/internal/worker"
)

type env struct {
	t         *testing.T
	idxURL    string
	idxSchema string
	dir       string
	site      *testsite.Site
	store     storage.Storage
	idx       *index.DB
	q         *queue.Queue
	coord     *coordinator.Coordinator
	api       *httptest.Server
	db        *sql.DB
}

const token = "worker-secret"

func newEnv(t *testing.T, sourcesYAML string, lease time.Duration) *env {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()
	e := &env{t: t, dir: dir, site: testsite.New()}
	t.Cleanup(e.site.Close)
	var err error
	if e.store, err = storage.NewFS(filepath.Join(dir, "storage")); err != nil {
		t.Fatal(err)
	}
	e.idxURL, e.idxSchema = pgtest.Schema(t, "index")
	if e.idx, err = index.Open(ctx, e.idxURL, e.idxSchema); err != nil {
		t.Fatal(err)
	}
	appURL, appSchema := pgtest.Schema(t, "app")
	db, err := appdb.Open(ctx, appURL, appSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(); e.idx.Close() })
	e.q = queue.New(db)
	e.db = db
	arc := archive.New(e.store, e.idx, filepath.Join(dir, "spool"), nil)
	e.coord, err = coordinator.New(coordinator.Options{Library: archive.Single(arc), Queue: e.q,
		Sources: sourcesIn(t, db, strings.ReplaceAll(sourcesYAML, "SITE", e.site.URL)), LeaseDuration: lease})
	if err != nil {
		t.Fatal(err)
	}
	e.api = httptest.NewServer(e.coord.InternalHandler(token))
	t.Cleanup(e.api.Close)
	return e
}

// sourcesIn stores a configuration written as YAML (the format of the
// copies in config/sources/) in the database.
func sourcesIn(t *testing.T, db *sql.DB, yaml string) *sourcestore.Store {
	t.Helper()
	cfg, err := config.ParseSources([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	st := sourcestore.New(db)
	if err := st.Save(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	return st
}

func (e *env) worker(name string, remote bool) *worker.Worker {
	var c worker.Coordinator = e.coord
	if remote {
		c = worker.NewHTTPCoordinator(e.api.URL, token)
	}
	return &worker.Worker{Name: name, Coord: c, Storages: map[string]storage.Storage{"default": e.store}, Engines: worker.DefaultEngines(),
		SpoolDir: filepath.Join(e.dir, "spool-"+name), Idle: 20 * time.Millisecond,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func (e *env) drain(w *worker.Worker) {
	e.t.Helper()
	for range 1000 {
		ok, err := w.RunOnce(context.Background())
		if err != nil {
			e.t.Fatal(err)
		}
		if !ok {
			return
		}
	}
	e.t.Fatal("queue never drained")
}

const basicSources = `
defaults:
  block: [doubleclick.net]
  skip_frames: [youtube.com]
  min_text_chars: 200
  fallback: http
sources:
  - name: test
    feeds: [SITE/rss.xml]
  - name: shells
    urls: [SITE/news/js.html]
`

func TestPollCaptureFallbackAttachments(t *testing.T) {
	e := newEnv(t, basicSources, time.Minute)
	ctx := context.Background()
	if err := e.coord.PollSources(ctx, true); err != nil {
		t.Fatal(err)
	}
	counts, _ := e.q.Counts(ctx)
	if counts[queue.Queued] != 3 {
		t.Fatalf("queued %v", counts)
	}
	st := e.coord.SourceStatuses(ctx)["test"]
	if st.Found != 2 || st.New != 2 {
		t.Fatalf("status %+v", st)
	}
	if err := e.coord.PollSources(ctx, true); err != nil {
		t.Fatal(err)
	}
	if counts, _ := e.q.Counts(ctx); counts[queue.Queued] != 3 {
		t.Fatal("seen URLs queued again")
	}
	// A rebuilt index forgets URLs that were only queued; the queue does not.
	raw, err := pgdb.Open(ctx, e.idxURL, e.idxSchema)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DELETE FROM urls`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	if err := e.coord.PollSources(ctx, true); err != nil {
		t.Fatal(err)
	}
	if counts, _ := e.q.Counts(ctx); counts[queue.Queued] != 3 {
		t.Fatalf("queued URLs queued again after the index lost them: %v", counts)
	}
	if st := e.coord.SourceStatuses(ctx)["test"]; st.New != 0 {
		t.Fatalf("status %+v", st)
	}

	e.drain(e.worker("w1", false))

	counts, _ = e.q.Counts(ctx)
	if counts[queue.Done] != 5 || counts[queue.Failed] != 0 {
		jobs, _ := e.q.List(ctx, "", 20, 0)
		for _, j := range jobs {
			t.Log(j.URL, j.Engine, j.State, j.LastError)
		}
		t.Fatalf("counts %v", counts)
	}
	jobs, _ := e.q.List(ctx, queue.Done, 20, 0)
	var fallback, pdf bool
	for _, j := range jobs {
		if strings.HasSuffix(j.URL, "js.html") && j.Fallback && j.Engine == engine.HTTP {
			fallback = true
		}
		if strings.HasSuffix(j.URL, "uchwala.pdf") && j.Engine == engine.HTTP && j.Priority == coordinator.PriorityAttachment {
			pdf = true
		}
	}
	if !fallback || !pdf {
		t.Fatalf("fallback %v pdf %v", fallback, pdf)
	}
	pages, total, _ := e.idx.Search(ctx, index.SearchQuery{Source: "test"})
	if total != 3 {
		t.Fatalf("pages %d", total)
	}
	_ = pages
	s, _ := e.idx.Stats(ctx, 30)
	if s.Revisits == 0 {
		t.Fatal("expected revisits between articles of one epoch")
	}
	var shells index.SourceStats
	for _, ss := range s.Sources {
		if ss.Source == "shells" {
			shells = ss
		}
	}
	if shells.Pages != 2 || shells.Fallbacks != 1 {
		t.Fatalf("fallback stats %+v", shells)
	}
}

func TestManyRemoteWorkers(t *testing.T) {
	e := newEnv(t, basicSources, time.Minute)
	ctx := context.Background()
	for i := range 30 {
		e.site.AddArticle(fmt.Sprintf("/news/extra-%02d.html", i), fmt.Sprintf("Artykuł %d", i),
			strings.Repeat(fmt.Sprintf("Treść artykułu numer %d o sprawach miasta. ", i), 10))
	}
	if err := e.coord.PollSources(ctx, true); err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for i := range 3 {
		w := e.worker(fmt.Sprintf("remote-%d", i), true)
		w.Slots = 3
		wg.Add(1)
		go func() { defer wg.Done(); w.Run(wctx) }()
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		c, _ := e.q.Counts(ctx)
		if c[queue.Queued] == 0 && c[queue.Leased] == 0 && c[queue.Done] > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	wg.Wait()
	counts, _ := e.q.Counts(ctx)
	if counts[queue.Queued]+counts[queue.Leased]+counts[queue.Failed] != 0 || counts[queue.Done] < 33 {
		t.Fatalf("counts %v", counts)
	}
	if len(e.coord.Workers()) != 3 {
		t.Fatalf("workers %+v", e.coord.Workers())
	}
	assertRevisitsResolve(t, e.idx, true)
	assertIncomingSelfContained(t, e.store)
}

// assertIncomingSelfContained checks, using storage alone, that every
// incoming WARC file has a sorted CDXJ next to it whose lines point at the
// right records, and that every revisit's payload is found in the indexes of
// the same directory.
func assertIncomingSelfContained(t *testing.T, st storage.Storage) {
	t.Helper()
	ctx := context.Background()
	objs, err := st.List(ctx, "incoming/")
	if err != nil {
		t.Fatal(err)
	}
	stored := map[string]map[string]bool{} // dir -> digests of full records
	var revisits []cdxj.Line
	var revisitDirs []string
	warcs := 0
	for _, o := range objs {
		if !strings.HasSuffix(o.Key, ".warc.gz") {
			continue
		}
		warcs++
		dir := path.Dir(o.Key)
		raw, err := storage.ReadAll(ctx, st, archive.IndexKey(o.Key))
		if err != nil {
			t.Fatalf("index of %s: %v", o.Key, err)
		}
		if ok, _ := cdxj.IsSorted(bytes.NewReader(raw)); !ok {
			t.Fatalf("index of %s not sorted", o.Key)
		}
		lines, err := cdxj.ReadAll(bytes.NewReader(raw))
		if err != nil || len(lines) == 0 {
			t.Fatalf("index of %s: %d lines, %v", o.Key, len(lines), err)
		}
		warc, err := storage.ReadAll(ctx, st, o.Key)
		if err != nil {
			t.Fatal(err)
		}
		if stored[dir] == nil {
			stored[dir] = map[string]bool{}
		}
		for _, l := range lines {
			if l.Filename != path.Base(o.Key) {
				t.Fatalf("%s: filename %q", o.Key, l.Filename)
			}
			off, _ := strconv.ParseInt(l.Offset, 10, 64)
			n, _ := strconv.ParseInt(l.Length, 10, 64)
			rec, err := warcio.ReadRecordAt(bytes.NewReader(warc[off:off+n]), 0, n)
			if err != nil || rec.ID() != l.RecordID || rec.Header.Get(warcio.HTargetURI) != l.URL {
				t.Fatalf("%s: line for %s does not match record at %d: %v", o.Key, l.URL, off, err)
			}
			if l.Type == warcio.TypeRevisit {
				revisits = append(revisits, l)
				revisitDirs = append(revisitDirs, dir)
			} else {
				stored[dir][l.Digest] = true
			}
		}
	}
	if warcs == 0 {
		t.Fatal("no incoming files")
	}
	for i, r := range revisits {
		if r.RefersTo != "" && !stored[revisitDirs[i]][r.Digest] {
			t.Fatalf("revisit %s: payload %s not in %s", r.URL, r.Digest, revisitDirs[i])
		}
	}
}

// assertRevisitsResolve checks that no revisit points to a missing record
// or crosses epochs.
func assertRevisitsResolve(t *testing.T, idx *index.DB, requireSome bool) {
	t.Helper()
	ctx := context.Background()
	eps, _ := idx.Epochs(ctx)
	revisits := 0
	for _, ep := range eps {
		recs, _ := idx.EpochRecords(ctx, "", ep.Epoch)
		for _, r := range recs {
			if r.Type != warcio.TypeRevisit || r.RefersTo == "" {
				continue
			}
			revisits++
			orig, err := idx.Record(ctx, r.RefersTo)
			if err != nil {
				t.Fatalf("revisit %s points to missing %s", r.URI, r.RefersTo)
			}
			if orig.Epoch != r.Epoch || orig.Digest != r.Digest {
				t.Fatalf("revisit %s inconsistent with original", r.URI)
			}
		}
	}
	if requireSome && revisits == 0 {
		t.Fatal("expected revisits")
	}
}

func TestKilledWorkerLeaseExpires(t *testing.T) {
	e := newEnv(t, basicSources, 300*time.Millisecond)
	ctx := context.Background()
	id, err := e.coord.AddURL(ctx, coordinator.URLRequest{URL: e.site.URL + "/news/article.html"})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := e.coord.Lease(ctx, worker.LeaseRequest{Worker: "dead", Engines: []string{"static"}, Max: 1})
	if err != nil || len(jobs) != 1 || jobs[0].ID != id || jobs[0].Options.MinTextChars != 200 {
		t.Fatalf("lease %+v %v", jobs, err)
	}
	survivor := e.worker("survivor", false)
	if ok, _ := survivor.RunOnce(ctx); ok {
		t.Fatal("leased job must not be handed out before expiry")
	}
	time.Sleep(400 * time.Millisecond)
	if ok, _ := survivor.RunOnce(ctx); !ok {
		t.Fatal("expired lease not handed out again")
	}
	j, _ := e.q.Get(ctx, id)
	if j.State != queue.Done || j.Worker != "survivor" || j.Attempts != 2 {
		t.Fatalf("job %+v", j)
	}

	late := capture(t, e, e.site.URL+"/news/article.html", jobs[0].Epoch)
	if err := e.coord.Complete(ctx, worker.Completion{JobID: id, Worker: "dead", Key: late.Key(), Capture: &late}); err != nil {
		t.Fatalf("late completion must be accepted as an extra capture: %v", err)
	}
	caps, _ := e.idx.PageCaptures(ctx, e.site.URL+"/news/article.html")
	if len(caps) != 2 {
		t.Fatalf("captures %d", len(caps))
	}
	j, _ = e.q.Get(ctx, id)
	if j.Worker != "survivor" {
		t.Fatal("first completion must win")
	}
}

// capture records a page outside the queue, like a slow worker would.
func capture(t *testing.T, e *env, url string, ep epoch.ID) recorder.CaptureFile {
	t.Helper()
	rec, err := recorder.New(recorder.Options{Epoch: ep, Dedup: e.idx.Dedup(""), SpoolDir: t.TempDir(), Source: "api", Engine: "static"})
	if err != nil {
		t.Fatal(err)
	}
	engine.HTTPEngine{}.Capture(context.Background(), engine.Job{URL: url}, rec)
	cf, _ := rec.Close()
	f, _ := os.Open(cf.Path)
	defer f.Close()
	if err := e.store.Put(context.Background(), cf.Key(), f, cf.Size); err != nil {
		t.Fatal(err)
	}
	return cf
}

func TestSealWaitsForLeasesAndRejectsLateCompletion(t *testing.T) {
	e := newEnv(t, basicSources, time.Minute)
	ctx := context.Background()
	e.coord.AddURL(ctx, coordinator.URLRequest{URL: e.site.URL + "/news/article.html"})
	e.drain(e.worker("w", false))
	id, _ := e.coord.AddURL(ctx, coordinator.URLRequest{URL: e.site.URL + "/news/article2.html"})
	jobs, _ := e.coord.Lease(ctx, worker.LeaseRequest{Worker: "slow", Engines: []string{"static"}, Max: 1})
	ep := jobs[0].Epoch

	if _, err := e.coord.Seal(ctx, "", ep, false); !errors.Is(err, coordinator.ErrActiveLeases) {
		t.Fatalf("seal with active lease: %v", err)
	}
	late := capture(t, e, e.site.URL+"/news/article2.html", ep)
	if _, err := e.coord.Seal(ctx, "", ep, true); err != nil {
		t.Fatal(err)
	}
	err := e.coord.Complete(ctx, worker.Completion{JobID: id, Worker: "slow", Key: late.Key(), Capture: &late})
	if !errors.Is(err, worker.ErrEpochClosed) {
		t.Fatalf("late completion after seal: %v", err)
	}
	j, _ := e.q.Get(ctx, id)
	if j.State != queue.Queued {
		t.Fatalf("job must return to the queue: %+v", j)
	}

	resp := httpPost(t, e.api.URL+worker.PathComplete, token, fmt.Sprintf(`{"jobId":%d,"worker":"slow","key":%q,"capture":{"name":%q,"epoch":%q,"size":%d}}`,
		id, late.Key(), late.Name, ep, late.Size))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("HTTP status %d", resp.StatusCode)
	}
	assertRevisitsResolve(t, e.idx, false)
}

func TestInternalAPIAuth(t *testing.T) {
	e := newEnv(t, basicSources, time.Minute)
	for _, tok := range []string{"", "wrong"} {
		resp := httpPost(t, e.api.URL+worker.PathLease, tok, `{"worker":"x","engines":["static"]}`)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q: %d", tok, resp.StatusCode)
		}
	}
	resp := httpPost(t, e.api.URL+worker.PathDedup, token, `{"epoch":"bad","digest":"x"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatal(resp.StatusCode)
	}
	closed := httptest.NewServer(e.coord.InternalHandler(""))
	defer closed.Close()
	if resp := httpPost(t, closed.URL+worker.PathLease, "", `{}`); resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("empty token must disable the internal API")
	}
}

func httpPost(t *testing.T, url, tok, body string) *http.Response {
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func TestRecordUploadAndSnapshots(t *testing.T) {
	e := newEnv(t, basicSources, time.Minute)
	ctx := context.Background()
	cf, err := e.coord.Record(ctx, "", "", "upload", "upload", func(r *recorder.Recorder) error {
		info, err := r.WriteResource(ctx, recorder.ResourceRecord{URI: "https://x.pl/a", ContentType: "text/html", Body: []byte("<title>Up</title><p>tekst</p>")})
		if err != nil {
			return err
		}
		_, err = r.WriteMetadata(recorder.PageInfo{URL: "https://x.pl/a", RecordID: info.ID, Title: "Up", Text: "tekst"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Stat(ctx, cf.Key()); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.idx.CountMatches(ctx, "tekst"); n != 1 {
		t.Fatal("upload not indexed")
	}
	if err := e.coord.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.coord.SnapshotSources(ctx); err != nil {
		t.Fatal(err)
	}
	snaps, _ := e.store.List(ctx, "config/sources/")
	if len(snaps) != 1 {
		t.Fatalf("snapshots %d", len(snaps))
	}
	if _, err := e.store.Stat(ctx, archive.ReadmeKey); err != nil {
		t.Fatal("README missing")
	}
	src, _ := e.coord.Sources().Find("test")
	found, fresh, err := e.coord.TestSource(ctx, src)
	if err != nil || found != 2 || fresh != 2 {
		t.Fatalf("test source %d %d %v", found, fresh, err)
	}
	if _, err := e.coord.AddURL(ctx, coordinator.URLRequest{URL: "notaurl"}); err == nil {
		t.Fatal("invalid URL accepted")
	}
}

const blocklistSources = `
defaults:
  min_text_chars: 0
blocklists:
  urls: [SITE/blocklist-hosts.txt, SITE/blocklist-adblock.txt]
sources:
  - name: test
    urls: [SITE/news/article.html]
`

// TestBlocklistsAppliedByWorkers checks that domains from public block lists
// are never contacted or archived, by local and remote workers, and that a
// changed configuration reaches remote workers through a new version.
func TestBlocklistsAppliedByWorkers(t *testing.T) {
	e := newEnv(t, blocklistSources, time.Minute)
	ctx := context.Background()
	if err := e.coord.UpdateBlocklists(ctx); err != nil {
		t.Fatal(err)
	}
	set, _ := e.coord.Blocklist(ctx)
	if !set.Blocked("ads.doubleclick.net") || !set.Blocked("www.youtube.com") {
		t.Fatal("lists not loaded")
	}
	st := e.coord.BlocklistStatuses()
	if len(st) != 2 || st[0].Entries != 2 || st[1].Entries != 2 {
		t.Fatalf("statuses %+v", st)
	}

	check := func(w *worker.Worker, wantBlocked int) {
		t.Helper()
		e.coord.AddURL(ctx, coordinator.URLRequest{URL: e.site.URL + "/news/article.html", Source: "test"})
		if ok, err := w.RunOnce(ctx); !ok || err != nil {
			t.Fatalf("no job: %v", err)
		}
		pages, _ := e.idx.PageCaptures(ctx, e.site.URL+"/news/article.html")
		latest := pages[0]
		meta, err := e.coord.Library().PageMetadata(ctx, latest)
		if err != nil {
			t.Fatal(err)
		}
		if meta.Blocked != wantBlocked || meta.Failed != 0 {
			t.Fatalf("%s: blocked %d failed %d, want %d blocked", w.Name, meta.Blocked, meta.Failed, wantBlocked)
		}
		data, err := storage.ReadAll(ctx, e.store, latest.Location)
		if err != nil {
			t.Fatal(err)
		}
		recs, _ := warcio.ReadAll(bytes.NewReader(data))
		if !bytes.Contains(recs[0].Block, []byte("blocklist: ")) {
			t.Fatalf("warcinfo lacks the block list version: %s", recs[0].Block)
		}
		for _, r := range recs {
			if strings.Contains(r.TargetURI(), "doubleclick") || strings.Contains(r.TargetURI(), "youtube") {
				t.Fatalf("blocked domain archived: %s", r.TargetURI())
			}
		}
	}
	check(e.worker("local", false), 2)
	remote := e.worker("remote", true)
	check(remote, 2)

	next := *e.coord.Sources()
	bl := *next.Blocklists
	bl.Allow = []string{"youtube.com"}
	next.Blocklists = &bl
	next.Defaults.SkipFrames = []string{"youtube.com"}
	if err := e.coord.UpdateSources(ctx, &next); err != nil {
		t.Fatal(err)
	}
	newSet, _ := e.coord.Blocklist(ctx)
	if newSet.Version() == set.Version() || newSet.Blocked("www.youtube.com") {
		t.Fatal("allow list not applied")
	}
	check(remote, 1)
}

func TestSourceManagementAndPollState(t *testing.T) {
	e := newEnv(t, "sources: []\n", time.Minute)
	ctx := context.Background()
	if len(e.coord.Sources().Sources) != 0 {
		t.Fatal("a fresh instance must start without sources")
	}
	if err := e.coord.SaveSource(ctx, config.Source{Name: "Zła nazwa", URLs: []string{e.site.URL + "/"}}); err == nil {
		t.Fatal("invalid source saved")
	}
	feed := config.Source{Name: "miasto", Feeds: []string{e.site.URL + "/rss.xml"}, Settings: config.Settings{Poll: config.Duration(time.Hour)}}
	if err := e.coord.SaveSource(ctx, feed); err != nil {
		t.Fatal(err)
	}
	if err := e.coord.SaveSource(ctx, config.Source{Name: "jeden", URLs: []string{e.site.URL + "/news/article2.html"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.coord.PollSources(ctx, false); err != nil {
		t.Fatal(err)
	}
	first := e.coord.SourceStatuses(ctx)["miasto"]
	if first.Found != 2 || first.New != 1 { // "jeden" already queued article2
		t.Fatalf("first poll %+v", first)
	}

	// A restarted coordinator reads the configuration and the poll state
	// from the database and does not poll again before the interval.
	restarted, err := coordinator.New(coordinator.Options{Library: e.coord.Library(), Queue: e.q, Sources: sourcestore.New(e.db)})
	if err != nil {
		t.Fatal(err)
	}
	restarted.Now = func() time.Time { return time.Now().Add(30 * time.Minute) }
	if err := restarted.PollSources(ctx, false); err != nil {
		t.Fatal(err)
	}
	if st := restarted.SourceStatuses(ctx)["miasto"]; !st.LastPoll.Equal(first.LastPoll) {
		t.Fatalf("polled again before the interval: %+v", st)
	}
	restarted.Now = func() time.Time { return time.Now().Add(61 * time.Minute) }
	if err := restarted.PollSources(ctx, false); err != nil {
		t.Fatal(err)
	}
	if st := restarted.SourceStatuses(ctx)["miasto"]; !st.LastPoll.After(first.LastPoll) || st.New != 0 {
		t.Fatalf("not polled after the interval: %+v", st)
	}

	if err := restarted.SetSourceEnabled(ctx, "miasto", false); err != nil {
		t.Fatal(err)
	}
	if err := restarted.SetSourceEnabled(ctx, "brak", false); !errors.Is(err, coordinator.ErrUnknownSource) {
		t.Fatalf("unknown source: %v", err)
	}
	if err := restarted.DeleteSource(ctx, "jeden"); err != nil {
		t.Fatal(err)
	}
	cfg, _ := sourcestore.New(e.db).Load(ctx)
	if len(cfg.Sources) != 1 || cfg.Sources[0].IsEnabled() {
		t.Fatalf("stored %+v", cfg.Sources)
	}
	if err := restarted.PollSource(ctx, "jeden"); !errors.Is(err, coordinator.ErrUnknownSource) {
		t.Fatalf("poll of a deleted source: %v", err)
	}
}

func TestAlertingMetrics(t *testing.T) {
	e := newEnv(t, "sources: []\n", time.Minute)
	ctx := context.Background()
	for _, s := range []config.Source{
		{Name: "ok", Feeds: []string{e.site.URL + "/rss.xml"}},
		{Name: "broken", Sitemaps: []string{e.site.URL + "/missing-sitemap.xml"}},
		{Name: "off", Feeds: []string{e.site.URL + "/rss.xml"}, Enabled: new(bool)},
	} {
		if err := e.coord.SaveSource(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.coord.PollSources(ctx, false); err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("poll error %v", err)
	}
	if _, err := e.coord.AddURL(ctx, coordinator.URLRequest{URL: e.site.URL + "/gone.html", Source: "ok"}); err != nil {
		t.Fatal(err)
	}
	e.drain(e.worker("remote", true)) // the reason travels through the internal API

	rr := httptest.NewRecorder()
	e.coord.Metrics.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	body := rr.Body.String()
	for _, want := range []string{
		`hoarfrost_source_failing{source="broken"} 1`,
		`hoarfrost_source_failing{source="ok"} 0`,
		`hoarfrost_source_poll_errors_total{source="broken"} 1`,
		`hoarfrost_source_urls_found{source="ok"} 2`,
		`hoarfrost_source_urls_found{source="broken"} 0`,
		`hoarfrost_source_poll_interval_seconds{source="ok"} 3600`,
		`hoarfrost_source_last_success_timestamp_seconds{source="ok"}`,
		`hoarfrost_source_last_poll_timestamp_seconds{source="broken"}`,
		`hoarfrost_capture_failures_total{reason="http_404",source="ok"} 1`,
		`hoarfrost_epochs{state="open",storage="default"} 1`,
		`hoarfrost_oldest_unsealed_epoch_end_timestamp_seconds{storage="default"}`,
		`hoarfrost_seal_errors{storage="default"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %s", want)
		}
	}
	for _, unwanted := range []string{`hoarfrost_source_last_success_timestamp_seconds{source="broken"}`, `source="off"`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("metrics have %s", unwanted)
		}
	}
	if st := e.coord.SourceStatuses(ctx)["ok"]; st.LastSuccess.IsZero() || !st.LastSuccess.Equal(st.LastPoll) {
		t.Fatalf("status %+v", st)
	}
}
