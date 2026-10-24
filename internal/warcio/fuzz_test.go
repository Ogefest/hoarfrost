package warcio

import (
	"bytes"
	"io"
	"testing"
)

func FuzzReader(f *testing.F) {
	var gz bytes.Buffer
	NewWriter(&gz).WriteRecord(responseRecord("https://example.com/", "seed"))
	var plain bytes.Buffer
	Encode(&plain, responseRecord("https://example.com/", "seed"))
	f.Add(gz.Bytes())
	f.Add(plain.Bytes())
	f.Add([]byte("WARC/1.1\r\nContent-Length: 3\r\n\r\nabc\r\n\r\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		rd := NewReader(bytes.NewReader(data))
		rd.MaxBlock = 1 << 20
		for range 100 {
			rec, _, _, err := rd.Next()
			if err != nil {
				if err == io.EOF {
					return
				}
				return
			}
			var out bytes.Buffer
			if err := Encode(&out, rec); err != nil {
				return
			}
			again, _, _, err := NewReader(&out).Next()
			if err != nil {
				t.Fatalf("re-read failed: %v", err)
			}
			if !bytes.Equal(again.Block, rec.Block) {
				t.Fatal("block changed on round trip")
			}
		}
	})
}

func FuzzParseHTTPResponse(f *testing.F) {
	f.Add([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"))
	f.Fuzz(func(t *testing.T, data []byte) {
		ParseHTTPResponse(data)
		StripHeaders(data, "Cookie")
	})
}
