package wacz_test

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/ogefest/hoarfrost/internal/cdxj"
	"github.com/ogefest/hoarfrost/internal/engine"
	"github.com/ogefest/hoarfrost/internal/engine/static"
	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/recorder"
	"github.com/ogefest/hoarfrost/internal/testsite"
	"github.com/ogefest/hoarfrost/internal/wacz"
	"github.com/ogefest/hoarfrost/internal/warcio"
)

func captures(t *testing.T) []wacz.Input {
	t.Helper()
	site := testsite.New()
	t.Cleanup(site.Close)
	committed := map[string]recorder.Ref{}
	var inputs []wacz.Input
	jobs := []struct{ path, source string }{
		{"/news/article.html", "serwis-a"},
		{"/news/article2.html", "serwis-a"},
		{"/files/uchwala.pdf", "serwis-b"},
	}
	for _, j := range jobs {
		rec, err := recorder.New(recorder.Options{
			Epoch: "2026-W41", SpoolDir: t.TempDir(), Source: j.source, Dedup: lookupFunc(committed),
			Blocked: func(h string) bool { return static.HostMatches(h, []string{"doubleclick.net", "youtube.com"}) },
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = static.Engine{}.Capture(context.Background(), engine.Job{URL: site.URL + j.path, Source: j.source,
			Options: engine.Options{SkipFrames: []string{"youtube.com"}}}, rec)
		if err != nil {
			t.Fatal(err)
		}
		cf, err := rec.Close()
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range cf.Records {
			if r.Type == warcio.TypeResponse {
				if _, ok := committed[r.Digest]; !ok {
					committed[r.Digest] = recorder.Ref{RecordID: r.ID, URI: r.URI, Date: r.Date}
				}
			}
		}
		path := cf.Path
		inputs = append(inputs, wacz.Input{Capture: cf, Open: func(context.Context) (io.ReadCloser, error) { return os.Open(path) }})
	}
	return inputs
}

type lookupFunc map[string]recorder.Ref

func (m lookupFunc) Lookup(_ context.Context, _ epoch.ID, d string) (*recorder.Ref, error) {
	if r, ok := m[d]; ok {
		return &r, nil
	}
	return nil, nil
}

func build(t *testing.T, inputs []wacz.Input, opts wacz.Options) *wacz.Result {
	t.Helper()
	opts.Epoch = "2026-W41"
	opts.Software = "hoarfrost/test"
	res, err := wacz.Build(context.Background(), filepath.Join(t.TempDir(), "W41.wacz"), inputs, opts)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// TestAbsoluteOffsetsMatchSequentialRead is property 14.4: every record read
// through its absolute offset equals the record found by iterating the
// package's WARC files sequentially.
func TestAbsoluteOffsetsMatchSequentialRead(t *testing.T) {
	inputs := captures(t)
	for _, concat := range []bool{false, true} {
		t.Run("concat="+strconv.FormatBool(concat), func(t *testing.T) {
			res := build(t, inputs, wacz.Options{ConcatBySource: concat})
			r, err := wacz.OpenFile(res.Path)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if concat && len(res.Files) != 2 {
				t.Fatalf("files %v", res.Files)
			}
			sequential := map[string]*warcio.Record{}
			for _, name := range res.Files {
				sec, err := r.Entry(wacz.ArchiveDir + name)
				if err != nil {
					t.Fatal(err)
				}
				recs, err := warcio.ReadAll(sec)
				if err != nil {
					t.Fatal(err)
				}
				for _, rec := range recs {
					sequential[rec.ID()] = rec
				}
			}
			if len(res.Records) == 0 {
				t.Fatal("no records")
			}
			for id, loc := range res.Records {
				got, err := r.ReadRecord(loc.Offset, loc.Length)
				if err != nil {
					t.Fatalf("%s: %v", id, err)
				}
				want := sequential[id]
				if want == nil || got.ID() != id || !bytes.Equal(got.Block, want.Block) {
					t.Fatalf("record %s differs", id)
				}
			}

			lines, err := r.CDXJ()
			if err != nil || len(lines) != len(res.Records)-res.Pages {
				t.Fatalf("cdxj %d lines, %d records: %v", len(lines), len(res.Records), err)
			}
			pages, _ := r.Pages()
			for _, p := range pages {
				rec, err := r.ReadRecord(offsetsOf(r)[p.Filename]+p.MetaOffset, p.MetaLength)
				if err != nil || rec.ID() != p.MetadataID || res.Records[p.MetadataID].Length != p.MetaLength {
					t.Fatalf("metadata record of %s not at its offset: %v", p.URL, err)
				}
			}
			offsets, _ := r.WARCOffsets()
			for _, l := range lines {
				base, ok := offsets[l.Filename]
				if !ok {
					t.Fatalf("cdxj filename %s not in package", l.Filename)
				}
				off, _ := strconv.ParseInt(l.Offset, 10, 64)
				if loc := res.Records[l.RecordID]; loc.Offset != base+off {
					t.Fatalf("cdxj offset for %s: %d+%d != %d", l.URL, base, off, loc.Offset)
				}
			}
			if probs := r.Verify(); len(probs) != 0 {
				t.Fatalf("verify: %+v", probs)
			}
			sum, _ := wacz.FileSHA256(res.Path)
			if sum != res.SHA256 {
				t.Fatal("sha256")
			}
		})
	}
}

func TestPagesAndDatapackage(t *testing.T) {
	inputs := captures(t)
	res := build(t, inputs, wacz.Options{Title: "2026-W41"})
	r, _ := wacz.OpenFile(res.Path)
	defer r.Close()
	pages, err := r.Pages()
	if err != nil || len(pages) != 3 || res.Pages != 3 {
		t.Fatalf("pages %d %v", len(pages), err)
	}
	for _, p := range pages {
		if p.MetadataID == "" || p.Filename == "" || p.Captured.IsZero() || p.URL == "" {
			t.Fatalf("page %+v", p)
		}
	}
	dp, err := r.Datapackage()
	if err != nil || dp.WACZVersion != "1.1.1" || dp.Profile != "data-package" || len(dp.Resources) != 5 || dp.Epoch != "2026-W41" {
		t.Fatalf("datapackage %+v %v", dp, err)
	}
	raw, _ := r.ReadEntry(wacz.PathPages)
	if !bytes.HasPrefix(raw, []byte(`{"format":"json-pages-1.0"`)) {
		t.Fatalf("pages header %s", raw[:60])
	}
	idx, _ := r.ReadEntry(wacz.PathCDXJ)
	if ok, _ := cdxj.IsSorted(bytes.NewReader(idx)); !ok {
		t.Fatal("cdxj not sorted")
	}
}

func TestZipNum(t *testing.T) {
	inputs := captures(t)
	res := build(t, inputs, wacz.Options{ZipNumThreshold: 5})
	if !res.ZipNum {
		t.Fatal("expected ZipNum index")
	}
	r, _ := wacz.OpenFile(res.Path)
	defer r.Close()
	lines, err := r.CDXJ()
	if err != nil || len(lines) != res.CDXJLines {
		t.Fatalf("zipnum lines %d %v", len(lines), err)
	}
	idx, _ := r.ReadEntry(wacz.PathZipNumIdx)
	if !bytes.HasPrefix(idx, []byte("!meta 0 ")) {
		t.Fatalf("idx %s", idx)
	}
	if probs := r.Verify(); len(probs) != 0 {
		t.Fatalf("verify: %+v", probs)
	}
}

func TestVerifyDetectsCorruption(t *testing.T) {
	inputs := captures(t)
	res := build(t, inputs, wacz.Options{})
	data, _ := os.ReadFile(res.Path)
	r0, _ := wacz.OpenFile(res.Path)
	pagesOff := findEntryOffset(t, r0, wacz.PathPages)
	r0.Close()
	data[pagesOff+5] ^= 0xff
	bad := filepath.Join(t.TempDir(), "bad.wacz")
	os.WriteFile(bad, data, 0o644)
	r, err := wacz.OpenFile(bad)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	probs := r.Verify()
	if len(probs) != 1 || probs[0].Path != wacz.PathPages {
		t.Fatalf("problems %+v", probs)
	}
}

func findEntryOffset(t *testing.T, r *wacz.Reader, name string) int64 {
	for _, f := range r.Files() {
		if f.Name == name {
			off, err := f.DataOffset()
			if err != nil {
				t.Fatal(err)
			}
			return off
		}
	}
	t.Fatalf("%s not found", name)
	return 0
}

func TestSourceFromName(t *testing.T) {
	cases := map[string]string{
		"20261011T070200Z-serwis_a-a1b2c3.warc.gz": "serwis_a",
		"20261011T070200Z-a_b_c-ff.warc.gz":        "a_b_c",
		"weird.warc.gz":                            "capture",
	}
	for in, want := range cases {
		if got := wacz.SourceFromName(in); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
}

// TestPyWACZValidate validates the package with the independent py-wacz tool.
func TestPyWACZValidate(t *testing.T) {
	bin, err := exec.LookPath("wacz")
	if err != nil {
		t.Skip("py-wacz not installed")
	}
	res := build(t, captures(t), wacz.Options{})
	out, err := exec.Command(bin, "validate", "-f", res.Path).CombinedOutput()
	if err != nil {
		t.Fatalf("wacz validate: %v\n%s", err, out)
	}
}

func offsetsOf(r *wacz.Reader) map[string]int64 {
	o, _ := r.WARCOffsets()
	return o
}

// TestZIP64 builds a package larger than 4 GiB (HOARFROST_TEST_LARGE set) and
// reads the last record through its absolute offset.
func TestZIP64(t *testing.T) {
	if os.Getenv("HOARFROST_TEST_LARGE") == "" {
		t.Skip("set HOARFROST_TEST_LARGE to build a package over 4 GiB")
	}
	dir := t.TempDir()
	rec, err := recorder.New(recorder.Options{Epoch: "2026-W41", SpoolDir: dir, Source: "big"})
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 64<<20)
	var last recorder.RecordInfo
	for i := 0; i < 70; i++ {
		crand.Read(chunk)
		last, err = rec.WriteResource(context.Background(), recorder.ResourceRecord{URI: fmt.Sprintf("https://big.example/%d", i), ContentType: "application/octet-stream", Body: chunk})
		if err != nil {
			t.Fatal(err)
		}
	}
	cf, err := rec.Close()
	if err != nil {
		t.Fatal(err)
	}
	if cf.Size < 1<<32 {
		t.Fatalf("capture only %d bytes", cf.Size)
	}
	path := cf.Path
	res, err := wacz.Build(context.Background(), filepath.Join(dir, "big.wacz"),
		[]wacz.Input{{Capture: cf, Open: func(context.Context) (io.ReadCloser, error) { return os.Open(path) }}}, wacz.Options{Epoch: "2026-W41"})
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(path)
	r, err := wacz.OpenFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	loc := res.Records[last.ID]
	if loc.Offset < 1<<32 {
		t.Fatalf("last record at %d, expected beyond 4 GiB", loc.Offset)
	}
	got, err := r.ReadRecord(loc.Offset, loc.Length)
	if err != nil || got.ID() != last.ID {
		t.Fatalf("read beyond 4 GiB: %v", err)
	}
	if !bytes.Equal(got.Block, chunk) {
		t.Fatal("content mismatch")
	}
	if probs := r.Verify(); len(probs) != 0 {
		t.Fatalf("verify: %+v", probs)
	}
}
