package web

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ogefest/hoarfrost/internal/pgdb/pgtest"

	"github.com/ogefest/hoarfrost/internal/appdb"
	"github.com/ogefest/hoarfrost/internal/archive"
	"github.com/ogefest/hoarfrost/internal/auth"
	"github.com/ogefest/hoarfrost/internal/config"
	"github.com/ogefest/hoarfrost/internal/coordinator"
	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/index"
	"github.com/ogefest/hoarfrost/internal/queue"
	"github.com/ogefest/hoarfrost/internal/sourcestore"
	"github.com/ogefest/hoarfrost/internal/storage"
	"github.com/ogefest/hoarfrost/internal/testsite"
	"github.com/ogefest/hoarfrost/internal/worker"
)

func init() {
	auth.ArgonTime, auth.ArgonMemory, auth.ArgonThreads = 1, 1024, 1
}

type env struct {
	t      *testing.T
	site   *testsite.Site
	srv    *httptest.Server
	s      *Server
	coord  *coordinator.Coordinator
	auth   *auth.Store
	worker *worker.Worker
	db     *sql.DB
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func newEnv(t *testing.T, extraStorages ...string) *env {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	e := &env{t: t, site: testsite.New()}
	t.Cleanup(e.site.Close)
	storages := map[string]storage.Storage{}
	var descr []string
	for _, name := range append([]string{"default"}, extraStorages...) {
		st, _ := storage.NewFS(filepath.Join(dir, "storage-"+name))
		storages[name] = st
		descr = append(descr, name+": "+st.String())
	}
	indexURL, indexSchema := pgtest.Schema(t, "index")
	idx, err := index.Open(ctx, indexURL, indexSchema)
	if err != nil {
		t.Fatal(err)
	}
	appURL, appSchema := pgtest.Schema(t, "app")
	db, err := appdb.Open(ctx, appURL, appSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(); idx.Close() })
	e.db = db
	sources := sourcestore.New(db)
	if err := sources.Save(ctx, &config.Sources{
		Defaults:   config.Defaults{Settings: config.Settings{Block: []string{"doubleclick.net"}, SkipFrames: []string{"youtube.com"}}},
		Blocklists: &config.Blocklists{URLs: []string{}},
		Sources:    []config.Source{{Name: "test", Feeds: []string{e.site.URL + "/rss.xml"}}},
	}); err != nil {
		t.Fatal(err)
	}
	lib := archive.NewLibrary(idx, storages, filepath.Join(dir, "spool"), quiet)
	e.coord, err = coordinator.New(coordinator.Options{Library: lib, Queue: queue.New(db), Sources: sources, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	e.auth = auth.New(db, "test-secret-0123456789-0123456789")
	e.worker = &worker.Worker{Name: "embedded", Coord: e.coord, Storages: storages, Engines: worker.DefaultEngines(), SpoolDir: filepath.Join(dir, "wspool"), Log: quiet}
	e.s, err = New(&Server{Coord: e.coord, Auth: e.auth, Log: quiet, WorkerToken: "wtok", Active: e.worker.Active, Storage: descr})
	if err != nil {
		t.Fatal(err)
	}
	e.srv = httptest.NewServer(e.s.Handler())
	t.Cleanup(e.srv.Close)
	return e
}

func (e *env) drain() {
	for range 100 {
		if ok, _ := e.worker.RunOnce(context.Background()); !ok {
			return
		}
	}
}

// browser is a cookie-keeping client that does not follow redirects.
type browser struct {
	*http.Client
	base string
	csrf string
}

func (e *env) browser() *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{Client: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, base: e.srv.URL}
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func (b *browser) get(t *testing.T, path string) (*http.Response, string) {
	t.Helper()
	resp, err := b.Get(b.base + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if m := csrfRe.FindSubmatch(body); m != nil {
		b.csrf = string(m[1])
	}
	return resp, string(body)
}

func (b *browser) post(t *testing.T, path string, form url.Values) (*http.Response, string) {
	t.Helper()
	resp, err := b.PostForm(b.base+path, form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func (e *env) admin(t *testing.T) *browser {
	b := e.browser()
	resp, _ := b.get(t, "/")
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login") {
		t.Fatalf("anonymous dashboard: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, _ = b.get(t, "/login")
	if resp.Header.Get("Location") != "/setup" {
		t.Fatal("first run must redirect to setup")
	}
	resp, _ = b.post(t, "/setup", url.Values{"login": {"admin"}, "password": {"admin-password-1"}, "password2": {"admin-password-1"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("setup: %d", resp.StatusCode)
	}
	b.get(t, "/account")
	return b
}

func TestSetupLoginCSRFAndPages(t *testing.T) {
	e := newEnv(t)
	b := e.admin(t)

	if resp, _ := e.browser().post(t, "/setup", url.Values{"login": {"x"}, "password": {"yyyyyyyyyyyy"}, "password2": {"yyyyyyyyyyyy"}}); resp.StatusCode != http.StatusForbidden {
		t.Fatal("setup must be disabled after the first user")
	}
	resp, _ := b.post(t, "/queue/add", url.Values{"urls": {e.site.URL + "/news/article.html"}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST without CSRF token: %d", resp.StatusCode)
	}
	resp, _ = b.post(t, "/queue/add", url.Values{"urls": {e.site.URL + "/news/article.html"}, "csrf": {b.csrf}})
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "queued") {
		t.Fatalf("queue add: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	e.drain()

	for _, p := range []string{"/", "/archive", "/archive?q=budzet", "/sources", "/sources/new", "/sources/test/edit", "/sources/settings", "/queue", "/queue?state=done", "/queue?state=failed", "/system", "/account"} {
		resp, body := b.get(t, p)
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, "</html>") {
			t.Fatalf("%s: %d\n%s", p, resp.StatusCode, body)
		}
		if resp.Header.Get("Content-Security-Policy") == "" {
			t.Fatalf("%s: missing CSP", p)
		}
	}
	_, body := b.get(t, "/archive?q=transport")
	m := regexp.MustCompile(`href="/page/(\d+)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("search result missing:\n%s", body)
	}
	resp, body = b.get(t, "/page/"+m[1])
	if resp.StatusCode != 200 || !strings.Contains(body, "transport publiczny") {
		t.Fatalf("page view: %d\n%s", resp.StatusCode, body)
	}
	resp, body = b.get(t, "/page/"+m[1]+"/preview")
	if !strings.HasPrefix(resp.Header.Get("Content-Security-Policy"), "sandbox") || !strings.Contains(body, "data:image/png;base64") || strings.Contains(body, "<script") {
		t.Fatalf("preview: %s", resp.Header.Get("Content-Security-Policy"))
	}
	resp, _ = b.get(t, "/api/export/"+m[1]+".html")
	if !strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatal("export download")
	}
	if resp, _ := b.get(t, "/replay/"+m[1]); resp.StatusCode != http.StatusConflict {
		t.Fatalf("replay before seal: %d", resp.StatusCode)
	}

	resp, _ = b.post(t, "/logout", url.Values{"csrf": {b.csrf}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatal("logout")
	}
	if resp, _ := b.get(t, "/"); resp.StatusCode != http.StatusSeeOther {
		t.Fatal("session survived logout")
	}
	resp, _ = b.post(t, "/login", url.Values{"login": {"admin"}, "password": {"wrong-password"}})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad login: %d", resp.StatusCode)
	}
	resp, _ = b.post(t, "/login", url.Values{"login": {"admin"}, "password": {"admin-password-1"}, "next": {"//evil.example/"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("login redirect %q", resp.Header.Get("Location"))
	}
	for range 10 {
		b.post(t, "/login", url.Values{"login": {"admin"}, "password": {"wrong"}})
	}
	if resp, _ := b.post(t, "/login", url.Values{"login": {"admin"}, "password": {"admin-password-1"}}); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("rate limit: %d", resp.StatusCode)
	}
}

func api(t *testing.T, method, url, token, ct string, body io.Reader) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, url, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func TestAPIAndReplay(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	editor, _ := e.auth.CreateUser(ctx, "editor", "editor-password", auth.RoleEditor)
	viewer, _ := e.auth.CreateUser(ctx, "viewer", "viewer-password", auth.RoleViewer)
	etok, _ := e.auth.CreateToken(ctx, editor, "ci")
	vtok, _ := e.auth.CreateToken(ctx, viewer, "ro")

	resp, body := api(t, "POST", e.srv.URL+"/api/urls", etok, "application/json", strings.NewReader(`{"url":"`+e.site.URL+`/news/article2.html"}`))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("add url: %d %s", resp.StatusCode, body)
	}
	if resp, _ := api(t, "POST", e.srv.URL+"/api/urls", vtok, "application/json", strings.NewReader(`{"url":"https://x.pl/"}`)); resp.StatusCode != http.StatusForbidden {
		t.Fatal("viewer must not add URLs")
	}
	if resp, _ := api(t, "GET", e.srv.URL+"/api/search?q=x", "hf_invalid", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("invalid token")
	}
	if resp, _ := api(t, "GET", e.srv.URL+"/api/search?q=x", "", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatal("anonymous API access")
	}

	var mp bytes.Buffer
	mw := multipart.NewWriter(&mp)
	fw, _ := mw.CreateFormFile("file", "saved.html")
	fw.Write([]byte(`<!DOCTYPE html><html><!-- Page saved with SingleFile url: https://serwis.pl/z-przegladarki saved date: x --><head><title>Z przeglądarki</title></head><body><p>Treść zapisana rozszerzeniem przeglądarki.</p></body></html>`))
	mw.WriteField("queue", "0")
	mw.Close()
	resp, body = api(t, "POST", e.srv.URL+"/api/upload", etok, mw.FormDataContentType(), &mp)
	if resp.StatusCode != http.StatusCreated || !strings.Contains(string(body), "z-przegladarki") {
		t.Fatalf("upload: %d %s", resp.StatusCode, body)
	}
	resp, body = api(t, "POST", e.srv.URL+"/api/upload?url=https://a.pl/raw", etok, "text/html", strings.NewReader("<html><title>Raw</title><p>surowe</p></html>"))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("raw upload: %d %s", resp.StatusCode, body)
	}
	if resp, _ := api(t, "POST", e.srv.URL+"/api/upload", etok, "application/octet-stream", strings.NewReader("\x00\x01")); resp.StatusCode != http.StatusBadRequest {
		t.Fatal("unknown upload format")
	}

	e.drain()
	resp, body = api(t, "GET", e.srv.URL+"/api/search?q=tramwajowej", vtok, "", nil)
	var sr struct {
		Total   int       `json:"total"`
		Results []apiPage `json:"results"`
	}
	json.Unmarshal(body, &sr)
	if resp.StatusCode != 200 || sr.Total != 1 {
		t.Fatalf("search: %s", body)
	}
	pg := sr.Results[0]
	_, body = api(t, "GET", fmt.Sprintf("%s/api/pages/%d", e.srv.URL, pg.ID), vtok, "", nil)
	if !strings.Contains(string(body), "przetarg") {
		t.Fatalf("page text: %s", body)
	}
	resp, body = api(t, "GET", e.srv.URL+"/api/record/"+url.PathEscape(strings.Trim(pg.RecordID, "<>")), vtok, "", nil)
	if resp.StatusCode != 200 || body[0] != 0x1f || resp.Header.Get("Content-Type") != "application/warc" {
		t.Fatalf("record: %d", resp.StatusCode)
	}
	_, body = api(t, "GET", e.srv.URL+"/api/search?q=przegladarki", vtok, "", nil)
	if !strings.Contains(string(body), "Z przeglądarki") {
		t.Fatalf("uploaded page not searchable: %s", body)
	}
	resp, body = api(t, "GET", e.srv.URL+"/metrics", vtok, "", nil)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "hoarfrost_captures_total") || !strings.Contains(string(body), "hoarfrost_queue_jobs") {
		t.Fatalf("metrics: %d", resp.StatusCode)
	}
	if resp, _ := api(t, "GET", e.srv.URL+"/sources", vtok, "", nil); resp.StatusCode != http.StatusForbidden {
		t.Fatal("viewer must not see sources")
	}

	ep := epoch.Of(time.Now())
	if _, err := e.coord.Seal(ctx, "", ep, true); err != nil {
		t.Fatal(err)
	}
	waczURL := e.srv.URL + "/wacz/default/" + strings.TrimPrefix(ep.VolumeKey(1), "weekly/")
	req, _ := http.NewRequest("GET", waczURL, nil)
	req.Header.Set("Authorization", "Bearer "+vtok)
	req.Header.Set("Range", "bytes=0-3")
	rr, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	head, _ := io.ReadAll(rr.Body)
	rr.Body.Close()
	if rr.StatusCode != http.StatusPartialContent || string(head) != "PK\x03\x04" {
		t.Fatalf("wacz range: %d %q", rr.StatusCode, head)
	}
	resp, body = api(t, "GET", fmt.Sprintf("%s/replay/%d", e.srv.URL, pg.ID), vtok, "", nil)
	coll := fmt.Sprintf("/wacz/default/%04d/W%02d/collection.json", ep.Year(), ep.Week())
	if resp.StatusCode != 200 || !strings.Contains(string(body), "<replay-web-page") || !strings.Contains(string(body), "replayBase=\"/replay/\"") ||
		!strings.Contains(string(body), `source="`+coll+`"`) {
		t.Fatalf("replay: %d %s", resp.StatusCode, body)
	}
	resp, body = api(t, "GET", e.srv.URL+coll, vtok, "", nil)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"path": "`+path.Base(ep.VolumeKey(1))+`"`) {
		t.Fatalf("collection: %d %s", resp.StatusCode, body)
	}
	if resp, _ := api(t, "GET", e.srv.URL+"/wacz/default/../catalog/x.json", vtok, "", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("path outside weekly/: %d", resp.StatusCode)
	}
	_, body = api(t, "GET", e.srv.URL+"/replay/sw.js", vtok, "", nil)
	if !strings.Contains(string(body), "importScripts") {
		t.Fatal("service worker")
	}
	resp, _ = api(t, "GET", fmt.Sprintf("%s/page/%d/preview", e.srv.URL, pg.ID), vtok, "", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("preview after seal: %d", resp.StatusCode)
	}

	if resp, _ := api(t, "POST", e.srv.URL+"/internal/lease", "wtok", "application/json", strings.NewReader(`{"worker":"x","engines":["static"]}`)); resp.StatusCode != 200 {
		t.Fatalf("internal API: %d", resp.StatusCode)
	}
}

func TestSourcesEditor(t *testing.T) {
	e := newEnv(t, "prasa")
	b := e.admin(t)
	ctx := context.Background()
	form := func(kv ...string) url.Values {
		v := url.Values{"csrf": {b.csrf}}
		for i := 0; i+1 < len(kv); i += 2 {
			v.Set(kv[i], kv[i+1])
		}
		return v
	}
	resp, body := b.post(t, "/sources/new", form("name", "Bad Name", "feeds", e.site.URL+"/rss.xml", "enabled", "1"))
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "Bad Name") {
		t.Fatalf("invalid name: %d", resp.StatusCode)
	}
	resp, body = b.post(t, "/sources/new", form("name", "nowy", "feeds", e.site.URL+"/rss.xml", "poll", "co godzinę", "enabled", "1"))
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "poll interval") || !strings.Contains(body, e.site.URL+"/rss.xml") {
		t.Fatalf("invalid poll interval: %d", resp.StatusCode)
	}
	resp, _ = b.post(t, "/sources/new", form("name", "nowy", "feeds", e.site.URL+"/rss.xml", "sitemaps", e.site.URL+"/sitemap-index.xml",
		"poll", "30m", "storage", "prasa", "fallback", "none", "max_size", "5MB", "attachments", "no", "enabled", "1"))
	if resp.StatusCode != http.StatusSeeOther || strings.Contains(resp.Header.Get("Location"), "err=") {
		t.Fatalf("create: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	res := e.coord.Sources().Resolve("nowy")
	if src, ok := e.coord.Sources().Find("nowy"); !ok || len(src.Sitemaps) != 1 || res.Poll != 30*time.Minute || res.Storage != "prasa" ||
		res.Options.Fallback != "" || res.Options.MaxResponseSize != 5<<20 || res.Options.Attachments || !src.IsEnabled() {
		t.Fatalf("created source %+v %+v", src, res)
	}
	if resp, _ := b.post(t, "/sources/new", form("name", "nowy", "urls", e.site.URL+"/", "enabled", "1")); resp.StatusCode != http.StatusBadRequest {
		t.Fatal("duplicate name accepted")
	}
	_, body = b.get(t, "/sources/nowy/edit")
	if !strings.Contains(body, `value="30m"`) || !strings.Contains(body, `value="5MB"`) || !strings.Contains(body, "readonly") {
		t.Fatalf("edit form not prefilled:\n%s", body)
	}
	resp, _ = b.post(t, "/sources/nowy/edit", form("name", "inna", "feeds", e.site.URL+"/rss.xml", "poll", "2h", "storage", "prasa"))
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("update: %d", resp.StatusCode)
	}
	if src, ok := e.coord.Sources().Find("nowy"); !ok || src.IsEnabled() || len(src.Sitemaps) != 0 || e.coord.Sources().Resolve("nowy").Poll != 2*time.Hour {
		t.Fatalf("updated source %+v", src)
	}
	if _, renamed := e.coord.Sources().Find("inna"); renamed {
		t.Fatal("edit renamed the source")
	}

	htmx := func(path string, v url.Values) string {
		req, _ := http.NewRequest("POST", e.srv.URL+path, strings.NewReader(v.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-CSRF-Token", b.csrf)
		tr, err := b.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer tr.Body.Close()
		out, _ := io.ReadAll(tr.Body)
		return string(out)
	}
	if out := htmx("/sources/test", form("feeds", e.site.URL+"/rss.xml")); !strings.Contains(out, "2 URLs found, 2 new") {
		t.Fatalf("test unsaved source: %s", out)
	}
	if out := htmx("/sources/test", form("feeds", "not a url")); !strings.Contains(out, "invalid URL") {
		t.Fatalf("test invalid source: %s", out)
	}
	if out := htmx("/sources/nowy/test", nil); !strings.Contains(out, "2 URLs found, 2 new") {
		t.Fatalf("test source: %s", out)
	}
	b.post(t, "/sources/nowy/toggle", form())
	if src, _ := e.coord.Sources().Find("nowy"); !src.IsEnabled() {
		t.Fatal("toggle")
	}
	if resp, _ := b.post(t, "/sources/nowy/poll", form()); strings.Contains(resp.Header.Get("Location"), "err=") {
		t.Fatalf("poll: %s", resp.Header.Get("Location"))
	}
	if st := e.coord.SourceStatuses(ctx)["nowy"]; st.LastPoll.IsZero() || st.Found != 2 {
		t.Fatalf("poll state %+v", st)
	}
	_, body = b.get(t, "/sources")
	if !strings.Contains(body, "2 found, 2 new") || !strings.Contains(body, "in 1 h") {
		t.Fatalf("sources list lacks poll state:\n%s", body)
	}

	resp, body = b.post(t, "/sources/settings/defaults", form("poll", "x"))
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "poll interval") {
		t.Fatalf("invalid defaults: %d", resp.StatusCode)
	}
	resp, _ = b.post(t, "/sources/settings/defaults", form("poll", "3h", "engine", "http", "max_per_domain", "4", "block", "ads.example\ntracker.example"))
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("defaults: %d", resp.StatusCode)
	}
	cfg := e.coord.Sources()
	if r := cfg.Resolve("test"); r.Poll != 3*time.Hour || r.Engine != "http" || cfg.MaxPerDomain() != 4 || len(r.Options.Block) != 2 {
		t.Fatalf("defaults not applied %+v", r)
	}
	if resp, _ := b.post(t, "/sources/settings/blocklists", form("urls", "https://lists.example/hosts", "refresh", "1m")); resp.StatusCode != http.StatusBadRequest {
		t.Fatal("too short refresh accepted")
	}
	b.post(t, "/sources/settings/blocklists", form("urls", "https://lists.example/hosts\nhttps://lists.example/ads", "allow", "cdn.example", "refresh", "6h"))
	if urls, refresh, allow := e.coord.Sources().BlocklistSettings(); len(urls) != 2 || refresh != 6*time.Hour || len(allow) != 1 {
		t.Fatalf("block lists %v %v %v", urls, refresh, allow)
	}
	b.post(t, "/sources/settings/blocklists", form("urls", "", "reset", "1"))
	if e.coord.Sources().Blocklists != nil {
		t.Fatal("default block lists not restored")
	}

	// The configuration survives a restart: it lives in the database.
	again, err := sourcestore.New(e.db).Load(ctx)
	if err != nil || len(again.Sources) != 2 || again.Resolve("nowy").Storage != "prasa" || again.MaxPerDomain() != 4 {
		t.Fatalf("stored configuration %+v %v", again, err)
	}
	snaps, _ := e.s.lib.All()[0].Storage.List(ctx, "config/sources/")
	if len(snaps) == 0 {
		t.Fatal("configuration not copied to the archive")
	}

	if resp, _ := b.post(t, "/sources/nowy/delete", form()); resp.StatusCode != http.StatusSeeOther {
		t.Fatal("delete")
	}
	if _, ok := e.coord.Sources().Find("nowy"); ok {
		t.Fatal("source not deleted")
	}
	resp, _ = b.post(t, "/system/users", url.Values{"csrf": {b.csrf}, "login": {"anna"}, "password": {"anna-password"}, "role": {"viewer"}})
	if resp.StatusCode != http.StatusSeeOther || strings.Contains(resp.Header.Get("Location"), "err=") {
		t.Fatalf("create user: %s", resp.Header.Get("Location"))
	}
	resp, _ = b.post(t, "/system/users/admin/delete", url.Values{"csrf": {b.csrf}})
	if !strings.Contains(resp.Header.Get("Location"), "err=") {
		t.Fatal("admin deleted themselves")
	}
	resp, _ = b.post(t, "/system/reindex", url.Values{"csrf": {b.csrf}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatal("reindex")
	}
	time.Sleep(100 * time.Millisecond)
	resp, _ = b.post(t, "/system/verify-all", url.Values{"csrf": {b.csrf}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatal("verify all")
	}
	_, body = b.get(t, "/system/tasks")
	if !strings.Contains(body, "task") {
		t.Fatalf("task status: %s", body)
	}
}

func TestHelpers(t *testing.T) {
	cases := map[string]string{"/x": "/x", "//evil": "/", "https://evil": "/", "": "/", "/\\evil": "/"}
	for in, want := range cases {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q", in, got)
		}
	}
	if recordID("abc") != "<urn:uuid:abc>" || recordID("<urn:uuid:x>") != "<urn:uuid:x>" || recordID("urn:uuid:y") != "<urn:uuid:y>" {
		t.Fatal("recordID")
	}
	if humanDuration(time.Hour) != "1h" || humanDuration(90*time.Minute) != "1h30m" || humanDuration(24*time.Hour) != "24h" {
		t.Fatal(humanDuration(time.Hour))
	}
	if humanBytes(1536) != "1.5 KiB" || humanBytes(10) != "10 B" {
		t.Fatal(humanBytes(1536))
	}
}

func TestAccountQueueAndEpochActions(t *testing.T) {
	e := newEnv(t)
	b := e.admin(t)
	ctx := context.Background()

	resp, body := b.post(t, "/account/tokens", url.Values{"csrf": {b.csrf}, "name": {"laptop"}})
	tok := regexp.MustCompile(`hf_[A-Za-z0-9_-]+`).FindString(body)
	if resp.StatusCode != http.StatusOK || tok == "" {
		t.Fatalf("token not shown: %d", resp.StatusCode)
	}
	if code, _ := api(t, "GET", e.srv.URL+"/api/search", tok, "", nil); code.StatusCode != http.StatusOK {
		t.Fatal("new token rejected")
	}
	toks, _ := e.auth.Tokens(ctx, 0)
	b.post(t, fmt.Sprintf("/account/tokens/%d/delete", toks[0].ID), url.Values{"csrf": {b.csrf}})
	if code, _ := api(t, "GET", e.srv.URL+"/api/search", tok, "", nil); code.StatusCode != http.StatusUnauthorized {
		t.Fatal("revoked token accepted")
	}
	resp, _ = b.post(t, "/account/password", url.Values{"csrf": {b.csrf}, "current": {"admin-password-1"}, "password": {"new-password-22"}, "password2": {"new-password-22"}})
	if strings.Contains(resp.Header.Get("Location"), "err=") {
		t.Fatalf("password change: %s", resp.Header.Get("Location"))
	}
	if _, err := e.auth.Authenticate(ctx, "admin", "new-password-22"); err != nil {
		t.Fatal("password not changed")
	}
	if resp, _ := b.get(t, "/account"); resp.StatusCode != http.StatusOK {
		t.Fatal("password change must keep the user signed in with a new session")
	}

	resp, _ = b.post(t, "/queue/add", url.Values{"csrf": {b.csrf}, "urls": {e.site.URL + "/error\n" + e.site.URL + "/news/article.html\nnot a url"}})
	if !strings.Contains(resp.Header.Get("Location"), "err=") {
		t.Fatal("invalid URL must be reported")
	}
	e.worker.RunOnce(ctx)
	e.worker.RunOnce(ctx)
	failed, _ := e.coord.Queue().List(ctx, queue.Failed, 10, 0)
	queued, _ := e.coord.Queue().List(ctx, queue.Queued, 10, 0)
	var id int64
	for _, j := range append(failed, queued...) {
		if strings.HasSuffix(j.URL, "/error") {
			id = j.ID
		}
	}
	if id == 0 {
		t.Fatal("error job missing")
	}
	e.coord.Queue().Finish(ctx, id, queue.Outcome{Error: "HTTP 500"})
	resp, _ = b.post(t, fmt.Sprintf("/queue/%d/retry", id), url.Values{"csrf": {b.csrf}})
	if strings.Contains(resp.Header.Get("Location"), "err=") {
		t.Fatalf("retry: %s", resp.Header.Get("Location"))
	}
	b.post(t, "/queue/retry-failed", url.Values{"csrf": {b.csrf}})
	if _, body := b.get(t, "/queue?state=queued"); !strings.Contains(body, "/error") {
		t.Fatal("retried job not listed")
	}

	ep := epoch.Of(time.Now())
	resp, _ = b.post(t, "/system/epochs/default/"+string(ep)+"/seal", url.Values{"csrf": {b.csrf}, "force": {"1"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatal("seal action")
	}
	waitTask(t, e)
	info, _ := e.coord.Library().Index.Epoch(ctx, "", ep)
	if info.State != index.StateVerified {
		t.Fatalf("epoch %+v", info)
	}
	b.post(t, "/system/epochs/default/"+string(ep)+"/verify", url.Values{"csrf": {b.csrf}})
	waitTask(t, e)
	if _, body := b.get(t, "/system"); !strings.Contains(body, "verified") {
		t.Fatal("system page does not show the verified week")
	}
	if resp, _ := b.post(t, "/system/epochs/default/bad/verify", url.Values{"csrf": {b.csrf}}); resp.StatusCode != http.StatusNotFound {
		t.Fatal("bad epoch")
	}
}

func waitTask(t *testing.T, e *env) {
	t.Helper()
	for range 200 {
		if running, _, _ := e.s.tasks.snapshot(); running == "" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("task did not finish")
}

func TestStoragesThroughAPI(t *testing.T) {
	e := newEnv(t, "prasa")
	ctx := context.Background()
	u, _ := e.auth.CreateUser(ctx, "ed", "editor-password", auth.RoleEditor)
	tok, _ := e.auth.CreateToken(ctx, u, "t")

	resp, body := api(t, "POST", e.srv.URL+"/api/urls", tok, "application/json", strings.NewReader(`{"url":"`+e.site.URL+`/news/article2.html","storage":"prasa"}`))
	if resp.StatusCode != http.StatusAccepted || !strings.Contains(string(body), `"storage":"prasa"`) {
		t.Fatalf("add url: %d %s", resp.StatusCode, body)
	}
	if resp, _ := api(t, "POST", e.srv.URL+"/api/urls", tok, "application/json", strings.NewReader(`{"url":"https://x.pl/","storage":"brak"}`)); resp.StatusCode != http.StatusBadRequest {
		t.Fatal("unknown storage accepted")
	}
	resp, body = api(t, "POST", e.srv.URL+"/api/upload?url=https://a.pl/z&storage=prasa", tok, "text/html", strings.NewReader("<html><title>Do prasy</title><p>tekst w prasie</p></html>"))
	if resp.StatusCode != http.StatusCreated || !strings.Contains(string(body), `"storage":"prasa"`) {
		t.Fatalf("upload: %d %s", resp.StatusCode, body)
	}
	if resp, _ := api(t, "POST", e.srv.URL+"/api/upload?url=https://a.pl/z&storage=brak", tok, "text/html", strings.NewReader("<html></html>")); resp.StatusCode != http.StatusBadRequest {
		t.Fatal("upload to unknown storage accepted")
	}
	e.drain()

	var sr struct {
		Total int `json:"total"`
	}
	_, body = api(t, "GET", e.srv.URL+"/api/search?storage=prasa", tok, "", nil)
	json.Unmarshal(body, &sr)
	if sr.Total != 3 || !strings.Contains(string(body), `"storage":"prasa"`) {
		t.Fatalf("pages in prasa (upload, article, its attachment): %s", body)
	}
	_, body = api(t, "GET", e.srv.URL+"/api/search?storage=default", tok, "", nil)
	json.Unmarshal(body, &sr)
	if sr.Total != 0 {
		t.Fatalf("pages in default: %s", body)
	}

	ep := epoch.Of(time.Now())
	if _, err := e.coord.Seal(ctx, "prasa", ep, true); err != nil {
		t.Fatal(err)
	}
	resp, _ = api(t, "GET", e.srv.URL+"/wacz/prasa/"+strings.TrimPrefix(ep.VolumeKey(1), "weekly/"), tok, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("wacz of prasa: %d", resp.StatusCode)
	}
	resp, _ = api(t, "GET", e.srv.URL+"/wacz/default/"+strings.TrimPrefix(ep.VolumeKey(1), "weekly/"), tok, "", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("default storage has no package: %d", resp.StatusCode)
	}

	e.auth.CreateUser(ctx, "root", "root-password-1", auth.RoleAdmin)
	b := e.browser()
	if resp, _ := b.post(t, "/login", url.Values{"login": {"root"}, "password": {"root-password-1"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatal("login")
	}
	for _, p := range []string{"/system", "/archive?storage=prasa", "/queue", "/queue?state=done"} {
		resp, body := b.get(t, p)
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, "prasa") {
			t.Fatalf("%s does not show the storage: %d", p, resp.StatusCode)
		}
	}
}

func TestSourcesAPI(t *testing.T) {
	e := newEnv(t, "prasa")
	ctx := context.Background()
	editor, _ := e.auth.CreateUser(ctx, "editor", "editor-password", auth.RoleEditor)
	viewer, _ := e.auth.CreateUser(ctx, "viewer", "viewer-password", auth.RoleViewer)
	etok, _ := e.auth.CreateToken(ctx, editor, "ci")
	vtok, _ := e.auth.CreateToken(ctx, viewer, "ro")
	call := func(method, path, tok, body string) (int, string) {
		t.Helper()
		var r io.Reader
		if body != "" {
			r = strings.NewReader(body)
		}
		resp, b := api(t, method, e.srv.URL+path, tok, "application/json", r)
		return resp.StatusCode, string(b)
	}
	if code, _ := call("GET", "/api/sources", vtok, ""); code != http.StatusForbidden {
		t.Fatalf("viewer listed sources: %d", code)
	}
	src := `{"sitemaps":["` + e.site.URL + `/sitemap-index.xml"],"poll":"45m","storage":"prasa","max_size":"2MB"}`
	if code, b := call("PUT", "/api/sources/mapa", etok, src); code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, b)
	}
	if code, _ := call("PUT", "/api/sources/mapa", etok, src); code != http.StatusOK {
		t.Fatalf("replace: %d", code)
	}
	for _, bad := range []string{`{"feeds":["ftp://x"]}`, `{"urls":["https://a.pl/"],"storage":"brak"}`, `{"urls":["https://a.pl/"],"unknown":1}`,
		`{"name":"inna","urls":["https://a.pl/"]}`, `{"urls":["https://a.pl/"],"poll":"10s"}`} {
		if code, _ := call("PUT", "/api/sources/mapa", etok, bad); code != http.StatusBadRequest {
			t.Fatalf("accepted %s: %d", bad, code)
		}
	}
	if code, b := call("POST", "/api/sources/mapa/poll", etok, ""); code != http.StatusOK || !strings.Contains(b, `"new":2`) {
		t.Fatalf("poll: %d %s", code, b)
	}
	code, b := call("GET", "/api/sources", etok, "")
	var list struct {
		Sources []struct {
			Name     string     `json:"name"`
			Poll     string     `json:"poll"`
			Storage  string     `json:"storage"`
			MaxSize  int64      `json:"max_size"`
			LastPoll *time.Time `json:"lastPoll"`
			NextPoll *time.Time `json:"nextPoll"`
			Found    int        `json:"found"`
		} `json:"sources"`
		Blocklists struct {
			URLs []string `json:"urls"`
		} `json:"blocklists"`
	}
	if err := json.Unmarshal([]byte(b), &list); err != nil || code != http.StatusOK || len(list.Sources) != 2 {
		t.Fatalf("list: %d %s %v", code, b, err)
	}
	m := list.Sources[0]
	if m.Name != "mapa" || m.Poll != "45m0s" || m.Storage != "prasa" || m.MaxSize != 2<<20 || m.LastPoll == nil || m.Found != 2 ||
		m.NextPoll.Sub(*m.LastPoll) != 45*time.Minute {
		t.Fatalf("source %+v", m)
	}
	if code, b := call("PUT", "/api/settings/defaults", etok, `{"engine":"http","poll":"2h","max_per_domain":3}`); code != http.StatusOK {
		t.Fatalf("defaults: %d %s", code, b)
	}
	if r := e.coord.Sources().Resolve("test"); r.Engine != "http" || r.Poll != 2*time.Hour || e.coord.Sources().MaxPerDomain() != 3 {
		t.Fatalf("defaults not applied %+v", r)
	}
	if code, _ := call("PUT", "/api/settings/blocklists", etok, `{"urls":["`+e.site.URL+`/blocklist-hosts.txt"],"refresh":"1h"}`); code != http.StatusOK {
		t.Fatal("block lists")
	}
	if code, b := call("POST", "/api/blocklists/update", etok, ""); code != http.StatusOK || !strings.Contains(b, "blocklist-hosts.txt") {
		t.Fatalf("block list update: %d %s", code, b)
	}
	if set, _ := e.coord.Blocklist(ctx); set.Len() == 0 {
		t.Fatal("block list not loaded")
	}
	if code, _ := call("PUT", "/api/settings/blocklists", etok, `null`); code != http.StatusOK || e.coord.Sources().Blocklists != nil {
		t.Fatal("block lists not reset to the default")
	}
	if code, _ := call("DELETE", "/api/sources/mapa", etok, ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if code, _ := call("DELETE", "/api/sources/mapa", etok, ""); code != http.StatusNotFound {
		t.Fatalf("second delete: %d", code)
	}
}

// TestInterruptedSealShown: a week left sealing by a stopped process is
// flagged on the dashboard and on the System page with its attempts.
func TestInterruptedSealShown(t *testing.T) {
	e := newEnv(t)
	b := e.admin(t)
	ctx := context.Background()
	ep := epoch.ID("2026-W43")
	idx := e.coord.Library().Index
	if err := idx.SetEpoch(ctx, index.EpochInfo{Epoch: ep, State: index.StateSealing}); err != nil {
		t.Fatal(err)
	}
	if err := e.coord.RecoverInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	_, body := b.get(t, "/system")
	for _, want := range []string{"Sealing of default/2026-W43 failed", "out of memory", "Attempt 1 of 3", "Sealing attempts", "state-interrupted", "Seal now"} {
		if !strings.Contains(body, want) {
			t.Errorf("system page lacks %q", want)
		}
	}
	if _, body := b.get(t, "/"); !strings.Contains(body, "Sealing default/2026-W43 failed") {
		t.Error("dashboard does not flag the interrupted seal")
	}
}
