// Package e2e runs the hoarfrost binary end to end: configure
// an RSS source, capture with an embedded and a remote worker, upload a page,
// seal the week, search, export, and rebuild the index on a fresh instance
// attached to the same storage.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ogefest/hoarfrost/internal/archive"

	"github.com/ogefest/hoarfrost/internal/pgdb/pgtest"

	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/index"
	"github.com/ogefest/hoarfrost/internal/storage"
	"github.com/ogefest/hoarfrost/internal/testsite"
	"github.com/ogefest/hoarfrost/internal/wacz"
	"github.com/ogefest/hoarfrost/internal/warcio"
)

var binary string

func TestMain(m *testing.M) {
	if os.Getenv("HOARFROST_E2E_SKIP") != "" {
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("", "hoarfrost-e2e-bin-")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(dir, "hoarfrost")
	out, err := exec.Command("go", "build", "-o", binary, "../cmd/hoarfrost").CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build failed: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type instance struct {
	t        *testing.T
	env      []string
	dataDir  string
	database string
	addr     string
}

func freePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func newInstance(t *testing.T, storageURL string, extraStorages ...string) *instance {
	dir := t.TempDir()
	in := &instance{t: t, dataDir: dir, database: pgtest.Database(t), addr: freePort(t)}
	in.env = append(os.Environ(),
		"HOARFROST_DATABASE="+in.database,
		"HOARFROST_DATA_DIR="+dir,
		"HOARFROST_STORAGE="+storageURL,
		"HOARFROST_SECRET=e2e-secret-0123456789-0123456789-xyz",
		"HOARFROST_LISTEN="+in.addr,
		"HOARFROST_INSECURE_COOKIE=1",
		"HOARFROST_WORKER_TOKEN=e2e-worker-token",
		"HOARFROST_WORKERS=1",
		"HOARFROST_LOG_LEVEL=warn",
		"HOARFROST_COORDINATOR=http://"+in.addr,
		"HOARFROST_STORAGES="+strings.Join(extraStorages, ","),
	)
	return in
}

func (in *instance) run(stdin string, args ...string) string {
	in.t.Helper()
	cmd := exec.Command(binary, append([]string{"-env", "/nonexistent"}, args...)...)
	cmd.Env = in.env
	cmd.Stdin = strings.NewReader(stdin)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		in.t.Fatalf("hoarfrost %v: %v\n%s\n%s", args, err, out.String(), errOut.String())
	}
	return strings.TrimSpace(out.String())
}

func (in *instance) start(extraEnv []string, args ...string) *exec.Cmd {
	in.t.Helper()
	cmd := exec.Command(binary, append([]string{"-env", "/nonexistent"}, args...)...)
	cmd.Env = append(append([]string{}, in.env...), extraEnv...)
	logf, _ := os.Create(filepath.Join(in.dataDir, args[0]+".log"))
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		in.t.Fatal(err)
	}
	in.t.Cleanup(func() {
		cmd.Process.Signal(syscall.SIGTERM)
		cmd.Wait()
		logf.Close()
		if in.t.Failed() {
			b, _ := os.ReadFile(logf.Name())
			in.t.Logf("%s log:\n%s", args[0], b)
		}
	})
	return cmd
}

func stop(t *testing.T, cmd *exec.Cmd) {
	cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		cmd.Process.Kill()
		t.Fatal("process did not stop on SIGTERM")
	}
}

