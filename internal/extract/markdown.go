package extract

import (
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// lineBreak marks a <br> inside inline text until the paragraph is collapsed.
const lineBreak = '\x00'

// markdownText renders the text of an HTML tree as lightweight Markdown:
// paragraphs separated by blank lines, "#" headings, "-" and "1." list
// items, "> " quotes, fenced code blocks and "|"-separated table rows. Inline
// markup is dropped; only the text and its block structure are kept.
func markdownText(n *html.Node) string {
	blocks := renderBlocks(n)
	lines := strings.Split(strings.ToValidUTF8(strings.Join(blocks, "\n\n"), ""), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	s := strings.Join(lines, "\n")
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(s)
}

type mdRenderer struct {
	blocks []string
	inline strings.Builder
}

// renderBlocks returns the Markdown blocks of n's children.
func renderBlocks(n *html.Node) []string {
	var r mdRenderer
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		r.node(c)
	}
	r.flush()
	return r.blocks
}

// inlineText returns n's text as a single line (line breaks kept for <br>).
func inlineText(n *html.Node) string {
	return strings.Join(renderBlocks(n), " ")
}

func (r *mdRenderer) flush() {
	if t := collapseInline(r.inline.String()); t != "" {
		r.blocks = append(r.blocks, t)
	}
	r.inline.Reset()
}

func (r *mdRenderer) block(s string) {
	r.flush()
	if strings.TrimSpace(s) != "" {
		r.blocks = append(r.blocks, s)
	}
}

func (r *mdRenderer) node(n *html.Node) {
	switch n.Type {
	case html.TextNode:
		r.inline.WriteString(n.Data)
		return
	case html.ElementNode:
	default:
		if n.Type == html.DocumentNode {
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				r.node(c)
			}
		}
		return
	}
	switch tag := n.Data; tag {
	case "script", "style", "noscript", "template", "svg", "head", "title", "img", "iframe", "button", "select", "input", "textarea":
	case "br":
		r.inline.WriteRune(lineBreak)
	case "h1", "h2", "h3", "h4", "h5", "h6":
		if t := inlineText(n); t != "" {
			level := int(tag[1] - '0')
			r.block(strings.Repeat("#", level) + " " + strings.ReplaceAll(t, "\n", " "))
		}
	case "hr":
		r.block("---")
	case "ul", "ol", "menu":
		r.block(renderList(n, tag == "ol"))
	case "blockquote":
		r.block(prefixLines(strings.Join(renderBlocks(n), "\n\n"), "> ", ">"))
	case "pre":
		r.block(codeBlock(n))
	case "code":
		if strings.Contains(textContent(n), "\n") {
			r.block(codeBlock(n))
		} else {
			r.inline.WriteString(textContent(n))
		}
	case "table":
		r.block(renderTable(n))
	default:
		if !isBlock(tag) {
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				r.node(c)
			}
			return
		}
		r.flush()
		r.blocks = append(r.blocks, renderBlocks(n)...)
	}
}

func isBlock(tag string) bool {
	switch tag {
	case "p", "div", "li", "article", "section", "main", "header", "footer", "aside", "nav",
		"figure", "figcaption", "details", "summary", "dl", "dt", "dd", "address", "form",
		"fieldset", "center", "body", "html", "tr", "td", "th", "caption":
		return true
	}
	return false
}

// collapseInline collapses whitespace runs to single spaces and turns <br>
// markers into newlines.
func collapseInline(s string) string {
	parts := strings.Split(s, string(lineBreak))
	for i, p := range parts {
		parts[i] = strings.Join(strings.Fields(p), " ")
	}
	return strings.Trim(strings.Join(parts, "\n"), "\n")
}

func textContent(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		if n.Type == html.ElementNode && n.Data == "br" {
			b.WriteByte('\n')
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

func codeBlock(n *html.Node) string {
	code := strings.Trim(textContent(n), "\n")
	if strings.TrimSpace(code) == "" {
		return ""
	}
	return "```\n" + code + "\n```"
}

func renderList(n *html.Node, ordered bool) string {
	var items []string
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		var body string
		switch {
		case c.Type == html.ElementNode && c.Data == "li":
			body = strings.Join(renderBlocks(c), "\n")
		case c.Type == html.ElementNode:
			var r mdRenderer
			r.node(c)
			r.flush()
			body = strings.Join(r.blocks, "\n")
		}
		if strings.TrimSpace(body) == "" {
			continue
		}
		marker := "- "
		if ordered {
			marker = strconv.Itoa(len(items)+1) + ". "
		}
		items = append(items, marker+prefixLines(body, strings.Repeat(" ", len(marker)), "")[len(marker):])
	}
	return strings.Join(items, "\n")
}

func renderTable(n *html.Node) string {
	var rows []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type != html.ElementNode {
				continue
			}
			switch c.Data {
			case "tr":
				var cells []string
				for td := c.FirstChild; td != nil; td = td.NextSibling {
					if td.Type == html.ElementNode && (td.Data == "td" || td.Data == "th") {
						cells = append(cells, strings.ReplaceAll(inlineText(td), "\n", " "))
					}
				}
				if strings.TrimSpace(strings.Join(cells, "")) != "" {
					rows = append(rows, strings.Join(cells, " | "))
				}
			case "caption":
				if t := inlineText(c); t != "" {
					rows = append(rows, t)
				}
			case "table":
			default:
				walk(c)
			}
		}
	}
	walk(n)
	return strings.Join(rows, "\n")
}

// prefixLines prefixes every line of s; blank lines get blankPrefix.
func prefixLines(s, prefix, blankPrefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			lines[i] = blankPrefix
		} else {
			lines[i] = prefix + l
		}
	}
	return strings.Join(lines, "\n")
}
