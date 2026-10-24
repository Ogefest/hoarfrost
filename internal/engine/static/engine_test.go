package static_test

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/ogefest/hoarfrost/internal/engine"
	"github.com/ogefest/hoarfrost/internal/engine/static"
	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/recorder"
	"github.com/ogefest/hoarfrost/internal/testsite"
	"github.com/ogefest/hoarfrost/internal/warcio"
)

var update = flag.Bool("update", false, "rewrite golden files")

// committed simulates the coordinator index: only records of finished
// captures are visible to deduplication, scoped by epoch.
type committed struct {
	mu   sync.Mutex
	refs map[epoch.ID]map[string]recorder.Ref
}

func (c *committed) Lookup(_ context.Context, ep epoch.ID, d string) (*recorder.Ref, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, ok := c.refs[ep][d]; ok {
		return &r, nil
	}
	return nil, nil
}

func (c *committed) commit(cf recorder.CaptureFile) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.refs == nil {
		c.refs = map[epoch.ID]map[string]recorder.Ref{}
	}
	if c.refs[cf.Epoch] == nil {
		c.refs[cf.Epoch] = map[string]recorder.Ref{}
	}
	for _, r := range cf.Records {
		if (r.Type == warcio.TypeResponse || r.Type == warcio.TypeResource) && r.Digest != "" {
			if _, ok := c.refs[cf.Epoch][r.Digest]; !ok {
				c.refs[cf.Epoch][r.Digest] = recorder.Ref{RecordID: r.ID, URI: r.URI, Date: r.Date}
			}
		}
	}
}

var opts = engine.Options{
	Block:        []string{"doubleclick.net"},
	SkipFrames:   []string{"youtube.com"},
	Attachments:  true,
	MinTextChars: 300,
	Fallback:     "chrome",
}

func capture(t *testing.T, site *testsite.Site, path string, ep epoch.ID, dedup recorder.Dedup) (engine.Result, recorder.CaptureFile) {
	t.Helper()
	rec, err := recorder.New(recorder.Options{
		Epoch: ep, Dedup: dedup, SpoolDir: t.TempDir(), Source: "test", Engine: engine.Static,
		Blocked: func(h string) bool { return static.HostMatches(h, opts.Block) },
	})
	if err != nil {
		t.Fatal(err)
	}
	job := engine.Job{URL: site.URL + path, Source: "test", Engine: engine.Static, Epoch: ep, Options: opts}
	res, err := static.Engine{}.Capture(context.Background(), job, rec)
	if err != nil {
		t.Fatal(err)
	}
	cf, err := rec.Close()
	if err != nil {
		t.Fatal(err)
	}
	return res, cf
}

// summary renders a capture in a stable, host-independent form.
func summary(t *testing.T, site *testsite.Site, cf recorder.CaptureFile) string {
	f, err := os.Open(cf.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	recs, err := warcio.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, r := range recs {
		uri := strings.ReplaceAll(r.TargetURI(), site.URL, "http://site")
		line := fmt.Sprintf("%-8s %s", r.Type(), uri)
		switch r.Type() {
		case warcio.TypeResponse, warcio.TypeRevisit:
			resp, err := warcio.ParseHTTPResponse(r.Block)
			if err == nil {
				line += fmt.Sprintf(" %d %s %s", resp.StatusCode, resp.Header.Get("Content-Type"), r.Header.Get(warcio.HPayloadDigest))
			}
			if r.Type() == warcio.TypeRevisit {
				line += " -> " + strings.ReplaceAll(r.Header.Get(warcio.HRefersToTargetURI), site.URL, "http://site")
			}
		case warcio.TypeRequest:
			req := string(r.Block)
			first, _, _ := strings.Cut(req, "\r\n")
			line += " " + first
		case warcio.TypeMetadata:
			line += " " + r.Header.Get(warcio.HContentType)
		case warcio.TypeWarcinfo:
			continue
		}
		lines = append(lines, line)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n") + "\n"
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		os.MkdirAll("testdata", 0o755)
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update)", err)
	}
	if got != string(want) {
		t.Errorf("%s mismatch:\n--- got\n%s--- want\n%s", name, got, want)
	}
}

