package warcio

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"strings"
)

// HTTP block content types.
const (
	ContentTypeHTTPResponse = "application/http; msgtype=response"
	ContentTypeHTTPRequest  = "application/http; msgtype=request"
)

// HTTPResponse is a parsed HTTP response block.
type HTTPResponse struct {
	StatusCode int
	Proto      string
	Header     http.Header
	// HeaderBytes holds the raw status line and headers including the final CRLF.
	HeaderBytes []byte
	// Payload is the entity body with transfer-encoding removed and
	// content-encoding preserved. Note that WARC-Payload-Digest is computed
	// over the raw bytes following the headers, as warcio and Heritrix do.
	Payload []byte
}

// ParseHTTPResponse parses a response record block. A missing or truncated
// body yields an empty or partial payload rather than an error.
func ParseHTTPResponse(block []byte) (*HTTPResponse, error) {
	br := bufio.NewReader(bytes.NewReader(block))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	hdrLen := headerEnd(block)
	body, _ := io.ReadAll(resp.Body)
	return &HTTPResponse{
		StatusCode:  resp.StatusCode,
		Proto:       resp.Proto,
		Header:      resp.Header,
		HeaderBytes: block[:hdrLen],
		Payload:     body,
	}, nil
}

// HTTPHeaderBytes returns the status line and headers of an HTTP block.
func HTTPHeaderBytes(block []byte) []byte { return block[:headerEnd(block)] }

func headerEnd(block []byte) int {
	if i := bytes.Index(block, []byte("\r\n\r\n")); i >= 0 {
		return i + 4
	}
	if i := bytes.Index(block, []byte("\n\n")); i >= 0 {
		return i + 2
	}
	return len(block)
}

// StripHeaders removes the named headers from a raw HTTP message (request or
// response) and returns the rewritten message. The body is left untouched.
func StripHeaders(msg []byte, names ...string) []byte {
	end := headerEnd(msg)
	head := msg[:end]
	lines := bytes.SplitAfter(head, []byte("\n"))
	var out bytes.Buffer
	out.Grow(len(msg))
	skipping := false
	for i, line := range lines {
		if i == 0 {
			out.Write(line)
			continue
		}
		if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') && len(bytes.TrimSpace(line)) > 0 {
			if !skipping {
				out.Write(line)
			}
			continue
		}
		skipping = false
		name, _, ok := bytes.Cut(line, []byte(":"))
		if ok {
			for _, n := range names {
				if strings.EqualFold(string(bytes.TrimSpace(name)), n) {
					skipping = true
					break
				}
			}
		}
		if !skipping {
			out.Write(line)
		}
	}
	out.Write(msg[end:])
	return out.Bytes()
}
