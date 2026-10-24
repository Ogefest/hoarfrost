package static

import (
	"bytes"
	"io"
	"net/url"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
	"golang.org/x/net/html/charset"
)

// Ref is a resource referenced by a page.
type Ref struct {
	URL  string
	Kind Kind
	set  func(string)
}

// Set rewrites the reference in the parsed document (export).
func (r Ref) Set(v string) {
	if r.set != nil {
		r.set(v)
	}
}

// InlineCSS is a <style> element or style attribute.
type InlineCSS struct {
	CSS []byte
	set func([]byte)
}

// Set replaces the inline stylesheet.
func (c InlineCSS) Set(v []byte) { c.set(v) }

// Page is a parsed HTML document with its resource references.
type Page struct {
	Root   *html.Node
	Base   *url.URL
	Refs   []Ref
	Links  []string // attachment links
	Inline []InlineCSS
}

// ParseOptions control which references are collected.
type ParseOptions struct {
	// SkipFrames lists domains whose iframes are not captured.
	SkipFrames []string
}

// DecodeHTML converts body to UTF-8 using the Content-Type and <meta> hints.
func DecodeHTML(body []byte, contentType string) []byte {
	r, err := charset.NewReader(bytes.NewReader(body), contentType)
	if err != nil {
		return body
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return body
	}
	return b
}

// ParsePage parses an HTML document located at pageURL.
func ParsePage(body []byte, contentType string, pageURL *url.URL, opts ParseOptions) (*Page, error) {
	root, err := html.ParseWithOptions(bytes.NewReader(DecodeHTML(body, contentType)), html.ParseOptionEnableScripting(false))
	if err != nil {
		return nil, err
	}
	p := &Page{Root: root, Base: pageURL}
	if b := findBase(root); b != "" {
		if u, err := url.Parse(b); err == nil {
			p.Base = pageURL.ResolveReference(u)
		}
	}
	seenLinks := map[string]bool{}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			p.element(n, opts, seenLinks)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return p, nil
}

