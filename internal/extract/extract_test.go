package extract

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"golang.org/x/text/encoding/charmap"
)

func TestHTMLArticle(t *testing.T) {
	body, err := os.ReadFile("testdata/article.html")
	if err != nil {
		t.Fatal(err)
	}
	res := HTML(body, "text/html; charset=utf-8", "https://serwis.pl/kraj/budzet")
	if !strings.Contains(res.Title, "Radni przyjęli budżet miasta") {
		t.Errorf("title %q", res.Title)
	}
	if !strings.Contains(res.Text, "transport publiczny") {
		t.Errorf("text missing article body: %q", res.Text)
	}
	if strings.Contains(res.Text, "do not index") || strings.Contains(res.Text, "Wszelkie prawa") {
		t.Errorf("boilerplate in text: %q", res.Text)
	}
	if res.Published != "2026-10-09" {
		t.Errorf("published %q", res.Published)
	}
}

func TestHTMLLegacyCharset(t *testing.T) {
	src := "<html><head><title>Zażółć gęślą jaźń</title></head><body><p>Treść</p></body></html>"
	enc, err := charmap.ISO8859_2.NewEncoder().String(src)
	if err != nil {
		t.Fatal(err)
	}
	res := HTML([]byte(enc), "text/html; charset=iso-8859-2", "")
	if res.Title != "Zażółć gęślą jaźń" {
		t.Errorf("title %q", res.Title)
	}
}

func TestHTMLFallbackForShortPages(t *testing.T) {
	res := HTML([]byte("<html><head><title>Krótka</title></head><body><div>Tylko kilka słów</div><script>x()</script></body></html>"), "text/html", "")
	if res.Title != "Krótka" || !strings.Contains(res.Text, "Tylko kilka słów") || strings.Contains(res.Text, "x()") {
		t.Errorf("fallback %+v", res)
	}
}

func TestDocumentDispatch(t *testing.T) {
	ctx := context.Background()
	res, err := Document(ctx, []byte("Pierwsza linia\n\n\n\nDruga   linia"), "text/plain; charset=utf-8", "")
	if err != nil || res.Title != "Pierwsza linia" || res.Text != "Pierwsza linia\n\nDruga linia" {
		t.Fatalf("plain %+v %v", res, err)
	}
	if _, err := Document(ctx, []byte{0, 1}, "image/png", ""); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("png: %v", err)
	}
	res, err = Document(ctx, []byte("<!DOCTYPE html><title>X</title><p>y</p>"), "", "")
	if err != nil || res.Title != "X" {
		t.Fatalf("sniffed html %+v %v", res, err)
	}
}

func TestDOCXAndODT(t *testing.T) {
	mk := func(member, content string) []byte {
		var b bytes.Buffer
		zw := zip.NewWriter(&b)
		w, _ := zw.Create(member)
		w.Write([]byte(content))
		zw.Close()
		return b.Bytes()
	}
	docx := mk("word/document.xml", `<w:document xmlns:w="x"><w:body><w:p><w:r><w:t>Raport roczny</w:t></w:r></w:p><w:p><w:r><w:t>Druga część</w:t></w:r></w:p></w:body></w:document>`)
	res, err := Document(context.Background(), docx, "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "")
	if err != nil || res.Text != "Raport roczny\nDruga część" {
		t.Fatalf("docx %+v %v", res, err)
	}
	odt := mk("content.xml", `<office:document-content xmlns:office="o" xmlns:text="t"><text:h>Tytuł</text:h><text:p>Akapit</text:p></office:document-content>`)
	res, err = Document(context.Background(), odt, "application/vnd.oasis.opendocument.text", "")
	if err != nil || res.Title != "Tytuł" {
		t.Fatalf("odt %+v %v", res, err)
	}
}

// minimalPDF builds a one-page PDF showing text.
func minimalPDF(text string) []byte {
	stream := fmt.Sprintf("BT /F1 12 Tf 72 720 Td (%s) Tj ET", text)
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes()
}

func TestPDF(t *testing.T) {
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Skip("pdftotext not installed")
	}
	res, err := Document(context.Background(), minimalPDF("Uchwala budzetowa 2026"), "application/pdf", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "Uchwala budzetowa 2026" || res.Title != res.Text {
		t.Fatalf("pdf %+v", res)
	}
}

func TestOCRWithoutTool(t *testing.T) {
	if _, err := exec.LookPath("tesseract"); err == nil {
		t.Skip("tesseract installed")
	}
	if _, err := OCR(context.Background(), []byte("x"), "image/png", ""); !errors.Is(err, ErrToolMissing) {
		t.Fatalf("want ErrToolMissing, got %v", err)
	}
}

func FuzzHTML(f *testing.F) {
	f.Add([]byte("<html><title>t</title><p>x</p></html>"))
	f.Fuzz(func(t *testing.T, b []byte) {
		HTML(b, "text/html", "https://example.com/")
	})
}

func TestStructuredData(t *testing.T) {
	body, err := os.ReadFile("testdata/structured.html")
	if err != nil {
		t.Fatal(err)
	}
	s := HTML(body, "text/html; charset=utf-8", "https://serwis.pl/budzet").Structured
	if s.Description != "Rada miasta przyjęła budżet na 2027 rok." {
		t.Errorf("description %q", s.Description)
	}
	if got := strings.Join(Types(s.SchemaOrg), ","); got != "BreadcrumbList,Event,NewsArticle,Person" {
		t.Errorf("types %s (%d objects)", got, len(s.SchemaOrg))
	}
	if got := strings.Join(People(s.SchemaOrg), ","); got != "Anna Kowalska,Jan Nowak,Ewa Zielińska" {
		t.Errorf("people %s", got)
	}
	if DatePublished(s.SchemaOrg) != "2026-10-09T08:15:00+02:00" {
		t.Errorf("datePublished %q", DatePublished(s.SchemaOrg))
	}
	var event map[string]any
	for _, o := range s.SchemaOrg {
		if o["@type"] == "Event" {
			event = o
		}
	}
	place, _ := event["location"].(map[string]any)
	if event["startDate"] != "2026-10-08T17:00" || event["name"] != "Sesja rady miasta" || place["name"] != "Ratusz" || event["organizer"] != "Rada Miasta" {
		t.Errorf("microdata event %v", event)
	}
	for _, o := range s.SchemaOrg {
		if _, ok := o["@context"]; ok {
			t.Errorf("context kept in %v", o)
		}
	}
}

func TestStructuredDataFallbacksAndLimits(t *testing.T) {
	s := StructuredData([]byte(`<html><head><meta property="og:description" content="Z OG"></head><body></body></html>`), "text/html")
	if s.Description != "Z OG" || len(s.SchemaOrg) != 0 {
		t.Errorf("%+v", s)
	}
	var b strings.Builder
	b.WriteString(`<html><head>`)
	for i := range 100 {
		fmt.Fprintf(&b, `<script type="application/ld+json">{"@context":"https://schema.org","@type":"Thing","name":"%d","x":"%s"}</script>`, i, strings.Repeat("a", 4000))
	}
	b.WriteString(`</head></html>`)
	s = StructuredData([]byte(b.String()), "text/html")
	if n := len(s.SchemaOrg); n == 0 || n > maxSchemaObjects {
		t.Errorf("%d objects", n)
	}
	size := 0
	for _, o := range s.SchemaOrg {
		j, _ := json.Marshal(o)
		size += len(j)
	}
	if size > maxSchemaBytes {
		t.Errorf("%d bytes kept", size)
	}
}
