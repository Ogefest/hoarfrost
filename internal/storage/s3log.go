package storage

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// s3LogTransport logs the requests that control objects (starting,
// completing and aborting multipart uploads, DELETE) and every request
// the server refuses (but for a missing object or upload), so what the server answered can be checked
// after a failed seal. Uploads of parts and reads that succeed are not
// logged.
type s3LogTransport struct {
	base http.RoundTripper
}

func (t *s3LogTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	q := r.URL.Query()
	op := s3Op(r.Method, q)
	start := time.Now()
	resp, err := t.base.RoundTrip(r)
	quiet := op == "upload-part" || op == "get" || op == "list" || op == "bucket" || op == "head" || op == "put"
	if err != nil {
		slog.Warn("s3 request failed", "op", op, "path", r.URL.Path, "duration", time.Since(start), "err", err)
		return resp, err
	}
	if quiet && (resp.StatusCode < 300 || resp.StatusCode == http.StatusNotFound && (op == "head" || op == "list")) {
		return resp, nil
	}
	attrs := []any{"op", op, "path", r.URL.Path, "status", resp.StatusCode, "duration", time.Since(start),
		"request_id", resp.Header.Get("X-Amz-Request-Id"), "server", resp.Header.Get("Server")}
	if op == "complete-upload" || resp.StatusCode >= 300 && r.Method != http.MethodHead {
		// Keep the start of the answer: a server may report an error in
		// the body of a 200 response to CompleteMultipartUpload.
		resp.Body = &s3BodyLog{rc: resp.Body, attrs: attrs, status: resp.StatusCode}
		return resp, nil
	}
	if resp.StatusCode >= 300 {
		slog.Warn("s3 request", attrs...)
	} else {
		slog.Info("s3 request", attrs...)
	}
	return resp, nil
}

func s3Op(method string, q map[string][]string) string {
	has := func(k string) bool { _, ok := q[k]; return ok }
	switch {
	case method == http.MethodPost && has("uploads"):
		return "start-upload"
	case method == http.MethodPost && has("uploadId"):
		return "complete-upload"
	case method == http.MethodPut && has("partNumber"):
		return "upload-part"
	case method == http.MethodDelete && has("uploadId"):
		return "abort-upload"
	case method == http.MethodGet && (has("uploads") || has("list-type") || has("prefix") || has("uploadId")):
		return "list"
	case method == http.MethodGet && has("location"):
		return "bucket"
	}
	return strings.ToLower(method)
}

// s3BodyLog logs a response with the start of its body when it is closed.
type s3BodyLog struct {
	rc     io.ReadCloser
	attrs  []any
	status int
	head   bytes.Buffer
	logged bool
}

func (b *s3BodyLog) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if room := 2048 - b.head.Len(); room > 0 {
		b.head.Write(p[:min(n, room)])
	}
	return n, err
}

func (b *s3BodyLog) Close() error {
	if !b.logged {
		b.logged = true
		attrs := append(b.attrs, "body", strings.TrimSpace(b.head.String()))
		if b.status >= 300 || strings.Contains(b.head.String(), "<Error>") {
			slog.Warn("s3 request", attrs...)
		} else {
			slog.Info("s3 request", attrs...)
		}
	}
	return b.rc.Close()
}
