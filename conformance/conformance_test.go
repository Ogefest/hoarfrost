package conformance

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ogefest/hoarfrost/internal/cdxj"
	"github.com/ogefest/hoarfrost/internal/upload"
	"github.com/ogefest/hoarfrost/internal/wacz"
	"github.com/ogefest/hoarfrost/internal/warcio"
)

// TestWarcioCheck validates record structure, block and payload digests of
// every WARC file in the package with warcio.
func TestWarcioCheck(t *testing.T) {
	f := getFixture(t)
	for _, w := range f.warcs {
		out := run(t, "warcio", "check", "-v", w)
		for _, bad := range []string{"digest failed", "invalid", "error"} {
			if bytes.Contains(bytes.ToLower(out), []byte(bad)) {
				t.Fatalf("warcio check %s reports %q:\n%s", filepath.Base(w), bad, out)
			}
		}
		if !bytes.Contains(out, []byte("digest pass")) {
			t.Fatalf("warcio verified nothing in %s:\n%s", filepath.Base(w), out)
		}
	}
}

type warcioEntry struct {
	Type   string `json:"warc-type"`
	URI    string `json:"warc-target-uri"`
	ID     string `json:"warc-record-id"`
	Offset string `json:"offset"`
	Length string `json:"length"`
	Digest string `json:"warc-payload-digest"`
}

// ourCDXJ returns the index lines of all volumes of the week.
func ourCDXJ(t *testing.T, f *fixture) []cdxj.Line {
	var all []cdxj.Line
	for _, p := range f.pkgs {
		r, err := wacz.OpenFile(p)
		if err != nil {
			t.Fatal(err)
		}
		lines, err := r.CDXJ()
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, lines...)
	}
	return all
}

// TestWarcioIndexMatchesCDXJ compares warcio's record positions with the
// package index: same records, offsets, lengths and digests.
func TestWarcioIndexMatchesCDXJ(t *testing.T) {
	f := getFixture(t)
	byID := map[string]warcioEntry{}
	byFile := map[string]string{}
	for _, w := range f.warcs {
		out := run(t, "warcio", "index", "-f", "warc-type,warc-target-uri,warc-record-id,offset,length,warc-payload-digest", w)
		sc := bufio.NewScanner(bytes.NewReader(out))
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		for sc.Scan() {
			var e warcioEntry
			if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
				t.Fatalf("warcio index output: %v", err)
			}
			byID[e.ID] = e
			byFile[e.ID] = filepath.Base(w)
		}
	}
	lines := ourCDXJ(t, f)
	indexed := map[string]bool{}
	for _, l := range lines {
		e, ok := byID[l.RecordID]
		if !ok {
			t.Fatalf("record %s (%s) unknown to warcio", l.RecordID, l.URL)
		}
		if e.Offset != l.Offset || e.Length != l.Length || byFile[l.RecordID] != l.Filename {
			t.Errorf("%s: warcio %s@%s+%s, cdxj %s@%s+%s", l.URL, byFile[l.RecordID], e.Offset, e.Length, l.Filename, l.Offset, l.Length)
		}
		if e.Digest != l.Digest || e.URI != l.URL || e.Type != l.Type {
			t.Errorf("%s: warcio %s %s %s, cdxj %s %s %s", l.URL, e.Type, e.URI, e.Digest, l.Type, l.URL, l.Digest)
		}
		indexed[l.RecordID] = true
	}
	for id, e := range byID {
		switch e.Type {
		case "response", "revisit", "resource":
			if !indexed[id] {
				t.Errorf("%s record %s (%s) missing from the package index", e.Type, id, e.URI)
			}
		}
	}
}

// TestCDXJIndexerAgrees indexes the WARC files with cdxj-indexer (the
// indexer used by pywb and py-wacz) and requires the same keys, timestamps
// and positions as the package index, so lookups by other tools find
// exactly the records hoarfrost indexed.
func TestCDXJIndexerAgrees(t *testing.T) {
	f := getFixture(t)
	type key struct{ file, offset string }
	theirs := map[key]cdxj.Line{}
	for _, w := range f.warcs {
		out := run(t, "cdxj-indexer", w)
		lines, err := cdxj.ReadAll(bytes.NewReader(out))
		if err != nil {
			t.Fatalf("parse cdxj-indexer output: %v\n%s", err, out)
		}
		for _, l := range lines {
			theirs[key{filepath.Base(w), l.Offset}] = l
		}
	}
	for _, ours := range ourCDXJ(t, f) {
		other, ok := theirs[key{ours.Filename, ours.Offset}]
		if !ok {
			t.Errorf("cdxj-indexer has no record at %s:%s (%s %s)", ours.Filename, ours.Offset, ours.Type, ours.URL)
			continue
		}
		if other.SURT != ours.SURT {
			t.Errorf("SURT differs for %s: cdxj-indexer %q, hoarfrost %q", ours.URL, other.SURT, ours.SURT)
		}
		if !other.Timestamp.Equal(ours.Timestamp) || other.Length != ours.Length || other.URL != ours.URL {
			t.Errorf("%s: cdxj-indexer %v len %s, hoarfrost %v len %s", ours.URL, other.Timestamp, other.Length, ours.Timestamp, ours.Length)
		}
		if other.Mime != ours.Mime || other.Status != ours.Status {
			t.Errorf("%s: cdxj-indexer mime %q status %q, hoarfrost %q %q", ours.URL, other.Mime, other.Status, ours.Mime, ours.Status)
		}
		if strings.TrimPrefix(other.Digest, "sha1:") != strings.TrimPrefix(ours.Digest, "sha1:") {
			t.Errorf("%s: digest %s vs %s", ours.URL, other.Digest, ours.Digest)
		}
	}
}

