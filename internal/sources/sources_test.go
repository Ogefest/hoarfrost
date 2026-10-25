package sources

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ogefest/hoarfrost/internal/config"
	"github.com/ogefest/hoarfrost/internal/testsite"
)

func TestFeedAndSitemap(t *testing.T) {
	site := testsite.New()
	defer site.Close()
	f := NewFetcher("test")
	ctx := context.Background()

	items, err := f.Feed(ctx, site.URL+"/rss.xml")
	if err != nil || len(items) != 2 || items[0].URL != site.URL+"/news/article.html" || items[0].Published.IsZero() {
		t.Fatalf("feed %+v %v", items, err)
	}
	items, err = f.Sitemap(ctx, site.URL+"/sitemap-index.xml")
	if err != nil || len(items) != 2 || items[1].URL != site.URL+"/news/article2.html" {
		t.Fatalf("sitemap %+v %v", items, err)
	}
	site.AddArticle("/news/new.html", "Nowy", "tekst")
	items, _ = f.Feed(ctx, site.URL+"/rss.xml")
	if len(items) != 3 {
		t.Fatalf("added article not in feed: %d", len(items))
	}
}

func TestAtomGzipAndNewsSitemap(t *testing.T) {
	atom := `<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom"><title>x</title>
<entry><title>A</title><link href="https://a.pl/1"/><updated>2026-10-10T10:00:00Z</updated></entry>
<entry><title>Old</title><link href="https://a.pl/old"/><updated>2020-01-01T10:00:00Z</updated></entry></feed>`
	news := `<?xml version="1.0"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9" xmlns:news="http://www.google.com/schemas/sitemap-news/0.9">
<url><loc>https://a.pl/2</loc><news:news><news:title>Dwa</news:title><news:publication_date>2026-10-10T08:00:00+02:00</news:publication_date></news:news></url>
<url><loc>https://a.pl/1</loc></url><url><loc>not a url</loc></url></urlset>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/atom":
			w.Write([]byte(atom))
		case "/news.xml.gz":
			var b bytes.Buffer
			gz := gzip.NewWriter(&b)
			gz.Write([]byte(news))
			gz.Close()
			w.Write(b.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	f := NewFetcher("")
	f.Now = func() time.Time { return time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC) }
	src := config.Source{Name: "a", Feeds: []string{srv.URL + "/atom", srv.URL + "/missing"},
		Sitemaps: []string{srv.URL + "/news.xml.gz"}, URLs: []string{"https://a.pl/static"}}
	items, err := f.Discover(context.Background(), src, 7*24*time.Hour)
	if err == nil {
		t.Fatal("expected error for missing feed")
	}
	var urls []string
	for _, it := range items {
		urls = append(urls, it.URL)
	}
	want := []string{"https://a.pl/1", "https://a.pl/2", "https://a.pl/static"}
	if len(urls) != len(want) {
		t.Fatalf("urls %v", urls)
	}
	for i := range want {
		if urls[i] != want[i] {
			t.Fatalf("urls %v", urls)
		}
	}
	if items[1].Title != "Dwa" || items[1].Published.IsZero() {
		t.Fatalf("news item %+v", items[1])
	}
}

func FuzzSitemap(f *testing.F) {
	f.Add([]byte(`<urlset><url><loc>https://a.pl/</loc></url></urlset>`))
	f.Fuzz(func(t *testing.T, b []byte) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(b) }))
		defer srv.Close()
		NewFetcher("").Sitemap(context.Background(), srv.URL)
	})
}

func TestProbeOrigin(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer site.Close()
	alias := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/temporary":
			http.Redirect(w, r, site.URL+r.URL.RequestURI(), http.StatusFound)
		case "/moved":
			http.Redirect(w, r, site.URL+"/", http.StatusMovedPermanently)
		default:
			http.Redirect(w, r, site.URL+r.URL.RequestURI(), http.StatusMovedPermanently)
		}
	}))
	defer alias.Close()
	f := NewFetcher("test")
	ctx := context.Background()
	for _, c := range []struct{ url, want string }{
		{alias.URL + "/news/a.html?id=1", Origin(site.URL)}, // the whole host redirects
		{alias.URL + "/temporary", Origin(alias.URL)},       // not permanent
		{alias.URL + "/moved", Origin(alias.URL)},           // another page, not another host
		{site.URL + "/news/a.html", Origin(site.URL)},
	} {
		if got, err := f.ProbeOrigin(ctx, c.url); err != nil || got != c.want {
			t.Errorf("%s: %q %v, want %q", c.url, got, err, c.want)
		}
	}
	if Origin("HTTPS://WWW.Example.com/A") != "https://www.example.com" || Origin("bad") != "" {
		t.Fatal("origin")
	}
}
