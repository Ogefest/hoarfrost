package extract

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func TestMarkdownText(t *testing.T) {
	src := `<body><h1>Tytuł</h1><p>Pierwszy   akapit z <b>pogrubieniem</b>
i <a href="/x">linkiem</a>.</p><h2>Sekcja</h2><p>Drugi akapit<br>z łamaniem linii.</p>
<ul><li>punkt pierwszy</li><li>punkt drugi<ul><li>zagnieżdżony</li></ul></li></ul>
<ol><li>raz</li><li>dwa</li></ol>
<blockquote><p>Cytat.</p><p>Drugi akapit cytatu.</p></blockquote>
<code><code>func main() {
    fmt.Println("hi")
}</code></code>
<p>Kod <code>x := 1</code> w tekście.</p>
<table><tr><th>A</th><th>B</th></tr><tr><td>1</td><td>2</td></tr></table>
<div><div>Zagnieżdżony</div> blok</div><script>x()</script></body>`
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	want := "# Tytuł\n\n" +
		"Pierwszy akapit z pogrubieniem i linkiem.\n\n" +
		"## Sekcja\n\n" +
		"Drugi akapit\nz łamaniem linii.\n\n" +
		"- punkt pierwszy\n- punkt drugi\n  - zagnieżdżony\n\n" +
		"1. raz\n2. dwa\n\n" +
		"> Cytat.\n>\n> Drugi akapit cytatu.\n\n" +
		"```\nfunc main() {\n    fmt.Println(\"hi\")\n}\n```\n\n" +
		"Kod x := 1 w tekście.\n\n" +
		"A | B\n1 | 2\n\n" +
		"Zagnieżdżony\n\nblok"
	if got := markdownText(doc); got != want {
		t.Errorf("markdownText:\n%s\n--- want ---\n%s", got, want)
	}
}

func TestHTMLKeepsParagraphs(t *testing.T) {
	p1 := strings.Repeat("Pierwszy akapit artykułu, wystarczająco długi, aby przeszedł filtry ekstrakcji. ", 3)
	p2 := strings.Repeat("Drugi akapit artykułu, również na tyle długi, by nie odrzuciły go heurystyki. ", 3)
	src := `<html><head><title>T</title></head><body><nav><a href="/">Start</a></nav><article><h1>Nagłówek artykułu</h1>
<p>` + p1 + `</p>
<p>` + p2 + `</p>
<ul><li>pierwszy punkt listy</li><li>drugi punkt listy</li></ul>
</article></body></html>`
	res := HTML([]byte(src), "text/html", "https://example.com/a")
	want := strings.TrimSpace(p1) + "\n\n" + strings.TrimSpace(p2) + "\n\n- pierwszy punkt listy\n- drugi punkt listy"
	if !strings.Contains(res.Text, want) {
		t.Errorf("text:\n%s\nwant to contain\n%s", res.Text, want)
	}
}
