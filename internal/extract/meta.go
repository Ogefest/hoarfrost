package extract

import (
	"encoding/json"
	"net/url"
	"sort"
	"strings"

	"golang.org/x/net/html"
	"willnorris.com/go/microformats"
)

// Limits on Open Graph and microformats data kept per page.
const (
	maxMetaValue       = 2000 // runes of one value
	maxOpenGraphValues = 100
	maxMicroformats    = 32
	maxMicroBytes      = 64 << 10
	maxKeywords        = 50
	maxKeyword         = 100
)

// openGraphPrefixes are the <meta property|name> namespaces kept: Open Graph
// (og:) with its object types, and Twitter Cards.
var openGraphPrefixes = []string{"og:", "article:", "book:", "profile:", "music:", "video:", "twitter:"}

func isOpenGraph(key string) bool {
	for _, p := range openGraphPrefixes {
		if strings.HasPrefix(key, p) && len(key) > len(p) {
			return true
		}
	}
	return false
}

// openGraph collects Open Graph and Twitter Card properties. A property that
// occurs once is a string; a repeated one (og:image, article:tag) is a list
// in document order.
type openGraph struct {
	values map[string][]string
	n      int
}

func (g *openGraph) add(key, value string) {
	value = truncateRunes(normalizeSpace(value), maxMetaValue)
	if value == "" || g.n >= maxOpenGraphValues {
		return
	}
	if g.values == nil {
		g.values = map[string][]string{}
	}
	g.values[key] = append(g.values[key], value)
	g.n++
}

func (g *openGraph) result() map[string]any {
	if len(g.values) == 0 {
		return nil
	}
	out := make(map[string]any, len(g.values))
	for k, v := range g.values {
		if len(v) == 1 {
			out[k] = v[0]
		} else {
			out[k] = v
		}
	}
	return out
}

