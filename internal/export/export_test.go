package export_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ogefest/hoarfrost/internal/pgdb/pgtest"

	"golang.org/x/net/html"

	"github.com/ogefest/hoarfrost/internal/archive"
	"github.com/ogefest/hoarfrost/internal/engine"
	"github.com/ogefest/hoarfrost/internal/engine/static"
	"github.com/ogefest/hoarfrost/internal/export"
	"github.com/ogefest/hoarfrost/internal/index"
	"github.com/ogefest/hoarfrost/internal/recorder"
	"github.com/ogefest/hoarfrost/internal/storage"
	"github.com/ogefest/hoarfrost/internal/testsite"
)

var blocked = []string{"doubleclick.net", "youtube.com"}

func setup(t *testing.T, paths ...string) (*testsite.Site, *archive.Archive) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	site := testsite.New()
	t.Cleanup(site.Close)
	st, _ := storage.NewFS(filepath.Join(dir, "storage"))
	indexURL, indexSchema := pgtest.Schema(t, "index")
	idx, err := index.Open(ctx, indexURL, indexSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	arc := archive.New(st, idx, filepath.Join(dir, "spool"), nil)
	for _, p := range paths {
		rec, _ := recorder.New(recorder.Options{Epoch: "2026-W41", Dedup: idx.Dedup(""), SpoolDir: dir,
			Blocked: func(h string) bool { return static.HostMatches(h, blocked) }})
		if _, err := (static.Engine{}).Capture(ctx, engine.Job{URL: site.URL + p, Options: engine.Options{SkipFrames: blocked}}, rec); err != nil {
			t.Fatal(err)
		}
		cf, _ := rec.Close()
		f, _ := os.Open(cf.Path)
		st.Put(ctx, cf.Key(), f, cf.Size)
		f.Close()
		idx.AddCapture(ctx, cf, cf.Key())
	}
	return site, arc
}

type blockingClient struct{}

func (blockingClient) RoundTrip(r *http.Request) (*http.Response, error) {
	if static.HostMatches(r.URL.Hostname(), blocked) {
		return nil, http.ErrUseLastResponse
	}
	return http.DefaultTransport.RoundTrip(r)
}

func TestExportFromArchiveMatchesLive(t *testing.T) {
	site, arc := setup(t, "/news/article.html")
	ctx := context.Background()
	opts := export.Options{SkipFrames: blocked}
	fromArchive, err := export.Page(ctx, export.ArchiveFetcher{Library: archive.Single(arc)}, site.URL+"/news/article.html", opts)
	if err != nil {
		t.Fatal(err)
	}
	live, err := export.Page(ctx, export.HTTPFetcher{Client: &http.Client{Transport: blockingClient{}}}, site.URL+"/news/article.html", opts)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fromArchive, live) {
		t.Fatal("export from the archive differs from export of the live page")
	}

	doc, err := html.Parse(bytes.NewReader(fromArchive))
	if err != nil {
		t.Fatal(err)
	}
	orig, _ := os.ReadFile("../testsite/site/article.html")
	origDoc, _ := html.Parse(bytes.NewReader(orig))
	if a, b := text(doc), text(origDoc); a != b {
		t.Fatalf("visible text differs:\n%s\n---\n%s", a, b)
	}
	if n, m := count(doc, "img"), count(origDoc, "img")+1; n != m {
		t.Fatalf("img elements %d, want %d (noscript unwrapped)", n, m)
	}

	s := string(fromArchive)
	if strings.Contains(s, "<script") || strings.Contains(s, "<base") || strings.Contains(s, "data-src") {
		t.Fatal("scripts, base or lazy attributes left")
	}
	srcRe := regexp.MustCompile(`(?:src|href|srcset)="([^"]*)"`)
	for _, m := range srcRe.FindAllStringSubmatch(s, -1) {
		v := m[1]
		if strings.HasPrefix(v, site.URL) && !strings.HasSuffix(v, ".html") && !strings.HasSuffix(v, ".pdf") {
			t.Errorf("resource not inlined: %s", v)
		}
	}
	logo := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("IMG:logo.png"))
	if !strings.Contains(s, logo) {
		t.Fatal("logo not inlined")
	}
	if !strings.Contains(s, `<meta charset="utf-8"/>`) {
		t.Fatal("charset not declared")
	}
	if !strings.Contains(s, `src="data:text/html;charset=utf-8;base64,`) {
		t.Fatal("iframe not inlined")
	}
	if !strings.Contains(s, `href="data:text/css;charset=utf-8;base64,`) {
		t.Fatal("stylesheet not inlined")
	}
	css := extractCSS(t, s)
	if !strings.Contains(css, "@import \"data:text/css") || !strings.Contains(css, "url(\"data:image/png;base64,") {
		t.Fatalf("nested CSS not inlined: %s", css)
	}
}

