package recorder

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/warcio"
)

func gzipBytes(s string) []byte {
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	gz.Write([]byte(s))
	gz.Close()
	return b.Bytes()
}

func testServer() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/gzip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(gzipBytes("<html>hello</html>"))
	})
	mux.HandleFunc("/chunked", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("part1"))
		w.(http.Flusher).Flush()
		w.Write([]byte("part2"))
	})
	mux.HandleFunc("/style.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		w.Write([]byte("body{color:red}"))
	})
	mux.HandleFunc("/copy.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		w.Write([]byte("body{color:red}"))
	})
	mux.HandleFunc("/304", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	})
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/style.css", http.StatusFound)
	})
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) {
		w.Write(bytes.Repeat([]byte("x"), 10000))
	})
	mux.HandleFunc("/echo-headers", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "session=1")
		w.Write([]byte(r.Header.Get("Cookie")))
	})
	return httptest.NewServer(mux)
}

type mapDedup map[string]Ref

func (m mapDedup) Lookup(_ context.Context, _ epoch.ID, d string) (*Ref, error) {
	if r, ok := m[d]; ok {
		return &r, nil
	}
	return nil, nil
}

func newRecorder(t *testing.T, opts Options) *Recorder {
	t.Helper()
	opts.SpoolDir = t.TempDir()
	if opts.Epoch == "" {
		opts.Epoch = "2026-W41"
	}
	if opts.Source == "" {
		opts.Source = "test source"
	}
	r, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func get(t *testing.T, c *http.Client, url string, hdr ...string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func readFile(t *testing.T, cf CaptureFile) []*warcio.Record {
	t.Helper()
	f, err := os.Open(cf.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	recs, err := warcio.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func TestRecordsRawExchange(t *testing.T) {
	srv := testServer()
	defer srv.Close()
	r := newRecorder(t, Options{Engine: "static", Worker: "w1"})
	c := r.Client()

	resp, body := get(t, c, srv.URL+"/gzip")
	if string(body) != "<html>hello</html>" {
		t.Fatalf("client body %q", body)
	}
	if resp.Header.Get("Content-Encoding") != "" || resp.Header.Get(HeaderRecordID) == "" {
		t.Fatalf("headers %v", resp.Header)
	}
	_, body = get(t, c, srv.URL+"/chunked")
	if string(body) != "part1part2" {
		t.Fatalf("chunked body %q", body)
	}

	cf, err := r.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(cf.Name, ".warc.gz") || !strings.Contains(cf.Name, "-test_source-") {
		t.Fatalf("name %s", cf.Name)
	}
	if cf.Key() != "incoming/2026/W41/"+cf.Name {
		t.Fatalf("key %s", cf.Key())
	}
	recs := readFile(t, cf)
	types := []string{}
	for _, rec := range recs {
		types = append(types, rec.Type())
		ok, err := warcio.VerifyDigest(rec.Header.Get(warcio.HBlockDigest), rec.Block)
		if !ok || err != nil {
			t.Errorf("%s block digest invalid", rec.Type())
		}
		if rec.Type() != warcio.TypeWarcinfo && rec.Header.Get(warcio.HWarcinfoID) != recs[0].ID() {
			t.Errorf("%s missing warcinfo id", rec.Type())
		}
	}
	if strings.Join(types, ",") != "warcinfo,request,response,request,response" {
		t.Fatalf("types %v", types)
	}
	if !bytes.Contains(recs[0].Block, []byte("engine: static")) || !bytes.Contains(recs[0].Block, []byte("epoch: 2026-W41")) {
		t.Fatalf("warcinfo %s", recs[0].Block)
	}

	gz := recs[2]
	if !bytes.Contains(gz.Block, []byte("Content-Encoding: gzip")) {
		t.Fatal("raw response lost content-encoding")
	}
	if !bytes.HasSuffix(gz.Block, gzipBytes("<html>hello</html>")) {
		t.Fatal("raw response body is not the original gzip stream")
	}
	if gz.Header.Get(warcio.HPayloadDigest) != warcio.Digest(gzipBytes("<html>hello</html>")) {
		t.Fatal("payload digest must cover the content-encoded entity")
	}
	if recs[1].Header.Get(warcio.HConcurrentTo) != gz.ID() {
		t.Fatal("request not linked to response")
	}
	if gz.Header.Get(warcio.HIPAddress) != "127.0.0.1" {
		t.Fatalf("ip %q", gz.Header.Get(warcio.HIPAddress))
	}

	ch := recs[4]
	if !bytes.Contains(ch.Block, []byte("Transfer-Encoding: chunked")) {
		t.Fatal("chunked encoding not preserved")
	}
	if ch.Header.Get(warcio.HPayloadDigest) != warcio.Digest(ch.Block[len(warcio.HTTPHeaderBytes(ch.Block)):]) {
		t.Fatal("payload digest must cover the raw body as received (warcio/Heritrix convention)")
	}

	lines := cf.CDXJ()
	if len(lines) != 2 || lines[0].Mime != "text/html" || lines[0].Status != "200" || lines[0].Filename != cf.Name {
		t.Fatalf("cdxj %+v", lines)
	}
	for i, info := range cf.Records {
		at, err := warcio.ReadRecordAt(mustOpen(t, cf.Path), info.Offset, info.Length)
		if err != nil || at.ID() != recs[i].ID() {
			t.Fatalf("record %d not readable at offset: %v", i, err)
		}
	}
}

func mustOpen(t *testing.T, p string) *os.File {
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// slowWriteConn returns from Write only some time after the bytes were sent,
// as when the transport's writer goroutine is descheduled.
type slowWriteConn struct{ net.Conn }

func (c slowWriteConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	time.Sleep(100 * time.Millisecond)
	return n, err
}

func TestRequestRecordedWhenResponseArrivesFirst(t *testing.T) {
	srv := testServer()
	defer srv.Close()
	r := newRecorder(t, Options{Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		return slowWriteConn{conn}, err
	}})
	get(t, r.Client(), srv.URL+"/style.css")
	cf, _ := r.Close()
	recs := readFile(t, cf)
	if len(recs) != 3 || recs[1].Type() != warcio.TypeRequest || !bytes.HasPrefix(recs[1].Block, []byte("GET /style.css HTTP/1.1")) {
		t.Fatalf("request record missing: %d records", len(recs))
	}
}

func TestDedupLocalAndExternal(t *testing.T) {
	srv := testServer()
	defer srv.Close()
	ext := mapDedup{}
	r := newRecorder(t, Options{Dedup: ext})
	c := r.Client()
	get(t, c, srv.URL+"/style.css")
	get(t, c, srv.URL+"/copy.css")
	cf, _ := r.Close()
	recs := readFile(t, cf)
	orig, rev := recs[2], recs[4]
	if orig.Type() != warcio.TypeResponse || rev.Type() != warcio.TypeRevisit {
		t.Fatalf("types %s %s", orig.Type(), rev.Type())
	}
	if rev.Header.Get(warcio.HProfile) != warcio.ProfileIdenticalPayload ||
		rev.Header.Get(warcio.HRefersTo) != orig.ID() ||
		rev.Header.Get(warcio.HRefersToTargetURI) != srv.URL+"/style.css" ||
		rev.Header.Get(warcio.HRefersToDate) != orig.Header.Get(warcio.HDate) ||
		rev.Header.Get(warcio.HPayloadDigest) != orig.Header.Get(warcio.HPayloadDigest) {
		t.Fatalf("revisit headers %+v", rev.Header)
	}
	if !bytes.HasSuffix(rev.Block, []byte("\r\n\r\n")) || bytes.Contains(rev.Block, []byte("color")) {
		t.Fatalf("revisit must hold headers only: %q", rev.Block)
	}
	if cf.Records[4].RefersTo != orig.ID() || cf.CDXJ()[1].Mime != "warc/revisit" {
		t.Fatal("record info for revisit")
	}

	ext[orig.Header.Get(warcio.HPayloadDigest)] = Ref{RecordID: "<urn:uuid:orig>", URI: "https://x/style.css", Date: time.Unix(0, 0)}
	r2 := newRecorder(t, Options{Dedup: ext})
	get(t, r2.Client(), srv.URL+"/style.css")
	cf2, _ := r2.Close()
	recs2 := readFile(t, cf2)
	if recs2[2].Type() != warcio.TypeRevisit || recs2[2].Header.Get(warcio.HRefersTo) != "<urn:uuid:orig>" {
		t.Fatal("external dedup not applied")
	}
}

func TestNotModifiedRevisit(t *testing.T) {
	srv := testServer()
	defer srv.Close()
	r := newRecorder(t, Options{})
	resp, _ := get(t, r.Client(), srv.URL+"/304", "If-None-Match", `"x"`)
	if resp.StatusCode != 304 {
		t.Fatal(resp.Status)
	}
	cf, _ := r.Close()
	rec := readFile(t, cf)[2]
	if rec.Type() != warcio.TypeRevisit || rec.Header.Get(warcio.HProfile) != warcio.ProfileServerNotModified {
		t.Fatalf("304 recorded as %s", rec.Type())
	}
}

func TestRedirectsAreSeparateRecords(t *testing.T) {
	srv := testServer()
	defer srv.Close()
	r := newRecorder(t, Options{})
	resp, body := get(t, r.Client(), srv.URL+"/redirect")
	if string(body) != "body{color:red}" || resp.Request.URL.Path != "/style.css" {
		t.Fatalf("redirect result %q %s", body, resp.Request.URL)
	}
	cf, _ := r.Close()
	var uris []string
	for _, ri := range cf.Records {
		if ri.Type == warcio.TypeResponse {
			uris = append(uris, ri.URI)
		}
	}
	if len(uris) != 2 || !strings.HasSuffix(uris[0], "/redirect") || cf.Records[2].Status != 302 {
		t.Fatalf("records %+v", cf.Records)
	}
}

func TestTruncation(t *testing.T) {
	srv := testServer()
	defer srv.Close()
	r := newRecorder(t, Options{MaxResponseSize: 1000})
	resp, body := get(t, r.Client(), srv.URL+"/big")
	if len(body) != 1000 || resp.Header.Get(HeaderTruncated) != "1" {
		t.Fatalf("body %d", len(body))
	}
	cf, _ := r.Close()
	rec := readFile(t, cf)[2]
	if rec.Header.Get(warcio.HTruncated) != "length" {
		t.Fatal("missing WARC-Truncated")
	}
}

func TestSensitiveHeadersStripped(t *testing.T) {
	srv := testServer()
	defer srv.Close()
	r := newRecorder(t, Options{})
	_, body := get(t, r.Client(), srv.URL+"/echo-headers", "Cookie", "secret=1", "Authorization", "Bearer tok")
	if string(body) != "secret=1" {
		t.Fatal("cookie should still reach the server")
	}
	cf, _ := r.Close()
	req := readFile(t, cf)[1]
	if bytes.Contains(req.Block, []byte("secret")) || bytes.Contains(req.Block, []byte("Bearer")) {
		t.Fatalf("sensitive header recorded: %q", req.Block)
	}
}

func TestBlockedAndScheme(t *testing.T) {
	r := newRecorder(t, Options{Blocked: func(h string) bool { return h == "ads.example" }})
	defer r.Abort()
	_, err := r.Client().Get("http://ads.example/x.js")
	var be *BlockedError
	if !errors.As(err, &be) {
		t.Fatalf("want BlockedError, got %v", err)
	}
	if _, err := r.Client().Get("ftp://example.com/"); err == nil {
		t.Fatal("ftp should fail")
	}
}

func TestTLS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("secure"))
	}))
	defer srv.Close()
	r := newRecorder(t, Options{InsecureTLS: true})
	_, body := get(t, r.Client(), srv.URL+"/")
	if string(body) != "secure" {
		t.Fatal(string(body))
	}
	cf, _ := r.Close()
	rec := readFile(t, cf)[2]
	if !bytes.HasPrefix(rec.Block, []byte("HTTP/1.1 200")) || !bytes.HasSuffix(rec.Block, []byte("secure")) {
		t.Fatalf("tls raw block %q", rec.Block)
	}
}

