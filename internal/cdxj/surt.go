// Package cdxj implements SURT canonicalization and CDXJ index lines.
package cdxj

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

var wwwPrefix = regexp.MustCompile(`^www\d*\.`)

// SURT returns the Sort-friendly URI Reordering Transform of rawURL in the
// form used by pywb and warcio: "com,example)/path?a=1&b=2".
func SURT(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(rawURL)), " ", "%20")
	}
	host := strings.ToLower(u.Hostname())
	host = strings.TrimSuffix(host, ".")
	port := u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	if net.ParseIP(host) == nil {
		host = wwwPrefix.ReplaceAllString(host, "")
	}
	if !strings.Contains(host, ":") {
		parts := strings.Split(host, ".")
		for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
			parts[i], parts[j] = parts[j], parts[i]
		}
		host = strings.Join(parts, ",")
	}
	if port != "" {
		host += ":" + port
	}

	p := strings.ToLower(minimalEscape(normalizePath(unescapeRepeatedly(u.EscapedPath()))))
	if len(p) > 1 {
		p = strings.TrimSuffix(p, "/")
	}

	var b strings.Builder
	b.WriteString(host)
	b.WriteString(")")
	b.WriteString(p)
	if q := minimalEscape(unescapeRepeatedly(u.RawQuery)); q != "" {
		params := strings.Split(strings.ToLower(q), "&")
		sort.SliceStable(params, func(i, j int) bool { return paramLess(params[i], params[j]) })
		b.WriteString("?")
		b.WriteString(strings.Join(params, "&"))
	}
	return b.String()
}

// paramLess orders query parameters as (key, value) pairs, like Python
// sorting the result of param.split("=", 1).
func paramLess(a, b string) bool {
	ak, av, aok := strings.Cut(a, "=")
	bk, bv, bok := strings.Cut(b, "=")
	if ak != bk {
		return ak < bk
	}
	if aok != bok {
		return !aok
	}
	return av < bv
}

// normalizePath resolves "." and ".." segments and collapses repeated
// slashes the way the Python surt canonicalizer does: ".." removes the
// previous segment (even an empty one) and is kept at the root.
func normalizePath(p string) string {
	var keep []string
	for _, seg := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		switch {
		case seg == ".":
		case seg == ".." && len(keep) > 0:
			keep = keep[:len(keep)-1]
		default:
			keep = append(keep, seg)
		}
	}
	out := "/" + strings.Join(keep, "/")
	for strings.Contains(out, "//") {
		out = strings.ReplaceAll(out, "//", "/")
	}
	return out
}

// unescapeRepeatedly decodes valid %XX sequences until nothing changes.
func unescapeRepeatedly(s string) string {
	for range 10 {
		next := unescapeOnce(s)
		if next == s {
			return s
		}
		s = next
	}
	return s
}

func unescapeOnce(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			b.WriteByte(unhex(s[i+1])<<4 | unhex(s[i+2]))
			i += 2
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	}
	return c - 'A' + 10
}

// minimalEscape percent-encodes control, space, non-ASCII, '#' and '%'
// bytes, as the Python surt (Google) canonicalizer does.
func minimalEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= 0x20 || c >= 0x7f || c == '#' || c == '%' {
			fmt.Fprintf(&b, "%%%02X", c)
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}
