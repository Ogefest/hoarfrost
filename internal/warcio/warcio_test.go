package warcio

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

var testDate = time.Date(2026, 10, 11, 7, 2, 0, 0, time.UTC)

func responseRecord(uri, body string) *Record {
	r := New(TypeResponse, testDate)
	r.Header.Set(HTargetURI, uri)
	r.Header.Set(HContentType, ContentTypeHTTPResponse)
	r.Block = []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: " +
		fmt.Sprint(len(body)) + "\r\n\r\n" + body)
	r.Header.Set(HPayloadDigest, Digest([]byte(body)))
	return r
}

func TestWriteReadRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	type pos struct{ off, n int64 }
	var positions []pos
	var written []*Record
	for i := range 5 {
		r := responseRecord(fmt.Sprintf("https://example.com/%d", i), strings.Repeat("x", i*100))
		off, n, err := w.WriteRecord(r)
		if err != nil {
			t.Fatal(err)
		}
		positions = append(positions, pos{off, n})
		written = append(written, r)
	}
	if w.Offset() != int64(buf.Len()) {
		t.Fatalf("offset %d != len %d", w.Offset(), buf.Len())
	}

	rd := NewReader(bytes.NewReader(buf.Bytes()))
	for i := 0; ; i++ {
		rec, off, n, err := rd.Next()
		if err == io.EOF {
			if i != 5 {
				t.Fatalf("got %d records", i)
			}
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if off != positions[i].off || n != positions[i].n {
			t.Errorf("record %d: pos (%d,%d) want (%d,%d)", i, off, n, positions[i].off, positions[i].n)
		}
		if rec.ID() != written[i].ID() || !bytes.Equal(rec.Block, written[i].Block) {
			t.Errorf("record %d mismatch", i)
		}
		ok, err := VerifyDigest(rec.Header.Get(HBlockDigest), rec.Block)
		if err != nil || !ok {
			t.Errorf("record %d block digest invalid: %v", i, err)
		}

		at, err := ReadRecordAt(bytes.NewReader(buf.Bytes()), off, n)
		if err != nil {
			t.Fatal(err)
		}
		if at.ID() != rec.ID() {
			t.Errorf("ReadRecordAt %d: id mismatch", i)
		}
	}
}

func TestPlainReader(t *testing.T) {
	var buf bytes.Buffer
	var ids []string
	var offs []int64
	for i := range 3 {
		r := responseRecord(fmt.Sprintf("https://example.com/%d", i), "body")
		offs = append(offs, int64(buf.Len()))
		if err := Encode(&buf, r); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.ID())
	}
	rd := NewReader(&buf)
	for i := range 3 {
		rec, off, _, err := rd.Next()
		if err != nil {
			t.Fatal(err)
		}
		if rec.ID() != ids[i] || off != offs[i] {
			t.Errorf("record %d: id %s off %d", i, rec.ID(), off)
		}
	}
	if _, _, _, err := rd.Next(); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
}

func TestEmptyStream(t *testing.T) {
	if _, _, _, err := NewReader(bytes.NewReader(nil)).Next(); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
}

func TestMaxBlock(t *testing.T) {
	var buf bytes.Buffer
	if err := Encode(&buf, responseRecord("https://example.com/", strings.Repeat("a", 1000))); err != nil {
		t.Fatal(err)
	}
	rd := NewReader(&buf)
	rd.MaxBlock = 100
	if _, _, _, err := rd.Next(); err == nil {
		t.Fatal("expected error")
	}
}

func TestTruncatedRecord(t *testing.T) {
	var buf bytes.Buffer
	Encode(&buf, responseRecord("https://example.com/", "hello world"))
	data := buf.Bytes()[:buf.Len()-20]
	if _, _, _, err := NewReader(bytes.NewReader(data)).Next(); err == nil {
		t.Fatal("expected error for truncated record")
	}
}

func TestHeaderOps(t *testing.T) {
	var h Header
	h.Add("A", "1")
	h.Add("b", "2")
	h.Add("a", "3")
	if got := h.Values("a"); len(got) != 2 {
		t.Fatalf("values %v", got)
	}
	h.Set("A", "x")
	if h.Get("a") != "x" || len(h.Values("a")) != 1 || h.Len() != 2 {
		t.Fatalf("set failed: %+v", h)
	}
	h.Del("B")
	if h.Get("b") != "" || h.Len() != 1 {
		t.Fatal("del failed")
	}
	var buf bytes.Buffer
	h.Set("Bad", "x\r\ny")
	if err := h.writeTo(&buf); err == nil {
		t.Fatal("expected error for CRLF in value")
	}
}

func TestDigest(t *testing.T) {
	d := Digest([]byte("hello"))
	if d != "sha1:VL2MMHO4YXUKFWV63YHTWSBM3GXKSQ2N" {
		t.Fatalf("digest %s", d)
	}
	h := NewDigester()
	h.Write([]byte("hello"))
	if FormatDigest(h) != d {
		t.Fatal("streaming digest differs")
	}
	cases := []struct {
		digest string
		ok     bool
	}{
		{d, true},
		{"sha1:aaf4c61ddcc5e8a2dabede0f3b482cd9aea9434d", true},
		{"sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824", true},
		{"md5:5d41402abc4b2a76b9719d911017c592", true},
		{"sha1:AAAA", false},
	}
	for _, c := range cases {
		ok, err := VerifyDigest(c.digest, []byte("hello"))
		if err != nil || ok != c.ok {
			t.Errorf("%s: ok=%v err=%v", c.digest, ok, err)
		}
	}
	if _, err := VerifyDigest("crc:1", nil); err == nil {
		t.Error("expected unsupported algorithm error")
	}
}

func TestRecordIDAndDate(t *testing.T) {
	id := NewRecordID()
	if !strings.HasPrefix(id, "<urn:uuid:") || len(id) != 47 || id == NewRecordID() {
		t.Fatalf("bad id %s", id)
	}
	d, err := ParseDate("2026-10-11T07:02:00.123Z")
	if err != nil || d.Nanosecond() == 0 {
		t.Fatal(err)
	}
	if FormatDate(testDate.In(time.FixedZone("x", 3600))) != "2026-10-11T07:02:00Z" {
		t.Fatal("FormatDate not UTC")
	}
}

func TestParseHTTPResponseChunked(t *testing.T) {
	block := []byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nContent-Encoding: gzip\r\n\r\n5\r\nhello\r\n6\r\n world\r\n0\r\n\r\n")
	resp, err := ParseHTTPResponse(block)
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Payload) != "hello world" || resp.StatusCode != 200 {
		t.Fatalf("payload %q", resp.Payload)
	}
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatal("content-encoding lost")
	}
	if !bytes.HasSuffix(resp.HeaderBytes, []byte("gzip\r\n\r\n")) {
		t.Fatalf("header bytes %q", resp.HeaderBytes)
	}
}

func TestStripHeaders(t *testing.T) {
	msg := []byte("GET / HTTP/1.1\r\nHost: a\r\nCookie: s=1;\r\n  continued\r\nauthorization: Bearer x\r\nAccept: */*\r\n\r\nCookie: body")
	got := string(StripHeaders(msg, "Cookie", "Authorization"))
	want := "GET / HTTP/1.1\r\nHost: a\r\nAccept: */*\r\n\r\nCookie: body"
	if got != want {
		t.Fatalf("got %q", got)
	}
}
