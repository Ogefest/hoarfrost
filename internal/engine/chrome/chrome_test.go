package chrome

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/ogefest/hoarfrost/internal/engine"
	"github.com/ogefest/hoarfrost/internal/recorder"
	"github.com/ogefest/hoarfrost/internal/testsite"
	"github.com/ogefest/hoarfrost/internal/warcio"
)

func TestCaptureJavaScriptPage(t *testing.T) {
	if testing.Short() || !Available(os.Getenv("HOARFROST_CHROME")) {
		t.Skip("no browser available")
	}
	site := testsite.New()
	defer site.Close()
	e := &Engine{Bin: os.Getenv("HOARFROST_CHROME"), NoSandbox: os.Getuid() == 0}
	defer e.Close()

	for _, path := range []string{"/news/js.html", "/news/article.html"} {
		rec, err := recorder.New(recorder.Options{Epoch: "2026-W41", SpoolDir: t.TempDir(), Engine: engine.Chrome})
		if err != nil {
			t.Fatal(err)
		}
		res, err := e.Capture(context.Background(), engine.Job{URL: site.URL + path, Options: engine.Options{
			Block: []string{"doubleclick.net", "youtube.com"}, ChromeScroll: true, ChromeWait: 1, Attachments: true}}, rec)
		if err != nil {
			t.Fatal(err)
		}
		cf, _ := rec.Close()
		uris := map[string]string{}
		for _, r := range cf.Records {
			if r.Type != warcio.TypeMetadata && r.Type != warcio.TypeRequest {
				uris[strings.TrimPrefix(r.URI, site.URL)] = r.Type
			}
		}
		switch path {
		case "/news/js.html":
			if !strings.Contains(res.Page.Text, "rendered by javascript") {
				t.Fatalf("JavaScript output missing from text: %q", res.Page.Text)
			}
			if uris["/js/app.js"] != warcio.TypeResponse || uris[path] != warcio.TypeResponse {
				t.Fatalf("records %v", uris)
			}
		case "/news/article.html":
			if uris["/css/main.css"] != warcio.TypeResponse || uris["/fonts/b.woff2"] == "" || uris["/img/logo.png"] == "" {
				t.Fatalf("browser resources not recorded: %v", uris)
			}
			if len(res.Discovered) != 1 {
				t.Fatalf("attachments %v", res.Discovered)
			}
		}
		if uris[DOMPrefix+site.URL+path] != warcio.TypeResource || res.Page.Extra["dom"] == "" || res.Page.RecordID == "" {
			t.Fatalf("DOM snapshot missing: %v %+v", uris, res.Page)
		}
		for k := range uris {
			if strings.Contains(k, "doubleclick") || strings.Contains(k, "youtube") {
				t.Fatalf("blocked host recorded: %s", k)
			}
		}
	}
}
