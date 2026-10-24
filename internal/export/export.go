// Package export assembles a single self-contained HTML file from a page
// and its resources, inlined as data: URIs ("monolith from the archive").
// The page walk is shared with the static engine; only the data source differs.
package export

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/ogefest/hoarfrost/internal/archive"
	"github.com/ogefest/hoarfrost/internal/engine/static"
	"github.com/ogefest/hoarfrost/internal/index"
)

// Fetcher returns the content of a URL and the URL it finally came from
// after redirects, against which relative references are resolved.
type Fetcher interface {
	Fetch(ctx context.Context, url string) (body []byte, contentType, finalURL string, err error)
}

// ErrNotArchived is returned when the archive has no capture of a URL.
var ErrNotArchived = errors.New("export: not archived")

// ArchiveFetcher reads captures from the archives, choosing the capture
// closest to At (preferring Storage) and following archived redirects.
type ArchiveFetcher struct {
	Library *archive.Library
	Storage string
	At      time.Time
}

func (f ArchiveFetcher) Fetch(ctx context.Context, rawURL string) ([]byte, string, string, error) {
	for range 6 {
		rec, err := f.Library.Index.ClosestIn(ctx, f.Storage, rawURL, f.At)
		if errors.Is(err, index.ErrNotFound) {
			return nil, "", "", fmt.Errorf("%w: %s", ErrNotArchived, rawURL)
		}
		if err != nil {
			return nil, "", "", err
		}
		res, err := f.Library.Fetch(ctx, rec)
		if err != nil {
			return nil, "", "", err
		}
		if res.Status >= 300 && res.Status < 400 {
			if loc := res.Header.Get("Location"); loc != "" {
				base, _ := url.Parse(rawURL)
				if next := static.Resolve(base, loc); next != "" && next != rawURL {
					rawURL = next
					continue
				}
			}
		}
		if res.Status >= 400 {
			return res.Body, res.ContentType, rawURL, fmt.Errorf("export: %s archived with HTTP %d", rawURL, res.Status)
		}
		return res.Body, res.ContentType, rawURL, nil
	}
	return nil, "", "", fmt.Errorf("export: too many redirects for %s", rawURL)
}

// HTTPFetcher fetches from the network with any client, e.g. a recorder's.
type HTTPFetcher struct{ Client *http.Client }

func (f HTTPFetcher) Fetch(ctx context.Context, rawURL string) ([]byte, string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", "", err
	}
	resp, err := f.Client.Do(req)
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	if _, err := b.ReadFrom(resp.Body); err != nil {
		return nil, "", "", err
	}
	final := resp.Request.URL.String()
	if resp.StatusCode >= 400 {
		return b.Bytes(), resp.Header.Get("Content-Type"), final, fmt.Errorf("export: %s returned HTTP %d", rawURL, resp.StatusCode)
	}
	return b.Bytes(), resp.Header.Get("Content-Type"), final, nil
}

// Options control the export.
type Options struct {
	// KeepScripts keeps <script> elements (off by default: exports are static).
	KeepScripts bool
	// Comment is added as an HTML comment at the top of <head>.
	Comment string
	// MaxBytes limits the total size of inlined resources.
	MaxBytes int64
	// SkipFrames lists domains whose iframes are dropped.
	SkipFrames []string
}

type exporter struct {
	ctx   context.Context
	f     Fetcher
	opts  Options
	mu    sync.Mutex
	cache map[string]string
	total int64
}

// Page exports pageURL as a single HTML document.
func Page(ctx context.Context, f Fetcher, pageURL string, opts Options) ([]byte, error) {
	if opts.MaxBytes == 0 {
		opts.MaxBytes = 200 << 20
	}
	e := &exporter{ctx: ctx, f: f, opts: opts, cache: map[string]string{}}
	body, ct, final, err := f.Fetch(ctx, pageURL)
	if err != nil {
		return nil, err
	}
	if mt := mediaType(ct); mt != "" && mt != "text/html" && mt != "application/xhtml+xml" {
		return nil, fmt.Errorf("export: %s is %s, not HTML", pageURL, mt)
	}
	return e.document(body, ct, final, 0)
}