func extractCSS(t *testing.T, s string) string {
	m := regexp.MustCompile(`href="data:text/css;charset=utf-8;base64,([^"]+)"`).FindStringSubmatch(s)
	if m == nil {
		t.Fatal("no stylesheet")
	}
	b, _ := base64.StdEncoding.DecodeString(m[1])
	return string(b)
}

func text(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && (n.Data == "script" || n.Data == "style" || n.Data == "noscript" || n.Data == "head") {
			return
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}

func count(n *html.Node, tag string) int {
	c := 0
	if n.Type == html.ElementNode && n.Data == tag {
		c++
	}
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		c += count(ch, tag)
	}
	return c
}

func TestRedirectsMissingAndErrors(t *testing.T) {
	site, arc := setup(t, "/redirect", "/files/uchwala.pdf")
	ctx := context.Background()
	f := export.ArchiveFetcher{Library: archive.Single(arc), At: time.Now()}
	out, err := export.Page(ctx, f, site.URL+"/redirect", export.Options{Comment: "Archived copy -- test"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte("Radni przyjęli budżet")) || !bytes.Contains(out, []byte("<!-- Archived copy - - test -->")) {
		t.Fatal("redirect not followed or comment missing")
	}
	if _, err := export.Page(ctx, f, site.URL+"/files/uchwala.pdf", export.Options{}); err == nil {
		t.Fatal("PDF is not exportable as HTML")
	}
	if _, err := export.Page(ctx, f, site.URL+"/nothing", export.Options{}); err == nil {
		t.Fatal("expected not archived")
	}

	small, err := export.Page(ctx, f, site.URL+"/news/article.html", export.Options{MaxBytes: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(small, []byte(`src="data:,"`)) {
		t.Fatal("resources over the limit must be blanked")
	}
}

func TestDataURI(t *testing.T) {
	if got := export.DataURI([]byte("a"), "text/css; charset=UTF-8"); got != "data:text/css;charset=UTF-8;base64,YQ==" {
		t.Fatal(got)
	}
	if got := export.DataURI([]byte("\x89PNG\r\n\x1a\n"), ""); !strings.HasPrefix(got, "data:image/png;base64,") {
		t.Fatal(got)
	}
}

// TestRedirectToOtherHost exports pages captured through redirects to another
// host: news.test -> www.news.test (the same URL for the index, which ignores
// "www.") and 127.0.0.1/elsewhere -> www.news.test, whose relative resources
// must be resolved against the target.
func TestRedirectToOtherHost(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	site := testsite.New()
	t.Cleanup(site.Close)
	addr := site.Listener.Addr().String()
	_, port, _ := net.SplitHostPort(addr)
	st, _ := storage.NewFS(filepath.Join(dir, "storage"))
	indexURL, indexSchema := pgtest.Schema(t, "index")
	idx, err := index.Open(ctx, indexURL, indexSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.Close() })
	arc := archive.New(st, idx, filepath.Join(dir, "spool"), nil)
	bare := "http://news.test:" + port + "/news/article.html"
	moved := site.URL + "/elsewhere"
	// Asking for the time of the capture's start makes the redirect record
	// the closest one, as for a page viewed in the UI.
	start := time.Now()
	for _, u := range []string{bare, moved} {
		rec, _ := recorder.New(recorder.Options{Epoch: "2026-W41", SpoolDir: dir,
			Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			}})
		if _, err := (static.Engine{}).Capture(ctx, engine.Job{URL: u}, rec); err != nil {
			t.Fatal(err)
		}
		cf, _ := rec.Close()
		f, _ := os.Open(cf.Path)
		st.Put(ctx, cf.Key(), f, cf.Size)
		f.Close()
		idx.AddCapture(ctx, cf, cf.Key())
	}
	f := export.ArchiveFetcher{Library: archive.Single(arc), At: start}
	for _, u := range []string{bare, moved} {
		out, err := export.Page(ctx, f, u, export.Options{})
		if err != nil {
			t.Fatal(u, err)
		}
		if !bytes.Contains(out, []byte("Radni przyjęli budżet")) || !bytes.Contains(out, []byte("data:text/css")) {
			t.Fatalf("%s exported without the target page or its styles:\n%.500s", u, out)
		}
	}
}