// TestPyWACZValidate runs the reference WACZ validator on every volume.
func TestPyWACZValidate(t *testing.T) {
	f := getFixture(t)
	for _, p := range f.pkgs {
		out := run(t, "wacz", "validate", "-f", p)
		t.Logf("%s: %s", filepath.Base(p), out)
	}
}

// TestZipAndChecksums checks with standard tools that every entry of every
// volume is stored uncompressed, its index is sorted, and SHA256SUMS
// verifies the volumes.
func TestZipAndChecksums(t *testing.T) {
	f := getFixture(t)
	for _, p := range f.pkgs {
		out := run(t, "zipinfo", "-l", p)
		n := 0
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 8 && (strings.HasPrefix(fields[0], "-") || strings.HasPrefix(fields[0], "d")) {
				n++
				if fields[6] != "stor" {
					t.Errorf("%s: entry %s uses %s, must be stored", filepath.Base(p), fields[len(fields)-1], fields[6])
				}
			}
		}
		if n < 5 {
			t.Fatalf("zipinfo listed %d entries:\n%s", n, out)
		}
		idx := run(t, "unzip", "-p", p, wacz.PathCDXJ)
		sort := exec.Command(requireTool(t, "sort"), "-c")
		sort.Env = append(os.Environ(), "LC_ALL=C")
		sort.Stdin = bytes.NewReader(idx)
		if out, err := sort.CombinedOutput(); err != nil {
			t.Fatalf("%s: LC_ALL=C sort -c: %v\n%s", filepath.Base(p), err, out)
		}
	}
	cmd := exec.Command(requireTool(t, "sha256sum"), "-c", "SHA256SUMS")
	cmd.Dir = filepath.Join(f.root, filepath.FromSlash(f.epoch.VolumeDir()))
	out, err := cmd.CombinedOutput()
	if err != nil || strings.Count(string(out), ": OK") != len(f.pkgs) {
		t.Fatalf("sha256sum -c SHA256SUMS: %v\n%s", err, out)
	}
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// startPywb loads the WARC files into a pywb collection and serves it.
func startPywb(t *testing.T, warcs []string) string {
	t.Helper()
	requireTool(t, "wayback")
	dir := t.TempDir()
	wb := exec.Command(requireTool(t, "wb-manager"), "init", "arch")
	wb.Dir = dir
	if out, err := wb.CombinedOutput(); err != nil {
		t.Fatalf("wb-manager init: %v\n%s", err, out)
	}
	add := exec.Command("wb-manager", append([]string{"add", "arch"}, warcs...)...)
	add.Dir = dir
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatalf("wb-manager add: %v\n%s", err, out)
	}
	port := freePort(t)
	srv := exec.Command("wayback", "-p", strconv.Itoa(port), "-b", "127.0.0.1")
	srv.Dir = dir
	var logBuf bytes.Buffer
	srv.Stdout, srv.Stderr = &logBuf, &logBuf
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		srv.Process.Kill()
		srv.Wait()
		if t.Failed() {
			t.Logf("pywb log:\n%s", logBuf.String())
		}
	})
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	for range 100 {
		if resp, err := http.Get(base + "/"); err == nil {
			resp.Body.Close()
			return base + "/arch"
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("pywb did not start")
	return ""
}

func get(t *testing.T, url string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// TestPywbReplay replays the week with pywb from the WARC files of all its
// volumes in one collection: pages, a PDF byte for byte, a redirect, and
// resources stored as revisits in another volume than their original,
// which pywb must resolve using WARC-Refers-To / WARC-Payload-Digest.
func TestPywbReplay(t *testing.T) {
	f := getFixture(t)
	base := startPywb(t, f.warcs)
	ctx := context.Background()

	code, body := get(t, base+"/mp_/"+f.site.URL+"/news/article.html")
	if code != http.StatusOK || !bytes.Contains(body, []byte("Radni przyjęli budżet miasta")) {
		t.Fatalf("article replay: %d", code)
	}
	code, body = get(t, base+"/id_/"+f.site.URL+"/files/uchwala.pdf")
	if code != http.StatusOK || !bytes.HasPrefix(body, []byte("%PDF-")) {
		t.Fatalf("pdf replay: %d %q", code, body[:min(len(body), 200)])
	}

	revisits := 0
	recs, err := f.idx.EpochRecords(ctx, "", f.epoch)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.Type != warcio.TypeRevisit || !strings.HasSuffix(r.URI, "/css/main.css") {
			continue
		}
		revisits++
		if orig, err := f.idx.Record(ctx, r.RefersTo); err != nil || orig.Location == r.Location {
			t.Fatalf("revisit %s should point into another volume: %+v %v", r.ID, orig, err)
		}
		ts := r.Date.UTC().Format("20060102150405")
		code, body := get(t, base+"/"+ts+"id_/"+r.URI)
		if code != http.StatusOK || !bytes.Equal(body, f.cssBody) {
			t.Fatalf("revisit of %s at %s replayed as %d %q", r.URI, ts, code, body)
		}
	}
	if revisits == 0 {
		t.Fatal("fixture has no revisit of the shared stylesheet")
	}

	code, body = get(t, base+"/mp_/"+f.site.URL+"/redirect")
	if code != http.StatusOK || !bytes.Contains(body, []byte("Radni przyjęli budżet miasta")) {
		t.Fatalf("archived redirect not followed in replay: %d", code)
	}
	code, _ = get(t, base+"/mp_/https://example.org/z-przegladarki")
	if code != http.StatusOK {
		t.Fatalf("browser upload (resource record) replay: %d", code)
	}
}

// TestForeignToolsRoundTrip goes the other way: files rewritten or packaged
// by other tools must be readable by hoarfrost.
func TestForeignToolsRoundTrip(t *testing.T) {
	f := getFixture(t)
	dir := t.TempDir()
	// The file holding the article: py-wacz must detect a page in it, which
	// the PDF or the 404 capture would not give.
	var src string
	for _, w := range f.warcs {
		for _, r := range readAll(t, w) {
			if r.Type() == warcio.TypeResponse && strings.HasSuffix(r.Header.Get(warcio.HTargetURI), "/news/article.html") {
				src = w
			}
		}
	}
	if src == "" {
		t.Fatal("fixture has no capture of the article")
	}
	recompressed := filepath.Join(dir, "recompressed.warc.gz")
	run(t, "warcio", "recompress", src, recompressed)
	orig := readAll(t, src)
	again := readAll(t, recompressed)
	if len(orig) != len(again) {
		t.Fatalf("warcio recompress: %d records, original %d", len(again), len(orig))
	}
	for i := range orig {
		if orig[i].ID() != again[i].ID() || !bytes.Equal(orig[i].Block, again[i].Block) {
			t.Fatalf("record %d differs after warcio recompress", i)
		}
	}

	pkg := filepath.Join(dir, "foreign.wacz")
	run(t, "wacz", "create", "-o", pkg, "--detect-pages", src)
	r, err := wacz.OpenFile(pkg)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	lines, err := r.CDXJ()
	if err != nil || len(lines) == 0 {
		t.Fatalf("index of a py-wacz package: %d lines, %v", len(lines), err)
	}
	if _, err := r.Pages(); err != nil {
		t.Fatalf("pages of a py-wacz package: %v", err)
	}
	body, _ := os.ReadFile(pkg)
	rec := newRecorder(t)
	res, err := upload.Store(context.Background(), rec, upload.Upload{Body: body, Filename: "foreign.wacz"})
	if err != nil {
		t.Fatalf("upload of a py-wacz package: %v", err)
	}
	if len(res.Pages) == 0 {
		t.Fatal("no pages imported from a py-wacz package")
	}
}

func readAll(t *testing.T, path string) []*warcio.Record {
	f, err := os.Open(path)
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

// TestExitScript runs scripts/exit-test.sh: reading an article with
// unzip, dd, gunzip, warcio and pywb only.
func TestExitScript(t *testing.T) {
	f := getFixture(t)
	for _, tool := range []string{"unzip", "warcio", "wb-manager", "wayback", "shuf", "dd"} {
		requireTool(t, tool)
	}
	cmd := exec.Command("bash", "../scripts/exit-test.sh", f.root)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("exit test: %v\n%s", err, out)
	}
}
