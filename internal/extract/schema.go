package extract

import (
	"encoding/json"
	"sort"
	"strings"

	"golang.org/x/net/html"

	"github.com/ogefest/hoarfrost/internal/recorder"
)

// Limits on structured data kept per page; some sites embed very large
// JSON-LD blocks (whole catalogues, breadcrumbs of every section).
const (
	maxSchemaObjects = 32
	maxSchemaBytes   = 64 << 10
	maxDescription   = 2000
)

// Structured is the page description and schema.org data found in HTML.
type Structured struct {
	Description string
	// SchemaOrg holds top-level schema.org objects, each with "@type",
	// from JSON-LD, microdata and RDFa, in document order.
	SchemaOrg []map[string]any
}

// Fill copies the description, the schema.org objects and the people named
// in them into the metadata of a page.
func (s Structured) Fill(p *recorder.PageInfo) {
	p.Description = s.Description
	p.SchemaOrg = s.SchemaOrg
	p.Authors = People(s.SchemaOrg)
}

// Types returns the distinct schema.org types of objects, without the
// vocabulary prefix, sorted.
func Types(objects []map[string]any) []string {
	seen := map[string]bool{}
	for _, o := range objects {
		for _, t := range typesOf(o["@type"]) {
			seen[t] = true
		}
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func typesOf(v any) []string {
	var out []string
	switch t := v.(type) {
	case string:
		for _, f := range strings.Fields(t) {
			out = append(out, shortType(f))
		}
	case []any:
		for _, x := range t {
			out = append(out, typesOf(x)...)
		}
	}
	return out
}

func shortType(t string) string {
	for _, p := range []string{"https://schema.org/", "http://schema.org/", "schema:"} {
		t = strings.TrimPrefix(t, p)
	}
	return t
}

// People returns the names of authors, creators, contributors and editors
// of the objects (also nested ones), without duplicates, in order.
func People(objects []map[string]any) []string {
	var out []string
	seen := map[string]bool{}
	var visit func(v any, depth int)
	add := func(v any) {
		var names []string
		var collect func(any)
		collect = func(x any) {
			switch p := x.(type) {
			case string:
				names = append(names, p)
			case map[string]any:
				if n, ok := p["name"].(string); ok {
					names = append(names, n)
				} else if n, ok := p["name"].([]any); ok && len(n) > 0 {
					collect(n[0])
				}
			case []any:
				for _, y := range p {
					collect(y)
				}
			}
		}
		collect(v)
		for _, n := range names {
			n = normalizeSpace(n)
			if n != "" && !seen[n] && !strings.HasPrefix(n, "http") {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	visit = func(v any, depth int) {
		if depth > 6 {
			return
		}
		switch o := v.(type) {
		case map[string]any:
			for _, k := range []string{"author", "creator", "contributor", "editor"} {
				if p, ok := o[k]; ok {
					add(p)
				}
			}
			for k, x := range o {
				if k != "author" && k != "creator" && k != "contributor" && k != "editor" {
					visit(x, depth+1)
				}
			}
		case []any:
			for _, x := range o {
				visit(x, depth+1)
			}
		}
	}
	for _, o := range objects {
		visit(o, 0)
	}
	return out
}

// DatePublished returns the first datePublished of the objects.
func DatePublished(objects []map[string]any) string {
	for _, o := range objects {
		if d, ok := o["datePublished"].(string); ok && d != "" {
			return d
		}
	}
	return ""
}

// StructuredData extracts meta description and schema.org objects.
func StructuredData(body []byte, contentType string) Structured {
	doc, err := html.Parse(strings.NewReader(decodeText(body, contentType)))
	if err != nil {
		return Structured{}
	}
	var s Structured
	var ogDescription string
	var objects []map[string]any
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "meta":
				name := strings.ToLower(attr(n, "name"))
				prop := strings.ToLower(attr(n, "property"))
				switch {
				case name == "description" && s.Description == "":
					s.Description = attr(n, "content")
				case prop == "og:description" && ogDescription == "":
					ogDescription = attr(n, "content")
				}
			case "script":
				if strings.EqualFold(strings.TrimSpace(strings.Split(attr(n, "type"), ";")[0]), "application/ld+json") && n.FirstChild != nil {
					objects = append(objects, jsonLD(n.FirstChild.Data)...)
				}
				return
			}
			if isItem(n) && !isProperty(n) {
				objects = append(objects, item(n, 0))
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if s.Description == "" {
		s.Description = ogDescription
	}
	s.Description = normalizeSpace(s.Description)
	if r := []rune(s.Description); len(r) > maxDescription {
		s.Description = string(r[:maxDescription])
	}
	size := 0
	for _, o := range objects {
		if len(typesOf(o["@type"])) == 0 || len(s.SchemaOrg) >= maxSchemaObjects {
			continue
		}
		b, _ := json.Marshal(o)
		if size+len(b) > maxSchemaBytes {
			continue
		}
		size += len(b)
		s.SchemaOrg = append(s.SchemaOrg, o)
	}
	return s
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func hasAttr(n *html.Node, key string) bool {
	for _, a := range n.Attr {
		if a.Key == key {
			return true
		}
	}
	return false
}

// jsonLD parses one JSON-LD block into top-level objects: arrays and
// "@graph" lists are flattened, the context is dropped.
func jsonLD(src string) []map[string]any {
	var v any
	if err := json.Unmarshal([]byte(strings.TrimSpace(src)), &v); err != nil {
		return nil
	}
	var out []map[string]any
	var add func(any)
	add = func(x any) {
		switch o := x.(type) {
		case []any:
			for _, y := range o {
				add(y)
			}
		case map[string]any:
			if ctx, ok := o["@context"]; ok && !schemaContext(ctx) {
				return
			}
			if g, ok := o["@graph"]; ok {
				add(g)
				if _, typed := o["@type"]; !typed {
					return
				}
			}
			delete(o, "@context")
			delete(o, "@graph")
			out = append(out, o)
		}
	}
	add(v)
	return out
}

func schemaContext(ctx any) bool {
	switch c := ctx.(type) {
	case string:
		return strings.Contains(c, "schema.org")
	case map[string]any:
		if v, ok := c["@vocab"].(string); ok {
			return strings.Contains(v, "schema.org")
		}
		return true
	case []any:
		for _, x := range c {
			if schemaContext(x) {
				return true
			}
		}
	}
	return false
}

// Microdata (itemscope/itemtype/itemprop) and RDFa Lite (typeof/property)
// share one walker.
func isItem(n *html.Node) bool {
	if hasAttr(n, "itemscope") {
		return strings.Contains(attr(n, "itemtype"), "schema.org")
	}
	return attr(n, "typeof") != "" && (strings.Contains(attr(n, "vocab"), "schema.org") || strings.Contains(attr(n, "typeof"), "schema"))
}

func isProperty(n *html.Node) bool { return attr(n, "itemprop") != "" || attr(n, "property") != "" }

func properties(n *html.Node) []string {
	p := attr(n, "itemprop")
	if p == "" {
		p = attr(n, "property")
	}
	var out []string
	for _, f := range strings.Fields(p) {
		if !strings.Contains(f, ":") || strings.HasPrefix(f, "schema:") || strings.Contains(f, "schema.org/") {
			out = append(out, shortType(f))
		}
	}
	return out
}

func item(n *html.Node, depth int) map[string]any {
	t := attr(n, "itemtype")
	if t == "" {
		t = attr(n, "typeof")
	}
	o := map[string]any{"@type": shortTypes(t)}
	if id := attr(n, "itemid"); id != "" {
		o["@id"] = id
	}
	var walk func(c *html.Node)
	walk = func(c *html.Node) {
		for ; c != nil; c = c.NextSibling {
			if c.Type != html.ElementNode {
				continue
			}
			props := properties(c)
			if len(props) > 0 {
				var v any
				if isItem(c) && depth < 6 {
					v = item(c, depth+1)
				} else {
					v = propertyValue(c)
				}
				for _, p := range props {
					addProp(o, p, v)
				}
				if isItem(c) {
					continue
				}
			} else if isItem(c) {
				continue
			}
			walk(c.FirstChild)
		}
	}
	walk(n.FirstChild)
	return o
}

func shortTypes(t string) any {
	f := strings.Fields(t)
	if len(f) == 1 {
		return shortType(f[0])
	}
	out := make([]any, len(f))
	for i, x := range f {
		out[i] = shortType(x)
	}
	return out
}

func addProp(o map[string]any, p string, v any) {
	switch cur := o[p].(type) {
	case nil:
		o[p] = v
	case []any:
		o[p] = append(cur, v)
	default:
		o[p] = []any{cur, v}
	}
}

func propertyValue(n *html.Node) string {
	if v := attr(n, "content"); v != "" {
		return v
	}
	switch n.Data {
	case "a", "link", "area":
		return attr(n, "href")
	case "img", "audio", "video", "source", "embed", "iframe":
		return attr(n, "src")
	case "time":
		if v := attr(n, "datetime"); v != "" {
			return v
		}
	case "meta":
		return attr(n, "content")
	case "data", "meter":
		return attr(n, "value")
	}
	var b strings.Builder
	var text func(*html.Node)
	text = func(c *html.Node) {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
			b.WriteByte(' ')
		}
		for x := c.FirstChild; x != nil; x = x.NextSibling {
			text(x)
		}
	}
	text(n)
	return normalizeSpace(b.String())
}