// OpenGraphValues returns the values of an Open Graph property as a list.
func OpenGraphValues(og map[string]any, key string) []string {
	switch v := og[key].(type) {
	case string:
		return []string{v}
	case []string:
		return v
	case []any:
		var out []string
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// microformats2 returns the top-level microformats2 items of doc in the
// JSON form of the mf2 parsing specification. Embedded HTML ("html" of
// e-* properties) is dropped, the page itself is archived, and long values
// are shortened.
func microformats2(doc *html.Node, pageURL string) []map[string]any {
	base, _ := url.Parse(pageURL)
	data := microformats.ParseNode(doc, base)
	if data == nil {
		return nil
	}
	var out []map[string]any
	size := 0
	for _, it := range data.Items {
		if len(out) >= maxMicroformats {
			break
		}
		b, err := json.Marshal(it)
		if err != nil {
			continue
		}
		var m map[string]any
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		trimMicroformat(m)
		b, _ = json.Marshal(m)
		if size+len(b) > maxMicroBytes {
			continue
		}
		size += len(b)
		out = append(out, m)
	}
	return out
}

func trimMicroformat(v any) any {
	switch x := v.(type) {
	case map[string]any:
		delete(x, "html")
		for k, y := range x {
			x[k] = trimMicroformat(y)
		}
	case []any:
		for i, y := range x {
			x[i] = trimMicroformat(y)
		}
	case string:
		return truncateRunes(x, maxMetaValue)
	}
	return v
}

// MicroformatTypes returns the distinct types of microformats items
// (h-entry, h-card…), also of nested ones, sorted.
func MicroformatTypes(items []map[string]any) []string {
	seen := map[string]bool{}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if ts, ok := x["type"].([]any); ok {
				for _, t := range ts {
					if s, ok := t.(string); ok {
						seen[s] = true
					}
				}
			}
			for _, y := range x {
				walk(y)
			}
		case []any:
			for _, y := range x {
				walk(y)
			}
		}
	}
	for _, it := range items {
		walk(it)
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// mfStrings returns the string values of a property of an mf2 item; for
// nested items (an h-card author) their "name" or "value".
func mfStrings(item map[string]any, prop string) []string {
	props, _ := item["properties"].(map[string]any)
	values, _ := props[prop].([]any)
	var out []string
	for _, v := range values {
		switch x := v.(type) {
		case string:
			out = append(out, x)
		case map[string]any:
			if names := mfStrings(x, "name"); len(names) > 0 {
				out = append(out, names[0])
			} else if s, ok := x["value"].(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// entries returns the h-entry items (posts, articles), also nested in an
// h-feed or another item's children.
func entries(items []map[string]any) []map[string]any {
	var out []map[string]any
	var walk func(map[string]any)
	walk = func(m map[string]any) {
		if ts, ok := m["type"].([]any); ok {
			for _, t := range ts {
				if t == "h-entry" {
					out = append(out, m)
					break
				}
			}
		}
		if cs, ok := m["children"].([]any); ok {
			for _, c := range cs {
				if cm, ok := c.(map[string]any); ok {
					walk(cm)
				}
			}
		}
	}
	for _, it := range items {
		walk(it)
	}
	return out
}

// Keywords returns the tags of a page: schema.org keywords and
// articleSection, Open Graph article:tag, microformats p-category and
// <meta name="keywords">, without duplicates (ignoring case), in that order.
func Keywords(schemaOrg []map[string]any, og map[string]any, mf []map[string]any, metaKeywords string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		s = truncateRunes(normalizeSpace(s), maxKeyword)
		k := strings.ToLower(s)
		if s == "" || seen[k] || len(out) >= maxKeywords || strings.HasPrefix(k, "http://") || strings.HasPrefix(k, "https://") {
			return
		}
		seen[k] = true
		out = append(out, s)
	}
	addList := func(s string) {
		for _, w := range strings.Split(s, ",") {
			add(w)
		}
	}
	for _, o := range schemaOrg {
		for _, key := range []string{"keywords", "articleSection"} {
			switch v := o[key].(type) {
			case string:
				addList(v)
			case []any:
				for _, x := range v {
					if s, ok := x.(string); ok {
						add(s)
					}
				}
			}
		}
	}
	for _, t := range OpenGraphValues(og, "article:tag") {
		addList(t)
	}
	for _, e := range entries(mf) {
		for _, c := range mfStrings(e, "category") {
			add(c)
		}
	}
	addList(metaKeywords)
	return out
}

func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// Item is a schema.org object or a microformats2 item of a page, nested
// ones included, as stored in the index table page_items.
type Item struct {
	Format   string // FormatSchemaOrg or FormatMicroformats
	Parent   int    // index of the enclosing item in the list, -1 for top-level items
	Property string // property of the parent holding the item ("author", "children")
	Types    []string
	Data     map[string]any
}

// Item formats.
const (
	FormatSchemaOrg    = "schema.org"
	FormatMicroformats = "microformats2"
)

const (
	maxItemsPerPage = 200
	maxItemDepth    = 6
)

// Items flattens the structured data of a page into items in document
// order (parents before their nested items).
func Items(schemaOrg []map[string]any, mf []map[string]any) []Item {
	var out []Item
	var walk func(format string, m map[string]any, parent int, property string, depth int)
	add := func(format string, v any, parent int, property string, depth int) {
		switch x := v.(type) {
		case map[string]any:
			walk(format, x, parent, property, depth)
		case []any:
			for _, y := range x {
				if m, ok := y.(map[string]any); ok {
					walk(format, m, parent, property, depth)
				}
			}
		}
	}
	walk = func(format string, m map[string]any, parent int, property string, depth int) {
		var types []string
		if format == FormatSchemaOrg {
			types = typesOf(m["@type"])
		} else {
			types = mfTypes(m)
		}
		if len(types) == 0 || depth > maxItemDepth || len(out) >= maxItemsPerPage {
			return
		}
		out = append(out, Item{Format: format, Parent: parent, Property: property, Types: types, Data: m})
		self := len(out) - 1
		if format == FormatSchemaOrg {
			for _, k := range sortedKeys(m) {
				add(format, m[k], self, k, depth+1)
			}
			return
		}
		props, _ := m["properties"].(map[string]any)
		for _, k := range sortedKeys(props) {
			add(format, props[k], self, k, depth+1)
		}
		add(format, m["children"], self, "children", depth+1)
	}
	for _, o := range schemaOrg {
		walk(FormatSchemaOrg, o, -1, "", 0)
	}
	for _, it := range mf {
		walk(FormatMicroformats, it, -1, "", 0)
	}
	return out
}

func mfTypes(m map[string]any) []string {
	var out []string
	switch t := m["type"].(type) {
	case []any:
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	case []string:
		out = append(out, t...)
	}
	return out
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
