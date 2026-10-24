package static

import (
	"bytes"
	"net/url"
	"strings"

	"github.com/tdewolff/parse/v2"
	"github.com/tdewolff/parse/v2/css"
)

// CSSRef is a URL referenced from CSS.
type CSSRef struct {
	Raw  string
	Kind Kind
}

// CSSRefs lists url() and @import references of a stylesheet.
func CSSRefs(src []byte) []CSSRef {
	var refs []CSSRef
	walkCSS(src, func(raw string, kind Kind) (string, bool) {
		refs = append(refs, CSSRef{Raw: raw, Kind: kind})
		return "", false
	})
	return refs
}

// ResolveCSSRefs returns absolute URLs referenced from CSS located at base.
func ResolveCSSRefs(src []byte, base *url.URL) []Ref {
	var out []Ref
	for _, r := range CSSRefs(src) {
		if abs := Resolve(base, r.Raw); abs != "" {
			out = append(out, Ref{URL: abs, Kind: r.Kind})
		}
	}
	return out
}

// RewriteCSS replaces references for which fn returns ok.
func RewriteCSS(src []byte, fn func(raw string, kind Kind) (string, bool)) []byte {
	return walkCSS(src, fn)
}

func walkCSS(src []byte, fn func(raw string, kind Kind) (string, bool)) []byte {
	lex := css.NewLexer(parse.NewInputBytes(bytes.Clone(src)))
	var out bytes.Buffer
	out.Grow(len(src))
	afterImport := false
	inURLFunc := false
	imageSetDepth := 0
	depth := 0
	for {
		tt, data := lex.Next()
		if tt == css.ErrorToken {
			break
		}
		switch tt {
		case css.AtKeywordToken:
			afterImport = strings.EqualFold(string(data), "@import")
			out.Write(data)
			continue
		case css.URLToken:
			raw := unquoteURLToken(data)
			kind := KindAsset
			if afterImport {
				kind = KindStyle
			}
			afterImport = false
			if rep, ok := fn(raw, kind); ok {
				out.WriteString(`url("` + escapeCSSString(rep) + `")`)
			} else {
				out.Write(data)
			}
			continue
		case css.FunctionToken:
			name := strings.ToLower(string(data))
			depth++
			if name == "url(" {
				inURLFunc = true
			} else if strings.HasSuffix(name, "image-set(") && imageSetDepth == 0 {
				imageSetDepth = depth
			}
			out.Write(data)
			continue
		case css.LeftParenthesisToken:
			depth++
		case css.RightParenthesisToken:
			if imageSetDepth == depth {
				imageSetDepth = 0
			}
			depth--
			inURLFunc = false
		case css.StringToken:
			if afterImport || inURLFunc || imageSetDepth > 0 {
				kind := KindAsset
				if afterImport {
					kind = KindStyle
				}
				afterImport = false
				raw := unescapeCSS(unquote(string(data)))
				if rep, ok := fn(raw, kind); ok {
					out.WriteString(`"` + escapeCSSString(rep) + `"`)
				} else {
					out.Write(data)
				}
				continue
			}
		case css.WhitespaceToken, css.CommentToken:
		default:
			afterImport = false
		}
		out.Write(data)
	}
	return out.Bytes()
}

func unquoteURLToken(tok []byte) string {
	s := string(tok)
	if i := strings.IndexByte(s, '('); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimSuffix(s, ")")
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		s = s[1 : len(s)-1]
	}
	return unescapeCSS(s)
}

// unquote removes the quotes of a CSS string token, which may be
// unterminated at the end of the input.
func unquote(s string) string {
	if s == "" || (s[0] != '"' && s[0] != '\'') {
		return s
	}
	q := s[0]
	s = s[1:]
	if strings.HasSuffix(s, string(q)) && !strings.HasSuffix(s, `\`+string(q)) {
		s = s[:len(s)-1]
	}
	return s
}

func unescapeCSS(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			if s[i] == '\n' {
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func escapeCSSString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\a `)
	return r.Replace(s)
}
