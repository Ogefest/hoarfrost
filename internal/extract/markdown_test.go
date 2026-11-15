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

// A utility class on the article container that merely mentions "teaser"
// made trafilatura discard the whole article and return the footer.
func TestHTMLIgnoresUtilityClasses(t *testing.T) {
	para := func(s string) string {
		return `<div class="wp-content-part contents"><div class="wp-content-text-raw x-tts"><p>` +
			strings.Repeat(s+" ", 4) + `</p></div></div>`
	}
	src := `<html><head><title>T</title></head><body><main><article>
<div class="article-body-grid"><div class="mb-4 flex flex-col [&_.wp-content-part-teaser]:mb-0 md:text-xl">` +
		para("Do zbrodni doszło w środę w domu jednorodzinnym przy ul. Sublańskiej.") +
		para("Policjanci szybko ustalili miejsce pobytu podejrzanego i go zatrzymali.") +
		para("Postępowanie nadzoruje prokuratura rejonowa, śledczy wyjaśniają przebieg.") +
		`</div></div></article></main>
<footer><ul><li>Regulamin</li><li>Reklama</li><li>O nas</li></ul><p>` +
		strings.Repeat("Pobieranie, zwielokrotnianie i przechowywanie treści wymaga zgody wydawcy. ", 8) +
		`</p></footer></body></html>`
	res := HTML([]byte(src), "text/html", "https://example.com/a")
	if !strings.Contains(res.Text, "Sublańskiej") || strings.Contains(res.Text, "Regulamin") {
		t.Errorf("article lost to the footer:\n%s", res.Text)
	}
}

func TestDropUtilityClasses(t *testing.T) {
	doc, _ := html.Parse(strings.NewReader(`<div class="article md:pb-4 w-[304px] [&_.teaser]:mb-0 lead"></div>`))
	dropUtilityClasses(doc)
	var b strings.Builder
	html.Render(&b, doc)
	if !strings.Contains(b.String(), `class="article lead"`) {
		t.Errorf("got %s", b.String())
	}
}
