package monolith

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func TestSplit(t *testing.T) {
	img := bytes.Repeat([]byte{0xff, 0xd8, 0x01}, 600)
	font := bytes.Repeat([]byte("WOFF"), 400)
	css := []byte("body{background:url(data:font/woff2;base64," + b64(font) + ")}" + strings.Repeat(" ", 1100))
	doc := `<html><head><link rel="stylesheet" href="data:text/css;charset=utf-8;base64,` + b64(css) + `">` +
		`</head><body><img src="data:image/jpeg;base64,` + b64(img) + `"><img src='data:image/jpeg;base64,` + b64(img) + `'>` +
		`<img src="data:image/png;base64,iVBORw0KGgo="></body></html>`
	out, res := Split([]byte(doc), 1024)
	if len(res) != 3 {
		t.Fatalf("resources = %d, want 3 (font, css, one image)", len(res))
	}
	if !bytes.Contains(out, []byte("data:image/png;base64,iVBORw0KGgo=")) {
		t.Error("small data: URI was moved out")
	}
	if bytes.Contains(out, []byte(";base64,/9")) {
		t.Error("large image left inline")
	}
	if bytes.Count(out, []byte(res[2].URL)) != 2 {
		t.Errorf("image URL not used twice: %s", out)
	}
	if res[0].ContentType != "font/woff2" || !bytes.Equal(res[0].Body, font) {
		t.Errorf("font = %q", res[0].ContentType)
	}
	if res[1].ContentType != "text/css;charset=utf-8" || !bytes.Contains(res[1].Body, []byte(res[0].URL)) {
		t.Errorf("css not rewritten: %s", res[1].Body[:80])
	}
	if !strings.HasPrefix(res[2].URL, "https://monolith.invalid/sha256/") || !strings.HasSuffix(res[2].URL, ".jpg") {
		t.Errorf("url = %s", res[2].URL)
	}
	if ResourceURL(img, "image/jpeg") != res[2].URL {
		t.Error("URL is not content-addressed")
	}
}

func TestSplitUnpadded(t *testing.T) {
	body := bytes.Repeat([]byte("ab"), 1001) // 2002 bytes: base64 needs padding
	enc := strings.TrimRight(b64(body), "=")
	_, res := Split([]byte(`<img src="data:image/gif;base64,`+enc+`">`), 1024)
	if len(res) != 1 || !bytes.Equal(res[0].Body, body) {
		t.Fatalf("unpadded payload not decoded: %d", len(res))
	}
}
