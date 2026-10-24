package upload

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ogefest/hoarfrost/internal/recorder"
	"github.com/ogefest/hoarfrost/internal/warcio"
)

const mhtml = "From: <Saved by Blink>\r\n" +
	"Snapshot-Content-Location: https://serwis.pl/artykul\r\n" +
	"Subject: =?utf-8?Q?Bud=C5=BCet_miasta?=\r\n" +
	"Date: Sat, 10 Oct 2026 10:00:00 -0000\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/related;\r\n\ttype=\"text/html\";\r\n\tboundary=\"----MultipartBoundary--abc----\"\r\n\r\n" +
	"------MultipartBoundary--abc----\r\n" +
	"Content-Type: text/html\r\n" +
	"Content-ID: <frame-1@mhtml.blink>\r\n" +
	"Content-Transfer-Encoding: quoted-printable\r\n" +
	"Content-Location: https://serwis.pl/artykul\r\n\r\n" +
	"<html><head><title>Bud=C5=BCet miasta</title></head><body><article><p>Rada miasta przyj=C4=99=\r\nla bud=C5=BCet.</p></article><img src=3D\"https://serwis.pl/logo.png\"></body></html>\r\n" +
	"------MultipartBoundary--abc----\r\n" +
	"Content-Type: image/png\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"Content-Location: https://serwis.pl/logo.png\r\n\r\n" +
	"aGVsbG8=\r\n" +
	"------MultipartBoundary--abc------\r\n"