// PageFromBody exports an already loaded document, e.g. a rendered DOM
// snapshot, resolving its resources against pageURL.
func PageFromBody(ctx context.Context, f Fetcher, body []byte, contentType, pageURL string, opts Options) ([]byte, error) {
	if opts.MaxBytes == 0 {
		opts.MaxBytes = 200 << 20
	}
	e := &exporter{ctx: ctx, f: f, opts: opts, cache: map[string]string{}}
	return e.document(body, contentType, pageURL, 0)
}

func (e *exporter) document(body []byte, ct, pageURL string, depth int) ([]byte, error) {
	base, err := url.Parse(pageURL)
	if err != nil {
		return nil, err
	}
	p, err := static.ParsePage(body, ct, base, static.ParseOptions{SkipFrames: e.opts.SkipFrames})
	if err != nil {
		return nil, err
	}
	for _, ic := range p.Inline {
		ic.Set(e.css(ic.CSS, p.Base, 0))
	}
	for _, r := range p.Refs {
		switch r.Kind {
		case static.KindScript:
			if e.opts.KeepScripts {
				r.Set(e.dataURI(r.URL, "text/javascript"))
			}
		case static.KindStyle:
			r.Set(e.stylesheet(r.URL, 0))
		case static.KindFrame:
			if depth >= 1 {
				r.Set("about:blank")
				continue
			}
			r.Set(e.frame(r.URL, depth))
		default:
			r.Set(e.dataURI(r.URL, ""))
		}
	}
	clean(p.Root, e.opts)
	if e.opts.Comment != "" && depth == 0 {
		if head := find(p.Root, atom.Head); head != nil {
			head.InsertBefore(&html.Node{Type: html.CommentNode, Data: " " + strings.ReplaceAll(e.opts.Comment, "--", "- -") + " "}, head.FirstChild)
		}
	}
	return p.Render(), nil
}

