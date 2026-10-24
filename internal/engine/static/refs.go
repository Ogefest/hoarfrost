// Package static captures server-rendered pages without JavaScript: it parses
// HTML and CSS, resolves every resource a browser would need and fetches it
// through the recorder. The same parser rewrites pages for export.
package static

import (
	"net/url"
	"path"
	"strings"
)

// Kind classifies a resource reference.
type Kind int

const (
	KindAsset      Kind = iota // image, font, media poster, icon
	KindStyle                  // stylesheet
	KindScript                 // script
	KindFrame                  // iframe document
	KindAttachment             // linked document such as a PDF
)

func (k Kind) String() string {
	return [...]string{"asset", "style", "script", "frame", "attachment"}[k]
}

// Resolve resolves ref against base and returns a fetchable absolute URL.
// It returns "" for empty, fragment-only, data:, javascript: and similar refs.
func Resolve(base *url.URL, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.HasPrefix(ref, "#") {
		return ""
	}
	u, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	if base != nil {
		u = base.ResolveReference(u)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}

var attachmentExt = map[string]bool{
	".pdf": true, ".doc": true, ".docx": true, ".xls": true, ".xlsx": true,
	".odt": true, ".ods": true, ".odp": true, ".ppt": true, ".pptx": true,
	".rtf": true, ".epub": true,
}

// IsAttachmentURL reports whether a link points at a document worth
// capturing separately with the http engine.
func IsAttachmentURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return attachmentExt[strings.ToLower(path.Ext(u.Path))]
}

// HostMatches reports whether host equals one of domains or is a subdomain.
func HostMatches(host string, domains []string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, d := range domains {
		d = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(d)), ".")
		if d != "" && (host == d || strings.HasSuffix(host, "."+d)) {
			return true
		}
	}
	return false
}
