// Package testsite serves a small deterministic news site used by engine,
// worker and end-to-end tests: shared CSS and fonts, lazy loading, srcset,
// iframes, a PDF attachment, redirects, conditional requests, RSS and a sitemap.
//
// Requests for a host under .test without "www." are redirected to the same
// URL on the www. host, like sites with a canonical host do; tests point such
// hosts at the server with a custom dialer.
package testsite

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

//go:embed site
var siteFS embed.FS

// Articles lists the article paths published in the feeds.
var Articles = []string{"/news/article.html", "/news/article2.html"}

// Site is a running test site.
type Site struct {
	*httptest.Server
	// URL is the base URL used in feeds and sitemaps.
	URL   string
	mu    sync.Mutex
	hits  map[string]int
	extra map[string]string
}

// New starts the site on a random local port.
func New() *Site {
	s := NewHandler("")
	s.Server = httptest.NewServer(s)
	s.URL = s.Server.URL
	return s
}

// NewHandler returns the site without starting a server, for use with a
// custom listener; baseURL is used in feeds and sitemaps.
func NewHandler(baseURL string) *Site {
	return &Site{URL: baseURL, hits: map[string]int{}, extra: map[string]string{}}
}

// Close stops the test server, if one was started.
func (s *Site) Close() {
	if s.Server != nil {
		s.Server.Close()
	}
}

// Hits returns how many times path was requested.
func (s *Site) Hits(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[path]
}

// AddArticle publishes an additional article in the feeds.
func (s *Site) AddArticle(path, title, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.extra[path] = fmt.Sprintf(`<!DOCTYPE html><html><head><meta charset="utf-8"><title>%s</title><link rel="stylesheet" href="/css/main.css"></head><body><article><h1>%s</h1><p>%s</p></article></body></html>`, title, title, body)
}

var lastModified = time.Date(2026, 10, 9, 6, 0, 0, 0, time.UTC)

func (s *Site) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.hits[r.URL.Path]++
	extra, isExtra := s.extra[r.URL.Path]
	s.mu.Unlock()
	w.Header()["Date"] = nil

	host, port, _ := net.SplitHostPort(r.Host)
	switch p := r.URL.Path; {
	case strings.HasSuffix(host, ".test") && !strings.HasPrefix(host, "www."):
		http.Redirect(w, r, "http://www."+r.Host+r.URL.RequestURI(), http.StatusMovedPermanently)
	case p == "/elsewhere":
		http.Redirect(w, r, "http://www.news.test:"+port+"/news/article.html", http.StatusFound)
	case isExtra:
		write(w, "text/html; charset=utf-8", []byte(extra))
	case p == "/redirect":
		http.Redirect(w, r, "/news/article.html", http.StatusMovedPermanently)
	case p == "/files/uchwala.pdf":
		write(w, "application/pdf", PDF("Uchwala budzetowa na rok 2027"))
	case p == "/etag.css":
		w.Header().Set("ETag", `"v1"`)
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		write(w, "text/css", []byte("p{margin:0}"))
	case p == "/rss.xml":
		write(w, "application/rss+xml", s.rss())
	case p == "/sitemap.xml":
		write(w, "application/xml", s.sitemap())
	case p == "/sitemap-index.xml":
		write(w, "application/xml", []byte(`<?xml version="1.0"?><sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9"><sitemap><loc>`+s.URL+`/sitemap.xml</loc></sitemap></sitemapindex>`))
	case p == "/blocklist-hosts.txt":
		write(w, "text/plain", []byte("# test hosts list\n0.0.0.0 ads.doubleclick.net\n0.0.0.0 www.youtube.com\n"))
	case p == "/blocklist-adblock.txt":
		write(w, "text/plain", []byte("[Adblock Plus 2.0]\n||doubleclick.net^\n||youtube.com^$third-party\n"))
	case p == "/error":
		http.Error(w, "boom", http.StatusInternalServerError)
	default:
		name := strings.TrimPrefix(p, "/")
		name = strings.TrimPrefix(name, "news/")
		b, err := fs.ReadFile(siteFS, "site/"+name)
		if err != nil || name == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Last-Modified", lastModified.Format(http.TimeFormat))
		write(w, contentType(name), b)
	}
}

func write(w http.ResponseWriter, ct string, b []byte) {
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", fmt.Sprint(len(b)))
	w.Write(b)
}

func contentType(name string) string {
	switch {
	case strings.HasSuffix(name, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		return "text/css"
	case strings.HasSuffix(name, ".js"):
		return "application/javascript"
	case strings.HasSuffix(name, ".png"):
		return "image/png"
	case strings.HasSuffix(name, ".jpg"):
		return "image/jpeg"
	case strings.HasSuffix(name, ".webp"):
		return "image/webp"
	case strings.HasSuffix(name, ".ico"):
		return "image/x-icon"
	case strings.Contains(name, ".woff"):
		return "font/woff2"
	}
	return "application/octet-stream"
}

func (s *Site) paths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]string(nil), Articles...)
	for p := range s.extra {
		out = append(out, p)
	}
	return out
}

func (s *Site) rss() []byte {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><rss version="2.0"><channel><title>Test</title>`)
	for _, p := range s.paths() {
		fmt.Fprintf(&b, `<item><title>%s</title><link>%s%s</link><pubDate>%s</pubDate></item>`, p, s.URL, p, lastModified.Format(time.RFC1123Z))
	}
	b.WriteString(`</channel></rss>`)
	return b.Bytes()
}

func (s *Site) sitemap() []byte {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`)
	for _, p := range s.paths() {
		fmt.Fprintf(&b, `<url><loc>%s%s</loc><lastmod>2026-10-09</lastmod></url>`, s.URL, p)
	}
	b.WriteString(`</urlset>`)
	return b.Bytes()
}

// PDF builds a minimal one-page PDF showing text (ASCII only).
func PDF(text string) []byte {
	stream := fmt.Sprintf("BT /F1 12 Tf 72 720 Td (%s) Tj ET", text)
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes()
}