func (in *instance) api(method, path, token, ct string, body io.Reader) (int, []byte) {
	in.t.Helper()
	req, _ := http.NewRequest(method, "http://"+in.addr+path, body)
	req.Header.Set("Authorization", "Bearer "+token)
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

type searchResult struct {
	Total   int `json:"total"`
	Results []struct {
		ID     int64  `json:"id"`
		URL    string `json:"url"`
		Title  string `json:"title"`
		Engine string `json:"engine"`
		Kind   string `json:"kind"`
	} `json:"results"`
}

func (in *instance) search(token, query string) searchResult {
	var sr searchResult
	_, b := in.api("GET", "/api/search?limit=100&"+query, token, "", nil)
	json.Unmarshal(b, &sr)
	return sr
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func storageURL(t *testing.T) string {
	if raw := os.Getenv("HOARFROST_TEST_S3"); raw != "" {
		u, _ := url.Parse(raw)
		u.Path = strings.TrimSuffix(u.Path, "/") + fmt.Sprintf("/e2e-%d", time.Now().UnixNano())
		return u.String()
	}
	return "file://" + t.TempDir()
}

func TestEndToEnd(t *testing.T) {
	site := testsite.New()
	defer site.Close()
	for i := range 3 {
		site.AddArticle(fmt.Sprintf("/news/e2e-%d.html", i), fmt.Sprintf("Sesja rady numer %d", i),
			strings.Repeat(fmt.Sprintf("Radni na sesji numer %d rozmawiali o budżecie obywatelskim i inwestycjach. ", i), 8))
	}
	st := storageURL(t)
	prasaDir := t.TempDir()
	prasa := "prasa=file://" + prasaDir
	a := newInstance(t, st, prasa)
	a.run("admin-password-e2e\n", "user", "-role", "admin", "-password-stdin", "add", "admin")
	token := a.run("", "user", "token", "admin", "e2e")
	if !strings.HasPrefix(token, "hf_") {
		t.Fatalf("token %q", token)
	}

	serve := a.start(nil, "serve", "-engines", "static,http")
	waitFor(t, "web server", 20*time.Second, func() bool {
		code, _ := a.api("GET", "/healthz", "", "", nil)
		return code == 200
	})
	// The instance starts without sources; they are configured through the
	// API, as an administrator would in the web UI.
	for _, c := range []struct{ method, path, body string }{
		{"PUT", "/api/settings/blocklists", fmt.Sprintf(`{"urls":["%[1]s/blocklist-hosts.txt","%[1]s/blocklist-adblock.txt"],"refresh":"1h"}`, site.URL)},
		{"POST", "/api/blocklists/update", ""},
		{"PUT", "/api/settings/defaults", `{"fallback":"http","min_text_chars":200}`},
		{"PUT", "/api/sources/miasto", fmt.Sprintf(`{"feeds":["%s/rss.xml"]}`, site.URL)},
		{"PUT", "/api/sources/kultura", fmt.Sprintf(`{"storage":"prasa","urls":["%s/news/article2.html"]}`, site.URL)},
		{"POST", "/api/sources/miasto/poll", ""},
		{"POST", "/api/sources/kultura/poll", ""},
	} {
		var body io.Reader
		if c.body != "" {
			body = strings.NewReader(c.body)
		}
		if code, b := a.api(c.method, c.path, token, "application/json", body); code >= 300 {
			t.Fatalf("%s %s: %d %s", c.method, c.path, code, b)
		}
	}
	remote := a.start([]string{"HOARFROST_DATA_DIR=" + t.TempDir(), "HOARFROST_WORKER_NAME=remote"}, "worker", "-engines", "static,http", "-slots", "2")

	waitFor(t, "captures", 90*time.Second, func() bool {
		return a.search(token, "kind=page&storage=default").Total >= 5 && a.search(token, "kind=attachment&storage=default").Total >= 1 &&
			a.search(token, "kind=attachment&storage=prasa").Total >= 1
	})

	var mp bytes.Buffer
	mw := multipart.NewWriter(&mp)
	fw, _ := mw.CreateFormFile("file", "saved.html")
	fw.Write([]byte(`<!DOCTYPE html><html><!-- Page saved with SingleFile url: https://example.org/z-rozszerzenia --><head><title>Zapis z rozszerzenia</title></head><body><p>Strona zapisana w przeglądarce użytkownika.</p></body></html>`))
	mw.Close()
	if code, b := a.api("POST", "/api/upload", token, mw.FormDataContentType(), &mp); code != http.StatusCreated {
		t.Fatalf("upload: %d %s", code, b)
	}
	if code, b := a.api("POST", "/api/upload?storage=prasa&url=https://example.org/do-prasy", token, "text/html",
		strings.NewReader("<html><title>Do prasy</title><p>Zapis skierowany do drugiego magazynu.</p></html>")); code != http.StatusCreated {
		t.Fatalf("upload to prasa: %d %s", code, b)
	}
	if got := a.search(token, "storage=prasa&q=magazynu"); got.Total != 1 {
		t.Fatalf("upload not routed to prasa: %+v", got)
	}
	if got := a.search(token, "storage=default&q=magazynu"); got.Total != 0 {
		t.Fatal("upload to prasa visible in the default storage")
	}

	res := a.search(token, "q=obywatelskim")
	if res.Total != 3 {
		t.Fatalf("full-text search: %+v", res)
	}
	art := a.search(token, "q=tramwajowej&storage=default").Results
	if len(art) != 1 {
		t.Fatalf("article search: %+v", art)
	}
	_, exportBefore := a.api("GET", fmt.Sprintf("/api/export/%d.html", art[0].ID), token, "", nil)
	if !bytes.Contains(exportBefore, []byte("data:image/png;base64")) {
		t.Fatal("export did not inline images")
	}
	if code, b := a.api("GET", "/api/search?q=doubleclick", token, "", nil); code != 200 || bytes.Contains(b, []byte(`"total":1`)) {
		t.Fatalf("blocked domain archived: %s", b)
	}

	ep := epoch.Of(time.Now())
	stop(t, remote)
	out := a.run("", "seal", "-epoch", string(ep), "-force")
	if !strings.Contains(out, "state verified") {
		t.Fatalf("seal: %s", out)
	}
	if out := a.run("", "seal", "-storage", "prasa", "-epoch", string(ep), "-force"); !strings.Contains(out, "prasa/"+string(ep)+" sealed") {
		t.Fatalf("seal prasa: %s", out)
	}
	if out := a.run("", "verify"); !strings.Contains(out, "default/"+string(ep)+": ok") || !strings.Contains(out, "prasa/"+string(ep)+": ok") {
		t.Fatalf("verify: %s", out)
	}
	assertNoBlockedDomains(t, st, ep)
	_, exportAfter := a.api("GET", fmt.Sprintf("/api/export/%d.html", art[0].ID), token, "", nil)
	if !bytes.Equal(stripComment(exportBefore), stripComment(exportAfter)) {
		t.Fatal("export changed after sealing")
	}
	if code, _ := a.api("GET", fmt.Sprintf("/replay/%d", art[0].ID), token, "", nil); code != 200 {
		t.Fatalf("replay after seal: %d", code)
	}
	stop(t, serve)

	b := newInstance(t, st, prasa)
	out = b.run("", "reindex")
	if !strings.Contains(out, "rebuilt 2 epoch") {
		t.Fatalf("reindex: %s", out)
	}
	if da, db := dump(t, a), dump(t, b); da != db {
		t.Fatalf("index of the fresh instance differs:\n%s", firstDiff(da, db))
	}
	exportCLI := b.run("", "export", "-url", site.URL+"/news/article2.html")
	if !bytes.Equal(stripComment([]byte(exportCLI)), bytes.TrimSpace(stripComment(exportAfter))) {
		t.Fatal("export from the reindexed instance differs")
	}

	pkg := filepath.Join(t.TempDir(), "week.wacz")
	s, err := storage.Open(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	data, err := storage.ReadAll(context.Background(), s, ep.WeeklyKey())
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(pkg, data, 0o644)
	table, err := storage.ReadAll(context.Background(), s, ep.ParquetKey())
	if err != nil {
		t.Fatalf("page table: %v", err)
	}
	rows, err := archive.ReadPageTable(bytes.NewReader(table), int64(len(table)))
	if err != nil || len(rows) == 0 {
		t.Fatalf("page table: %d rows, %v", len(rows), err)
	}
	found := false
	for _, r := range rows {
		found = found || strings.HasSuffix(r.URL, "/news/article2.html") && r.Description != "" && len(r.Authors) == 1
	}
	if !found {
		t.Fatalf("page table lacks article2 with description and author: %+v", rows)
	}
	conformance(t, pkg)
	conformance(t, filepath.Join(prasaDir, filepath.FromSlash(ep.WeeklyKey())))
	exitTest(t, st, exportAfter)
}

// exitTest runs scripts/exit-test.sh (the exit test) against the storage,
// using rclone for S3.
func exitTest(t *testing.T, storageURL string, export []byte) {
	if _, err := exec.LookPath("wb-manager"); err != nil {
		if os.Getenv("HOARFROST_CONFORMANCE") == "required" {
			t.Fatal("pywb is required")
		}
		t.Log("pywb not installed, skipping exit test")
		return
	}
	src := strings.TrimPrefix(storageURL, "file://")
	env := os.Environ()
	if strings.HasPrefix(storageURL, "s3://") {
		if _, err := exec.LookPath("rclone"); err != nil {
			t.Log("rclone not installed, skipping exit test on S3")
			return
		}
		u, _ := url.Parse(storageURL)
		q := u.Query()
		src = "hoarfrost:" + u.Host + u.Path
		env = append(env, "RCLONE_CONFIG_HOARFROST_TYPE=s3", "RCLONE_CONFIG_HOARFROST_PROVIDER=Other",
			"RCLONE_CONFIG_HOARFROST_ACCESS_KEY_ID="+os.Getenv("AWS_ACCESS_KEY_ID"),
			"RCLONE_CONFIG_HOARFROST_SECRET_ACCESS_KEY="+os.Getenv("AWS_SECRET_ACCESS_KEY"),
			"RCLONE_CONFIG_HOARFROST_ENDPOINT="+q.Get("endpoint"), "RCLONE_CONFIG_HOARFROST_REGION="+q.Get("region"),
			"RCLONE_CONFIG_HOARFROST_FORCE_PATH_STYLE=true")
	}
	exp := filepath.Join(t.TempDir(), "export.html")
	os.WriteFile(exp, export, 0o644)
	script, _ := filepath.Abs("../scripts/exit-test.sh")
	cmd := exec.Command("bash", script, src, exp)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("exit test failed: %v\n%s", err, out)
	}
	t.Logf("exit test:\n%s", out)
}

var commentRe = []byte("<!-- Archived copy")

func stripComment(b []byte) []byte {
	if i := bytes.Index(b, commentRe); i >= 0 {
		if j := bytes.Index(b[i:], []byte("-->")); j >= 0 {
			return append(append([]byte{}, b[:i]...), b[i+j+3:]...)
		}
	}
	return b
}

func dump(t *testing.T, in *instance) string {
	idx, err := index.Open(context.Background(), in.database, index.Schema)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	d, err := idx.Dump(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func firstDiff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < max(len(al), len(bl)); i++ {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			return "- " + x + "\n+ " + y
		}
	}
	return ""
}

// conformance runs the checks with independent tools when the tools
// are installed (CI installs them).
func conformance(t *testing.T, pkg string) {
	script, _ := filepath.Abs("../scripts/conformance.sh")
	if _, err := exec.LookPath("warcio"); err != nil {
		if os.Getenv("HOARFROST_CONFORMANCE") == "required" {
			t.Fatal("warcio is required")
		}
		t.Log("warcio not installed, skipping conformance suite")
		return
	}
	out, err := exec.Command("bash", script, pkg).CombinedOutput()
	if err != nil {
		t.Fatalf("conformance suite failed: %v\n%s", err, out)
	}
	t.Logf("conformance:\n%s", out)
}

// assertNoBlockedDomains checks that no domain from the block lists reached
// the sealed package and that capture files record the list version.
func assertNoBlockedDomains(t *testing.T, storageURL string, ep epoch.ID) {
	t.Helper()
	ctx := context.Background()
	s, err := storage.Open(ctx, storageURL)
	if err != nil {
		t.Fatal(err)
	}
	data, err := storage.ReadAll(ctx, s, ep.WeeklyKey())
	if err != nil {
		t.Fatal(err)
	}
	r, err := wacz.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	lines, _ := r.CDXJ()
	for _, l := range lines {
		if strings.Contains(l.URL, "doubleclick") || strings.Contains(l.URL, "youtube") {
			t.Fatalf("blocked domain in the archive: %s", l.URL)
		}
	}
	offsets, _ := r.WARCOffsets()
	withVersion := 0
	for name := range offsets {
		sec, _ := r.Entry(wacz.ArchiveDir + name)
		rd := warcio.NewReader(sec)
		info, _, _, err := rd.Next()
		if err == nil && bytes.Contains(info.Block, []byte("blocklist: ")) {
			withVersion++
		}
	}
	if withVersion == 0 {
		t.Fatal("no capture records the block list version in warcinfo")
	}
}