func TestCaptureArticleGolden(t *testing.T) {
	site := testsite.New()
	defer site.Close()
	res, cf := capture(t, site, "/news/article.html", "2026-W41", &committed{})

	if res.Page.Title != "Radni przyjęli budżet miasta" || !strings.Contains(res.Page.Text, "transport publiczny") {
		t.Fatalf("page %+v", res.Page)
	}
	if res.Page.Published != "2026-10-09" || res.NeedsFallback {
		t.Fatalf("published %q fallback %v", res.Page.Published, res.NeedsFallback)
	}
	if len(res.Discovered) != 1 || res.Discovered[0] != site.URL+"/files/uchwala.pdf" {
		t.Fatalf("discovered %v", res.Discovered)
	}
	if site.Hits("/files/uchwala.pdf") != 0 {
		t.Fatal("attachment must become a new job, not be fetched inline")
	}
	if site.Hits("/nested.html") != 0 {
		t.Fatal("nested iframes beyond depth 1 must not be fetched")
	}
	if res.Page.Failed != 0 {
		t.Fatalf("failed resources: %d", res.Page.Failed)
	}
	golden(t, "article.golden", summary(t, site, cf))
}

func TestDedupWithinEpochOnly(t *testing.T) {
	site := testsite.New()
	defer site.Close()
	idx := &committed{}
	_, first := capture(t, site, "/news/article.html", "2026-W41", idx)
	idx.commit(first)
	_, second := capture(t, site, "/news/article2.html", "2026-W41", idx)
	golden(t, "article2-same-epoch.golden", summary(t, site, second))

	revisits := 0
	for _, r := range second.Records {
		if r.Type == warcio.TypeRevisit {
			revisits++
			found := false
			for _, o := range first.Records {
				if o.ID == r.RefersTo && o.Digest == r.Digest {
					found = true
				}
			}
			if !found {
				t.Errorf("revisit %s points to unknown record", r.URI)
			}
		}
	}
	if revisits < 4 {
		t.Fatalf("expected shared resources as revisits, got %d", revisits)
	}

	_, third := capture(t, site, "/news/article2.html", "2026-W42", idx)
	for _, r := range third.Records {
		if r.Type == warcio.TypeRevisit {
			t.Fatalf("revisit across epochs: %s", r.URI)
		}
	}
}

func TestFallbackForJavaScriptShell(t *testing.T) {
	site := testsite.New()
	defer site.Close()
	res, _ := capture(t, site, "/news/js.html", "2026-W41", &committed{})
	if !res.NeedsFallback {
		t.Fatalf("expected fallback, text %q", res.Page.Text)
	}
}

func TestRedirectAndErrors(t *testing.T) {
	site := testsite.New()
	defer site.Close()
	res, _ := capture(t, site, "/redirect", "2026-W41", &committed{})
	if res.Page.Extra["finalUrl"] != site.URL+"/news/article.html" || res.Page.Resources == 0 {
		t.Fatalf("redirect page %+v", res.Page)
	}

	rec, _ := recorder.New(recorder.Options{Epoch: "2026-W41", SpoolDir: t.TempDir()})
	defer rec.Abort()
	_, err := static.Engine{}.Capture(context.Background(), engine.Job{URL: site.URL + "/error", Options: opts}, rec)
	var se *engine.StatusError
	if err == nil || !strings.Contains(err.Error(), "500") || !errors.As(err, &se) {
		t.Fatalf("want status error, got %v", err)
	}
}

func TestHTTPEnginePDF(t *testing.T) {
	site := testsite.New()
	defer site.Close()
	rec, _ := recorder.New(recorder.Options{Epoch: "2026-W41", SpoolDir: t.TempDir()})
	res, err := engine.HTTPEngine{}.Capture(context.Background(), engine.Job{URL: site.URL + "/files/uchwala.pdf"}, rec)
	if err != nil {
		t.Fatal(err)
	}
	cf, _ := rec.Close()
	if res.Page.Kind != "attachment" || res.Page.Mime != "application/pdf" {
		t.Fatalf("page %+v", res.Page)
	}
	if len(cf.Records) != 4 {
		t.Fatalf("records %d", len(cf.Records))
	}
}
