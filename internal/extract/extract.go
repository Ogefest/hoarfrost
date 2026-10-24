// Package extract derives searchable text and basic metadata from captured
// documents: HTML articles, PDF, office documents and plain text.
package extract

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"mime"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/markusmobius/go-trafilatura"
	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

// Result is the extracted content of a document.
type Result struct {
	Title     string
	Text      string
	Published string // YYYY-MM-DD when known
	Language  string
	Structured
}

// ErrUnsupported is returned for content types without an extractor.
var ErrUnsupported = errors.New("extract: unsupported content type")

// Document extracts text from body according to its content type.
func Document(ctx context.Context, body []byte, contentType, pageURL string) (Result, error) {
	mt, _, _ := mime.ParseMediaType(contentType)
	switch {
	case mt == "text/html" || mt == "application/xhtml+xml" || (mt == "" && looksHTML(body)):
		return HTML(body, contentType, pageURL), nil
	case mt == "application/pdf":
		text, err := PDF(ctx, body)
		return Result{Title: firstLine(text), Text: text}, err
	case mt == "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return zipXMLText(body, "word/document.xml", "p")
	case mt == "application/vnd.oasis.opendocument.text":
		return zipXMLText(body, "content.xml", "p", "h")
	case strings.HasPrefix(mt, "text/"):
		text := normalizeSpace(decodeText(body, contentType))
		return Result{Title: firstLine(text), Text: text}, nil
	}
	return Result{}, ErrUnsupported
}

func looksHTML(b []byte) bool {
	head := strings.ToLower(string(b[:min(len(b), 512)]))
	return strings.Contains(head, "<html") || strings.Contains(head, "<!doctype html")
}

func decodeText(body []byte, contentType string) string {
	r, err := charset.NewReader(bytes.NewReader(body), contentType)
	if err != nil {
		return strings.ToValidUTF8(string(body), "")
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return strings.ToValidUTF8(string(body), "")
	}
	return string(b)
}

// HTML extracts the main article text, title and publication date.
func HTML(body []byte, contentType, pageURL string) Result {
	src := decodeText(body, contentType)
	opts := trafilatura.Options{
		EnableFallback:  true,
		ExcludeComments: true,
		Deduplicate:     true,
	}
	if u, err := url.Parse(pageURL); err == nil && u.Host != "" {
		opts.OriginalURL = u
	}
	res := Result{Structured: StructuredData([]byte(src), "text/html; charset=utf-8", pageURL)}
	if out, err := trafilatura.Extract(strings.NewReader(src), opts); err == nil && out != nil {
		res.Title = strings.TrimSpace(out.Metadata.Title)
		res.Text = normalizeSpace(out.ContentText)
		res.Language = out.Metadata.Language
		if !out.Metadata.Date.IsZero() && out.Metadata.Date.Year() > 1990 {
			res.Published = out.Metadata.Date.Format(time.DateOnly)
		}
	}
	if res.Title == "" || res.Text == "" {
		title, text := plainHTML(src)
		if res.Title == "" {
			res.Title = title
		}
		if res.Text == "" {
			res.Text = text
		}
	}
	return res
}

// plainHTML returns the <title> and all visible text of a document.
func plainHTML(src string) (title, text string) {
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		return "", ""
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "noscript", "template", "svg":
				return
			case "title":
				if title == "" && n.FirstChild != nil {
					title = strings.TrimSpace(n.FirstChild.Data)
				}
				return
			}
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
			b.WriteByte(' ')
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode && isBlock(n.Data) {
			b.WriteByte('\n')
		}
	}
	walk(doc)
	return title, normalizeSpace(b.String())
}

func isBlock(tag string) bool {
	switch tag {
	case "p", "div", "br", "li", "h1", "h2", "h3", "h4", "h5", "h6", "tr", "article", "section", "blockquote":
		return true
	}
	return false
}

var (
	spaceRun = regexp.MustCompile(`[ \t\r\f\v\x{00a0}]+`)
	lineRun  = regexp.MustCompile(`\n\s*\n\s*(\n\s*)+`)
)

func normalizeSpace(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = spaceRun.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	s = strings.Join(lines, "\n")
	s = lineRun.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	line = strings.TrimSpace(line)
	if utf8.RuneCountInString(line) > 200 {
		line = string([]rune(line)[:200])
	}
	return line
}

// ErrToolMissing is returned when an optional external tool is not installed.
var ErrToolMissing = errors.New("extract: external tool not installed")

// PDF extracts text with pdftotext (poppler), if available.
func PDF(ctx context.Context, body []byte) (string, error) {
	bin, err := exec.LookPath("pdftotext")
	if err != nil {
		return "", ErrToolMissing
	}
	cmd := exec.CommandContext(ctx, bin, "-enc", "UTF-8", "-", "-")
	cmd.Stdin = bytes.NewReader(body)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return normalizeSpace(string(out)), nil
}

func zipXMLText(body []byte, member string, paragraphTags ...string) (Result, error) {
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return Result{}, err
	}
	f, err := zr.Open(member)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	dec := xml.NewDecoder(io.LimitReader(f, 64<<20))
	var b strings.Builder
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Result{}, err
		}
		switch t := tok.(type) {
		case xml.CharData:
			b.Write(t)
		case xml.EndElement:
			for _, p := range paragraphTags {
				if t.Name.Local == p {
					b.WriteByte('\n')
				}
			}
		}
	}
	text := normalizeSpace(b.String())
	return Result{Title: firstLine(text), Text: text}, nil
}
