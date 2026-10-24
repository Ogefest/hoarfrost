// Package warcio reads and writes WARC 1.1 records, one gzip member per record.
package warcio

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/textproto"
	"strconv"
	"strings"
)

// Standard WARC header names.
const (
	HType              = "WARC-Type"
	HRecordID          = "WARC-Record-ID"
	HDate              = "WARC-Date"
	HTargetURI         = "WARC-Target-URI"
	HContentLength     = "Content-Length"
	HContentType       = "Content-Type"
	HBlockDigest       = "WARC-Block-Digest"
	HPayloadDigest     = "WARC-Payload-Digest"
	HConcurrentTo      = "WARC-Concurrent-To"
	HRefersTo          = "WARC-Refers-To"
	HRefersToTargetURI = "WARC-Refers-To-Target-URI"
	HRefersToDate      = "WARC-Refers-To-Date"
	HProfile           = "WARC-Profile"
	HWarcinfoID        = "WARC-Warcinfo-ID"
	HFilename          = "WARC-Filename"
	HIPAddress         = "WARC-IP-Address"
	HTruncated         = "WARC-Truncated"
)

// Record types.
const (
	TypeWarcinfo = "warcinfo"
	TypeRequest  = "request"
	TypeResponse = "response"
	TypeRevisit  = "revisit"
	TypeResource = "resource"
	TypeMetadata = "metadata"
)

// Revisit profiles.
const (
	ProfileIdenticalPayload  = "http://netpreserve.org/warc/1.1/revisit/identical-payload-digest"
	ProfileServerNotModified = "http://netpreserve.org/warc/1.1/revisit/server-not-modified"
)

type field struct {
	name  string
	value string
}

// Header is an ordered list of WARC named fields with case-insensitive lookup.
type Header struct {
	fields []field
}

// Get returns the first value for name, or "".
func (h *Header) Get(name string) string {
	for _, f := range h.fields {
		if strings.EqualFold(f.name, name) {
			return f.value
		}
	}
	return ""
}

// Set replaces all values for name, keeping the position of the first one.
func (h *Header) Set(name, value string) {
	for i, f := range h.fields {
		if strings.EqualFold(f.name, name) {
			h.fields[i].value = value
			h.del(name, i+1)
			return
		}
	}
	h.fields = append(h.fields, field{name, value})
}

// Add appends a value without removing existing ones.
func (h *Header) Add(name, value string) {
	h.fields = append(h.fields, field{name, value})
}

// Del removes all values for name.
func (h *Header) Del(name string) { h.del(name, 0) }

func (h *Header) del(name string, from int) {
	out := h.fields[:from]
	for _, f := range h.fields[from:] {
		if !strings.EqualFold(f.name, name) {
			out = append(out, f)
		}
	}
	h.fields = out
}

// Values returns all values for name.
func (h *Header) Values(name string) []string {
	var vs []string
	for _, f := range h.fields {
		if strings.EqualFold(f.name, name) {
			vs = append(vs, f.value)
		}
	}
	return vs
}

// Len returns the number of fields.
func (h *Header) Len() int { return len(h.fields) }

// Each calls fn for every field in order.
func (h *Header) Each(fn func(name, value string)) {
	for _, f := range h.fields {
		fn(f.name, f.value)
	}
}

func (h *Header) writeTo(w io.Writer) error {
	bw := bufio.NewWriter(w)
	bw.WriteString("WARC/1.1\r\n")
	for _, f := range h.fields {
		if strings.ContainsAny(f.name, "\r\n:") || strings.ContainsAny(f.value, "\r\n") {
			return fmt.Errorf("warcio: invalid header field %q", f.name)
		}
		bw.WriteString(f.name)
		bw.WriteString(": ")
		bw.WriteString(f.value)
		bw.WriteString("\r\n")
	}
	bw.WriteString("\r\n")
	return bw.Flush()
}

func readHeader(r *bufio.Reader) (Header, error) {
	var h Header
	line, err := readLine(r)
	for err == nil && line == "" {
		line, err = readLine(r)
	}
	if err != nil {
		return h, err
	}
	if !strings.HasPrefix(line, "WARC/") {
		return h, fmt.Errorf("warcio: bad version line %q", truncate(line, 40))
	}
	tp := textproto.NewReader(r)
	for {
		l, err := tp.ReadContinuedLine()
		if err != nil {
			return h, noEOF(err)
		}
		if l == "" {
			return h, nil
		}
		name, value, ok := strings.Cut(l, ":")
		if !ok {
			return h, fmt.Errorf("warcio: malformed header line %q", truncate(l, 40))
		}
		h.fields = append(h.fields, field{strings.TrimSpace(name), strings.TrimSpace(value)})
	}
}

func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil && (err != io.EOF || line == "") {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (h *Header) contentLength() (int64, error) {
	v := h.Get(HContentLength)
	if v == "" {
		return 0, fmt.Errorf("warcio: missing Content-Length")
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("warcio: bad Content-Length %q", v)
	}
	return n, nil
}

func noEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
