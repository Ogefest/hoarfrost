package static

import (
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"
)

func TestParseSrcset(t *testing.T) {
	cases := []struct {
		in   string
		want []Candidate
	}{
		{"a.jpg 400w, b.jpg 1200w", []Candidate{{"a.jpg", 400, 1}, {"b.jpg", 1200, 1}}},
		{"a.jpg, b.jpg 2x", []Candidate{{"a.jpg", 0, 1}, {"b.jpg", 0, 2}}},
		{"  a.jpg  ,b.jpg 1.5x ", []Candidate{{"a.jpg", 0, 1}, {"b.jpg", 0, 1.5}}},
		{"img.php?a=1,2 100w", []Candidate{{"img.php?a=1,2", 100, 1}}},
		{"", nil},
	}
	for _, c := range cases {
		got := ParseSrcset(c.in)
		if len(got) != len(c.want) {
			t.Errorf("%q: %+v", c.in, got)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%q[%d] = %+v, want %+v", c.in, i, got[i], c.want[i])
			}
		}
	}
	best, _ := Best(ParseSrcset("s.jpg 400w, l.jpg 1200w, m.jpg 800w"))
	if best.URL != "l.jpg" {
		t.Fatal(best)
	}
	best, _ = Best(ParseSrcset("a.jpg, b.jpg 3x, c.jpg 2x"))
	if best.URL != "b.jpg" {
		t.Fatal(best)
	}
	if _, ok := Best(nil); ok {
		t.Fatal("empty best")
	}
}

func TestCSSRefs(t *testing.T) {
	src := []byte(`@import "a.css"; @import url('b.css') screen;
	body{background:url(img/x.png) no-repeat; src: url( "f.woff2" ) format("woff2")}
	.y{background-image:image-set("one.png" 1x, url(two.png) 2x)}
	.z{content:"not-a-url.png"; background: url(data:image/png;base64,AAA=)}
	/* url(comment.png) */ .e{background:url(es\(c.png)}`)
	var got []string
	for _, r := range CSSRefs(src) {
		got = append(got, r.Kind.String()+":"+r.Raw)
	}
	want := "style:a.css style:b.css asset:img/x.png asset:f.woff2 asset:one.png asset:two.png asset:data:image/png;base64,AAA= asset:es(c.png"
	if strings.Join(got, " ") != want {
		t.Fatalf("got  %s\nwant %s", strings.Join(got, " "), want)
	}

	base, _ := url.Parse("https://example.com/css/main.css")
	var abs []string
	for _, r := range ResolveCSSRefs(src, base) {
		abs = append(abs, r.URL)
	}
	if len(abs) != 7 || abs[0] != "https://example.com/css/a.css" {
		t.Fatalf("resolved %v", abs)
	}

	out := RewriteCSS(src, func(raw string, k Kind) (string, bool) {
		if raw == "img/x.png" || raw == "a.css" || raw == "one.png" {
			return "data:x\"y", true
		}
		return "", false
	})
	s := string(out)
	for _, frag := range []string{`url("data:x\"y")`, `@import "data:x\"y"`, `image-set("data:x\"y" 1x`, `url( "f.woff2" )`, `"not-a-url.png"`} {
		if !strings.Contains(s, frag) {
			t.Errorf("rewritten CSS missing %q:\n%s", frag, s)
		}
	}
}

func TestParsePage(t *testing.T) {
	body, err := os.ReadFile("../../testsite/site/article.html")
	if err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse("https://serwis.pl/news/article.html")
	p, err := ParsePage(body, "text/html; charset=utf-8", base, ParseOptions{SkipFrames: []string{"youtube.com"}})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range p.Refs {
		got = append(got, r.Kind.String()+" "+strings.TrimPrefix(r.URL, "https://serwis.pl"))
	}
	sort.Strings(got)
	want := []string{
		"asset /fonts/b.woff2",
		"asset /img/favicon.ico",
		"asset /img/large.jpg",
		"asset /img/lazy.jpg",
		"asset /img/logo.png",
		"asset /img/noscript.jpg",
		"asset /img/photo.jpg",
		"asset /img/photo2x.webp",
		"asset /img/small.jpg",
		"asset https://ads.doubleclick.net/pixel.gif",
		"frame /frame.html",
		"script /js/app.js",
		"style /css/main.css",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("refs:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if len(p.Links) != 1 || p.Links[0] != "https://serwis.pl/files/uchwala.pdf" {
		t.Fatalf("links %v", p.Links)
	}
	if len(p.Inline) != 2 {
		t.Fatalf("inline css %d", len(p.Inline))
	}
	if p.Title() != "Radni przyjęli budżet miasta" {
		t.Fatal(p.Title())
	}

	for _, r := range p.Refs {
		r.Set("REWRITTEN")
	}
	out := string(p.Render())
	if strings.Contains(out, "data-src") || strings.Contains(out, "large.jpg") || !strings.Contains(out, `srcset="REWRITTEN"`) {
		t.Fatalf("rewrite incomplete:\n%s", out)
	}
}

func TestUnterminatedCSSString(t *testing.T) {
	refs := CSSRefs([]byte(`@import "next.css`))
	if len(refs) != 1 || refs[0].Raw != "next.css" {
		t.Fatalf("%+v", refs)
	}
	if got := unquote(`'a\'`); got != `a\'` {
		t.Fatal(got)
	}
}

func TestResolveAndHosts(t *testing.T) {
	base, _ := url.Parse("https://a.pl/x/y.html")
	cases := map[string]string{
		"z.png":              "https://a.pl/x/z.png",
		"/z.png#frag":        "https://a.pl/z.png",
		"//cdn.a.pl/z.png":   "https://cdn.a.pl/z.png",
		"javascript:void(0)": "",
		"data:image/png,xx":  "",
		"#top":               "",
		"  ":                 "",
		"mailto:a@b":         "",
	}
	for in, want := range cases {
		if got := Resolve(base, in); got != want {
			t.Errorf("Resolve(%q) = %q, want %q", in, got, want)
		}
	}
	if !HostMatches("stats.g.doubleclick.net", []string{"doubleclick.net"}) || HostMatches("notdoubleclick.net", []string{"doubleclick.net"}) {
		t.Fatal("HostMatches")
	}
	if !IsAttachmentURL("https://a.pl/f/Raport.PDF?x=1") || IsAttachmentURL("https://a.pl/pdf") {
		t.Fatal("IsAttachmentURL")
	}
}

func FuzzParsePage(f *testing.F) {
	f.Add([]byte(`<html><base href="/x/"><img srcset="a 1x, b 2x" data-src="c"><style>@import "d";</style></html>`))
	base, _ := url.Parse("https://example.com/")
	f.Fuzz(func(t *testing.T, b []byte) {
		p, err := ParsePage(b, "text/html", base, ParseOptions{})
		if err != nil {
			return
		}
		for _, r := range p.Refs {
			r.Set("x")
		}
		p.Render()
	})
}

func FuzzCSS(f *testing.F) {
	f.Add([]byte(`@import "a.css"; a{background:url(b.png)} b{x:image-set("c" 1x)}`))
	f.Add([]byte(`@import "`))
	f.Add([]byte(`a{background:url("x.png`))
	f.Fuzz(func(t *testing.T, b []byte) {
		CSSRefs(b)
		RewriteCSS(b, func(string, Kind) (string, bool) { return "x", true })
	})
}