func TestResourceAndMetadata(t *testing.T) {
	r := newRecorder(t, Options{Engine: "upload"})
	ctx := context.Background()
	info, err := r.WriteResource(ctx, ResourceRecord{URI: "https://a/page", ContentType: "text/html", Body: []byte("<p>x</p>")})
	if err != nil || info.Type != warcio.TypeResource {
		t.Fatal(err)
	}
	dup, err := r.WriteResource(ctx, ResourceRecord{URI: "https://a/page2", ContentType: "text/html", Body: []byte("<p>x</p>")})
	if err != nil || dup.Type != warcio.TypeRevisit || dup.RefersTo != info.ID {
		t.Fatalf("resource dedup %+v %v", dup, err)
	}
	if _, err := r.WriteMetadata(PageInfo{URL: "https://a/page", RecordID: info.ID, Title: "T", Text: "body"}); err != nil {
		t.Fatal(err)
	}
	cf, err := r.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(cf.Pages) != 1 || cf.Pages[0].Engine != "upload" || cf.Pages[0].Source != "test source" {
		t.Fatalf("pages %+v", cf.Pages)
	}
	recs := readFile(t, cf)
	meta := recs[3]
	if meta.Type() != warcio.TypeMetadata || meta.Header.Get(warcio.HRefersTo) != info.ID ||
		!bytes.Contains(meta.Block, []byte(`"title":"T"`)) {
		t.Fatalf("metadata %s %q", meta.Type(), meta.Block)
	}
	if len(cf.CDXJ()) != 2 {
		t.Fatal("metadata must not be in CDXJ")
	}
	if _, err := r.WriteMetadata(PageInfo{}); err == nil {
		t.Fatal("write after close")
	}
}