func (e *exporter) fetch(rawURL string) (body []byte, ct, final string, ok bool) {
	body, ct, final, err := e.f.Fetch(e.ctx, rawURL)
	if err != nil {
		return nil, "", "", false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.total+int64(len(body)) > e.opts.MaxBytes {
		return nil, "", "", false
	}
	e.total += int64(len(body))
	return body, ct, final, true
}

const missing = "data:,"

func (e *exporter) cached(key string, build func() string) string {
	e.mu.Lock()
	v, ok := e.cache[key]
	e.mu.Unlock()
	if ok {
		return v
	}
	v = build()
	e.mu.Lock()
	e.cache[key] = v
	e.mu.Unlock()
	return v
}

func (e *exporter) dataURI(rawURL, fallbackType string) string {
	return e.cached("asset "+rawURL, func() string {
		body, ct, _, ok := e.fetch(rawURL)
		if !ok {
			return missing
		}
		if ct == "" {
			ct = fallbackType
		}
		return DataURI(body, ct)
	})
}

func (e *exporter) stylesheet(rawURL string, depth int) string {
	return e.cached("style "+rawURL, func() string {
		body, _, final, ok := e.fetch(rawURL)
		if !ok {
			return missing
		}
		base, err := url.Parse(final)
		if err != nil {
			return missing
		}
		return DataURI(e.css(body, base, depth+1), "text/css; charset=utf-8")
	})
}

func (e *exporter) css(src []byte, base *url.URL, depth int) []byte {
	return static.RewriteCSS(src, func(raw string, kind static.Kind) (string, bool) {
		if strings.HasPrefix(strings.TrimSpace(raw), "data:") {
			return "", false
		}
		abs := static.Resolve(base, raw)
		if abs == "" {
			return "", false
		}
		if kind == static.KindStyle {
			if depth >= static.DefaultMaxCSSDepth {
				return missing, true
			}
			return e.stylesheet(abs, depth), true
		}
		return e.dataURI(abs, ""), true
	})
}

func (e *exporter) frame(rawURL string, depth int) string {
	return e.cached("frame "+rawURL, func() string {
		body, ct, final, ok := e.fetch(rawURL)
		if !ok || (mediaType(ct) != "" && mediaType(ct) != "text/html") {
			return missing
		}
		out, err := e.document(body, ct, final, depth+1)
		if err != nil {
			return missing
		}
		return DataURI(out, "text/html; charset=utf-8")
	})
}

// DataURI encodes content as a base64 data: URI.
func DataURI(b []byte, contentType string) string {
	ct := contentType
	if mt, params, err := mime.ParseMediaType(contentType); err == nil {
		ct = mt
		if cs, ok := params["charset"]; ok && strings.HasPrefix(mt, "text/") {
			ct += ";charset=" + cs
		}
	}
	if ct == "" {
		ct = http.DetectContentType(b)
		ct, _, _ = strings.Cut(ct, ";")
	}
	return "data:" + ct + ";base64," + base64.StdEncoding.EncodeToString(b)
}

func mediaType(ct string) string {
	mt, _, _ := strings.Cut(ct, ";")
	return strings.ToLower(strings.TrimSpace(mt))
}

func find(n *html.Node, a atom.Atom) *html.Node {
	if n.Type == html.ElementNode && n.DataAtom == a {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if f := find(c, a); f != nil {
			return f
		}
	}
	return nil
}

// clean removes scripts, event handlers, <base> and refresh, unwraps
// <noscript> and declares UTF-8.
func clean(root *html.Node, opts Options) {
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		for c := n.FirstChild; c != nil; {
			next := c.NextSibling
			if c.Type == html.ElementNode {
				switch {
				case c.DataAtom == atom.Script && !opts.KeepScripts,
					c.DataAtom == atom.Base,
					c.DataAtom == atom.Meta && isRefreshOrCharset(c),
					c.DataAtom == atom.Link && isPrefetch(c):
					n.RemoveChild(c)
					c = next
					continue
				case c.DataAtom == atom.Noscript && !opts.KeepScripts:
					for gc := c.FirstChild; gc != nil; {
						gnext := gc.NextSibling
						c.RemoveChild(gc)
						n.InsertBefore(gc, c)
						gc = gnext
					}
					n.RemoveChild(c)
					c = next
					continue
				}
				if !opts.KeepScripts {
					attrs := c.Attr[:0]
					for _, a := range c.Attr {
						if strings.HasPrefix(strings.ToLower(a.Key), "on") ||
							(a.Key == "href" && strings.HasPrefix(strings.ToLower(strings.TrimSpace(a.Val)), "javascript:")) {
							continue
						}
						attrs = append(attrs, a)
					}
					c.Attr = attrs
				}
			}
			walk(c)
			c = next
		}
	}
	walk(root)
	if head := find(root, atom.Head); head != nil {
		meta := &html.Node{Type: html.ElementNode, Data: "meta", DataAtom: atom.Meta, Attr: []html.Attribute{{Key: "charset", Val: "utf-8"}}}
		head.InsertBefore(meta, head.FirstChild)
	}
}

func isRefreshOrCharset(n *html.Node) bool {
	for _, a := range n.Attr {
		k := strings.ToLower(a.Key)
		if k == "charset" {
			return true
		}
		if k == "http-equiv" && (strings.EqualFold(a.Val, "refresh") || strings.EqualFold(a.Val, "content-type")) {
			return true
		}
	}
	return false
}

func isPrefetch(n *html.Node) bool {
	for _, a := range n.Attr {
		if a.Key == "rel" {
			for _, r := range strings.Fields(strings.ToLower(a.Val)) {
				switch r {
				case "preload", "prefetch", "modulepreload", "preconnect", "dns-prefetch":
					return true
				}
			}
		}
	}
	return false
}