func findBase(n *html.Node) string {
	if n.Type == html.ElementNode && n.DataAtom == atom.Base {
		if v, ok := attr(n, "href"); ok {
			return v
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if v := findBase(c); v != "" {
			return v
		}
	}
	return ""
}

func attr(n *html.Node, name string) (string, bool) {
	for _, a := range n.Attr {
		if a.Namespace == "" && a.Key == name {
			return a.Val, true
		}
	}
	return "", false
}

// SetAttr sets or adds an attribute.
func SetAttr(n *html.Node, name, val string) {
	for i, a := range n.Attr {
		if a.Namespace == "" && a.Key == name {
			n.Attr[i].Val = val
			return
		}
	}
	n.Attr = append(n.Attr, html.Attribute{Key: name, Val: val})
}

// DelAttr removes an attribute.
func DelAttr(n *html.Node, name string) {
	out := n.Attr[:0]
	for _, a := range n.Attr {
		if a.Namespace != "" || a.Key != name {
			out = append(out, a)
		}
	}
	n.Attr = out
}

var lazySrc = []string{"data-src", "data-lazy-src", "data-original", "data-lazy", "data-url"}
var lazySrcset = []string{"data-srcset", "data-lazy-srcset"}

func (p *Page) add(raw string, kind Kind, set func(string)) {
	if abs := Resolve(p.Base, raw); abs != "" {
		p.Refs = append(p.Refs, Ref{URL: abs, Kind: kind, set: set})
	}
}

func (p *Page) attrRef(n *html.Node, name string, kind Kind) {
	if v, ok := attr(n, name); ok {
		p.add(v, kind, func(s string) { SetAttr(n, name, s) })
	}
}

// srcsetRef adds the best candidate of a srcset attribute; target is the
// attribute that receives the rewritten single URL.
func (p *Page) srcsetRef(n *html.Node, name, target string) {
	v, ok := attr(n, name)
	if !ok {
		return
	}
	best, ok := Best(ParseSrcset(v))
	if !ok {
		return
	}
	p.add(best.URL, KindAsset, func(s string) {
		if name != target {
			DelAttr(n, name)
		}
		SetAttr(n, target, s)
	})
}

func (p *Page) lazy(n *html.Node) {
	for _, name := range lazySrc {
		if v, ok := attr(n, name); ok && strings.TrimSpace(v) != "" {
			p.add(v, KindAsset, func(s string) {
				DelAttr(n, name)
				SetAttr(n, "src", s)
			})
			break
		}
	}
	for _, name := range lazySrcset {
		if _, ok := attr(n, name); ok {
			p.srcsetRef(n, name, "srcset")
			break
		}
	}
}

func (p *Page) element(n *html.Node, opts ParseOptions, seenLinks map[string]bool) {
	if v, ok := attr(n, "style"); ok && strings.Contains(v, "url") {
		p.Inline = append(p.Inline, InlineCSS{CSS: []byte(v), set: func(b []byte) { SetAttr(n, "style", string(b)) }})
	}
	switch n.DataAtom {
	case atom.Img:
		p.attrRef(n, "src", KindAsset)
		p.srcsetRef(n, "srcset", "srcset")
		p.lazy(n)
	case atom.Source:
		p.srcsetRef(n, "srcset", "srcset")
		p.lazy(n)
		if n.Parent != nil && n.Parent.DataAtom == atom.Picture {
			p.attrRef(n, "src", KindAsset)
		}
	case atom.Video:
		p.attrRef(n, "poster", KindAsset)
	case atom.Input:
		if t, _ := attr(n, "type"); strings.EqualFold(t, "image") {
			p.attrRef(n, "src", KindAsset)
		}
	case atom.Body, atom.Table, atom.Td, atom.Th:
		p.attrRef(n, "background", KindAsset)
	case atom.Image:
		p.attrRef(n, "href", KindAsset)
		for i, a := range n.Attr {
			if a.Key == "href" && a.Namespace != "" {
				i := i
				p.add(a.Val, KindAsset, func(s string) { n.Attr[i].Val = s })
			}
		}
	case atom.Script:
		p.attrRef(n, "src", KindScript)
	case atom.Iframe, atom.Frame:
		src, _ := attr(n, "src")
		if abs := Resolve(p.Base, src); abs != "" {
			if u, _ := url.Parse(abs); u != nil && !HostMatches(u.Hostname(), opts.SkipFrames) {
				p.add(src, KindFrame, func(s string) { SetAttr(n, "src", s) })
			}
		}
	case atom.Link:
		p.link(n)
	case atom.Style:
		if c := n.FirstChild; c != nil && c.Type == html.TextNode {
			p.Inline = append(p.Inline, InlineCSS{CSS: []byte(c.Data), set: func(b []byte) { c.Data = string(b) }})
		}
	case atom.A, atom.Area:
		if v, ok := attr(n, "href"); ok {
			if abs := Resolve(p.Base, v); abs != "" && IsAttachmentURL(abs) && !seenLinks[abs] {
				seenLinks[abs] = true
				p.Links = append(p.Links, abs)
			}
		}
	}
}

func (p *Page) link(n *html.Node) {
	rel, _ := attr(n, "rel")
	rels := strings.Fields(strings.ToLower(rel))
	has := func(want string) bool {
		for _, r := range rels {
			if r == want {
				return true
			}
		}
		return false
	}
	switch {
	case has("stylesheet"):
		p.attrRef(n, "href", KindStyle)
	case has("icon") || has("apple-touch-icon") || has("apple-touch-icon-precomposed") || has("mask-icon"):
		p.attrRef(n, "href", KindAsset)
		p.srcsetRef(n, "imagesrcset", "imagesrcset")
	case has("preload") || has("modulepreload"):
		as, _ := attr(n, "as")
		switch strings.ToLower(as) {
		case "style":
			p.attrRef(n, "href", KindStyle)
		case "font", "image":
			p.attrRef(n, "href", KindAsset)
			p.srcsetRef(n, "imagesrcset", "imagesrcset")
		case "script":
			p.attrRef(n, "href", KindScript)
		}
		if has("modulepreload") {
			p.attrRef(n, "href", KindScript)
		}
	}
}

// Render serializes the (possibly rewritten) document.
func (p *Page) Render() []byte {
	var b bytes.Buffer
	html.Render(&b, p.Root)
	return b.Bytes()
}

// Title returns the document <title>.
func (p *Page) Title() string {
	var find func(*html.Node) string
	find = func(n *html.Node) string {
		if n.Type == html.ElementNode && n.DataAtom == atom.Title && n.FirstChild != nil {
			return strings.TrimSpace(n.FirstChild.Data)
		}
		if n.Type == html.ElementNode && n.DataAtom == atom.Svg {
			return ""
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if t := find(c); t != "" {
				return t
			}
		}
		return ""
	}
	return find(p.Root)
}