func newRecorder(t *testing.T) *recorder.Recorder {
	r, err := recorder.New(recorder.Options{Epoch: "2026-W41", SpoolDir: t.TempDir(), Source: "upload", Engine: "upload"})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func records(t *testing.T, cf recorder.CaptureFile) []*warcio.Record {
	f, _ := os.Open(cf.Path)
	defer f.Close()
	recs, err := warcio.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func TestMHTML(t *testing.T) {
	m, err := ParseMHTML([]byte(mhtml))
	if err != nil {
		t.Fatal(err)
	}
	if m.URL != "https://serwis.pl/artykul" || m.Title != "Budżet miasta" || len(m.Parts) != 2 || string(m.Parts[1].Body) != "hello" {
		t.Fatalf("%+v", m)
	}
	if h := m.HTML(); h == nil || !strings.Contains(string(h.Body), "przyjęla budżet") {
		t.Fatalf("html part %q", m.HTML().Body)
	}

	r := newRecorder(t)
	res, err := Store(context.Background(), r, Upload{Filename: "page.mhtml", Body: []byte(mhtml)})
	if err != nil {
		t.Fatal(err)
	}
	cf, _ := r.Close()
	if res.Kind != KindMHTML || len(res.Pages) != 1 || res.Pages[0].Title != "Budżet miasta" || !strings.Contains(res.Pages[0].Text, "przyjęla") {
		t.Fatalf("%+v", res)
	}
	recs := records(t, cf)
	if recs[1].Type() != warcio.TypeResource || recs[1].Header.Get(warcio.HContentType) != MHTMLContentType || !bytes.Equal(recs[1].Block, []byte(mhtml)) {
		t.Fatal("mhtml resource record")
	}
	if !res.Pages[0].Captured.Equal(time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("date %v", res.Pages[0].Captured)
	}
}

func TestSingleFile(t *testing.T) {
	body := []byte("<!DOCTYPE html> <html><!--\n Page saved with SingleFile \n url: https://serwis.pl/a \n saved date: x\n--><head><title>T</title></head><body><p>Tekst</p></body></html>")
	r := newRecorder(t)
	res, err := Store(context.Background(), r, Upload{Body: body})
	if err != nil || res.Kind != KindSingleFile || res.Pages[0].URL != "https://serwis.pl/a" || res.Pages[0].Title != "T" {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := Store(context.Background(), r, Upload{Body: []byte("<html><p>x</p></html>")}); err == nil {
		t.Fatal("upload without URL must fail")
	}
	res, err = Store(context.Background(), r, Upload{Body: []byte("<html><p>x</p></html>"), URL: "https://b.pl/", Title: "Override"})
	if err != nil || res.Pages[0].Title != "Override" {
		t.Fatal(err)
	}
}

func foreignWARC(t *testing.T) []byte {
	var b bytes.Buffer
	w := warcio.NewWriter(&b)
	date := time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC)
	resp := warcio.New(warcio.TypeResponse, date)
	resp.Header.Set(warcio.HTargetURI, "https://serwis.pl/x")
	resp.Header.Set(warcio.HContentType, warcio.ContentTypeHTTPResponse)
	html := "<html><head><title>Obca</title></head><body><p>Artykul z rozszerzenia</p></body></html>"
	resp.Block = []byte("HTTP/1.1 200 OK\r\nContent-Type: text/html\r\nSet-Cookie: sid=secret\r\n\r\n" + html)
	req := warcio.New(warcio.TypeRequest, date)
	req.Header.Set(warcio.HTargetURI, "https://serwis.pl/x")
	req.Header.Set(warcio.HConcurrentTo, resp.ID())
	req.Header.Set(warcio.HContentType, warcio.ContentTypeHTTPRequest)
	req.Block = []byte("GET /x HTTP/1.1\r\nHost: serwis.pl\r\nCookie: sid=secret\r\nAuthorization: Basic eDp5\r\n\r\n")
	rev := warcio.New(warcio.TypeRevisit, date)
	rev.Header.Set(warcio.HTargetURI, "https://serwis.pl/y")
	rev.Header.Set(warcio.HRefersTo, "<urn:uuid:elsewhere>")
	for _, r := range []*warcio.Record{req, resp, rev} {
		if _, _, err := w.WriteRecord(r); err != nil {
			t.Fatal(err)
		}
	}
	return b.Bytes()
}

func TestWACZAndWARCStripSecrets(t *testing.T) {
	warc := foreignWARC(t)
	var z bytes.Buffer
	zw := zip.NewWriter(&z)
	f, _ := zw.CreateHeader(&zip.FileHeader{Name: "archive/data.warc.gz", Method: zip.Store})
	f.Write(warc)
	p, _ := zw.Create("pages/pages.jsonl")
	p.Write([]byte(`{"format":"json-pages-1.0","id":"pages"}` + "\n" + `{"url":"https://serwis.pl/x","title":"Z listy","ts":"2026-10-10T08:00:00Z"}` + "\n"))
	zw.Close()

	for name, body := range map[string][]byte{"wacz": z.Bytes(), "warc": warc} {
		t.Run(name, func(t *testing.T) {
			r := newRecorder(t)
			res, err := Store(context.Background(), r, Upload{Body: body})
			if err != nil {
				t.Fatal(err)
			}
			cf, _ := r.Close()
			if len(res.Pages) != 1 || res.Pages[0].URL != "https://serwis.pl/x" || !strings.Contains(res.Pages[0].Text, "rozszerzenia") {
				t.Fatalf("%+v", res.Pages)
			}
			if name == "wacz" && res.Pages[0].Title != "Z listy" {
				t.Fatal("title from pages.jsonl")
			}
			for _, rec := range records(t, cf) {
				if bytes.Contains(rec.Block, []byte("secret")) || bytes.Contains(rec.Block, []byte("Authorization")) {
					t.Fatalf("%s record keeps sensitive data: %q", rec.Type(), rec.Block)
				}
				if rec.Type() == warcio.TypeRevisit {
					t.Fatal("foreign revisits must not be copied")
				}
				if ok, _ := warcio.VerifyDigest(rec.Header.Get(warcio.HBlockDigest), rec.Block); !ok {
					t.Fatal("block digest not recomputed")
				}
			}
		})
	}
}

func TestDetect(t *testing.T) {
	cases := map[string]Upload{
		KindWACZ:       {Body: []byte("PK\x03\x04rest")},
		KindWARC:       {Body: []byte("WARC/1.1\r\n")},
		KindMHTML:      {Filename: "a.MHT", Body: []byte("x")},
		KindSingleFile: {Body: []byte("<!DOCTYPE html><html>")},
	}
	for want, u := range cases {
		if got, err := Detect(u); err != nil || got != want {
			t.Errorf("%s: %s %v", want, got, err)
		}
	}
	if _, err := Detect(Upload{Body: []byte{0, 1, 2}}); err != ErrUnknownFormat {
		t.Fatal(err)
	}
}

func FuzzParseMHTML(f *testing.F) {
	f.Add([]byte(mhtml))
	f.Fuzz(func(t *testing.T, b []byte) { ParseMHTML(b) })
}
