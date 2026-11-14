// Package monolith imports pages saved by monolith (single HTML files with
// every image, stylesheet and font inlined as a base64 data: URI). Each page
// is split into the HTML and its inlined files; the files get
// content-addressed URLs, so the logos, stylesheets, fonts and thumbnails
// repeated on thousands of pages are stored once per week.
package monolith

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"regexp"
	"strings"
)

// Host is the reserved (RFC 2606) host of the URLs given to inlined files.
// They never existed on the web: monolith does not keep original URLs.
const Host = "monolith.invalid"

// DefaultMinSize is the smallest decoded payload moved out of a page;
// smaller data: URIs stay inline, where they cost less than a record.
const DefaultMinSize = 1024

// maxDepth bounds nesting (a font in a stylesheet in a frame…).
const maxDepth = 4

// Resource is a file moved out of a page.
type Resource struct {
	URL         string
	ContentType string
	Body        []byte
}

// dataURI matches a base64 data: URI. The media type and parameters are
// optional; the payload ends at the first character outside the base64
// alphabet (a quote, parenthesis, whitespace or '>').
var dataURI = regexp.MustCompile(`data:([A-Za-z0-9.+-]+/[A-Za-z0-9.+-]+)?((?:;[A-Za-z0-9._+-]+=[^;,"'()\s>]*)*);(?i:base64),([A-Za-z0-9+/]+=*)`)

// Split replaces base64 data: URIs of at least minSize decoded bytes with
// resource URLs and returns the rewritten document and the files, each once
// (in order of first use). Stylesheets, HTML frames and SVG images are split
// recursively before they are hashed.
func Split(doc []byte, minSize int) ([]byte, []Resource) {
	s := splitter{min: minSize, seen: map[string]bool{}}
	out := s.rewrite(doc, 0)
	return out, s.res
}

type splitter struct {
	min  int
	seen map[string]bool
	res  []Resource
}

func (s *splitter) rewrite(doc []byte, depth int) []byte {
	return dataURI.ReplaceAllFunc(doc, func(m []byte) []byte {
		sub := dataURI.FindSubmatch(m)
		payload := sub[3]
		if base64.StdEncoding.DecodedLen(len(payload)) < s.min {
			return m
		}
		body, err := decode(payload)
		if err != nil || len(body) < s.min {
			return m
		}
		mt := strings.ToLower(string(sub[1]))
		if depth < maxDepth && nested(mt) {
			body = s.rewrite(body, depth+1)
		}
		ct := contentType(mt, string(sub[2]))
		u := ResourceURL(body, mt)
		if !s.seen[u] {
			s.seen[u] = true
			s.res = append(s.res, Resource{URL: u, ContentType: ct, Body: body})
		}
		return []byte(u)
	})
}

func decode(p []byte) ([]byte, error) {
	trimmed := strings.TrimRight(string(p), "=")
	return base64.RawStdEncoding.DecodeString(trimmed)
}

func nested(mt string) bool {
	return mt == "text/css" || mt == "text/html" || mt == "image/svg+xml"
}

func contentType(mt, params string) string {
	if mt == "" {
		return "application/octet-stream"
	}
	return mt + params
}

// ResourceURL returns the content-addressed URL of a file: the SHA-256 of
// its bytes with an extension for common media types. The table is fixed
// (not the system MIME database), so the URL is the same on every machine.
func ResourceURL(body []byte, mediaType string) string {
	sum := sha256.Sum256(body)
	return "https://" + Host + "/sha256/" + hex.EncodeToString(sum[:]) + extension(mediaType)
}

var knownExt = map[string]string{
	"image/jpeg": ".jpg", "image/jpg": ".jpg", "image/png": ".png", "image/gif": ".gif",
	"image/webp": ".webp", "image/avif": ".avif", "image/svg+xml": ".svg", "image/x-icon": ".ico",
	"image/vnd.microsoft.icon": ".ico", "text/css": ".css", "text/html": ".html",
	"font/woff2": ".woff2", "font/woff": ".woff", "font/ttf": ".ttf", "font/otf": ".otf",
	"application/font-woff": ".woff", "application/font-woff2": ".woff2",
	"application/x-font-woff": ".woff", "application/vnd.ms-fontobject": ".eot",
}

func extension(mt string) string {
	return knownExt[mt]
}