func TestAbortRemovesFile(t *testing.T) {
	r := newRecorder(t, Options{})
	r.Abort()
	if _, err := os.Stat(r.path); !os.IsNotExist(err) {
		t.Fatal("spool file left behind")
	}
}

func TestDecode(t *testing.T) {
	if b, ok := Decode("gzip", gzipBytes("abc")); !ok || string(b) != "abc" {
		t.Fatal("gzip")
	}
	if b, ok := Decode("gzip", []byte("notgzip")); ok || string(b) != "notgzip" {
		t.Fatal("bad gzip should pass through")
	}
	if _, ok := Decode("", []byte("x")); ok {
		t.Fatal("identity")
	}
}

// TestWarcioCheck validates output with the independent Python warcio
// implementation when it is installed.
func TestWarcioCheck(t *testing.T) {
	bin, err := exec.LookPath("warcio")
	if err != nil {
		t.Skip("warcio not installed")
	}
	srv := testServer()
	defer srv.Close()
	r := newRecorder(t, Options{})
	c := r.Client()
	for _, p := range []string{"/gzip", "/chunked", "/style.css", "/copy.css", "/redirect", "/304"} {
		get(t, c, srv.URL+p)
	}
	r.WriteResource(context.Background(), ResourceRecord{URI: "https://a/x", ContentType: "text/plain", Body: []byte("x")})
	r.WriteMetadata(PageInfo{URL: srv.URL + "/gzip", Title: "t"})
	cf, _ := r.Close()
	out, err := exec.Command(bin, "check", "-v", cf.Path).CombinedOutput()
	if err != nil || bytes.Contains(out, []byte("digest failed")) {
		t.Fatalf("warcio check failed: %v\n%s", err, out)
	}
	out, err = exec.Command(bin, "index", "-f", "warc-type,warc-target-uri,offset,length", cf.Path).CombinedOutput()
	if err != nil {
		t.Fatalf("warcio index: %v\n%s", err, out)
	}
	for _, ri := range cf.Records {
		if !bytes.Contains(out, []byte(fmt.Sprintf(`"offset": "%d", "length": "%d"`, ri.Offset, ri.Length))) {
			t.Errorf("warcio sees different offsets for %s:\n%s", ri.Type, out)
		}
	}
}

func TestScanMatchesRecorder(t *testing.T) {
	srv := testServer()
	defer srv.Close()
	r := newRecorder(t, Options{})
	c := r.Client()
	for _, p := range []string{"/gzip", "/style.css", "/copy.css", "/304"} {
		get(t, c, srv.URL+p)
	}
	info, _ := r.WriteResource(context.Background(), ResourceRecord{URI: "https://a/x", ContentType: "text/plain; charset=utf-8", Body: []byte("x")})
	r.WriteMetadata(PageInfo{URL: srv.URL + "/gzip", Title: "t", Text: "body", RecordID: info.ID})
	cf, _ := r.Close()

	scanned, err := Scan(mustOpen(t, cf.Path), cf.Name, cf.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	if scanned.Size != cf.Size || len(scanned.Records) != len(cf.Records) {
		t.Fatalf("size %d/%d records %d/%d", scanned.Size, cf.Size, len(scanned.Records), len(cf.Records))
	}
	for i := range cf.Records {
		a, b := cf.Records[i], scanned.Records[i]
		a.Date, b.Date = a.Date.Truncate(time.Second), b.Date.Truncate(time.Second)
		if a != b {
			t.Errorf("record %d:\n recorded %+v\n scanned  %+v", i, a, b)
		}
	}
	if len(scanned.Pages) != 1 || scanned.Pages[0].MetadataID != cf.Pages[0].MetadataID || scanned.Pages[0].Text != "body" {
		t.Fatalf("pages %+v", scanned.Pages)
	}
}

func TestMetadataContentTypeIncludesFormerName(t *testing.T) {
	for _, ct := range []string{MetadataContentType,
		`application/json; profile="https://github.com/ogefest/bindery/capture-metadata"`} {
		if !IsMetadataContentType(ct) {
			t.Errorf("%s not recognized", ct)
		}
	}
	if IsMetadataContentType("application/json") {
		t.Error("plain JSON taken for capture metadata")
	}
}
