package legacy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/recorder"
)

type fakeRecorder struct {
	dir   string
	files []recorder.CaptureFile
}

func (f *fakeRecorder) Record(ctx context.Context, _ string, ep epoch.ID, source, eng string, fn func(*recorder.Recorder) error) (recorder.CaptureFile, error) {
	r, err := recorder.New(recorder.Options{Epoch: ep, SpoolDir: f.dir, Source: source, Engine: eng})
	if err != nil {
		return recorder.CaptureFile{}, err
	}
	if err := fn(r); err != nil {
		return recorder.CaptureFile{}, err
	}
	cf, err := r.Close()
	f.files = append(f.files, cf)
	return cf, err
}

func TestImport(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string, mtime time.Time) {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
		os.Chtimes(p, mtime, mtime)
	}
	d1 := time.Date(2024, 3, 5, 10, 0, 0, 0, time.UTC)
	d2 := time.Date(2024, 3, 20, 10, 0, 0, 0, time.UTC)
	write("2024-03-05/a.html", `<html><head><link rel="canonical" href="https://serwis.pl/a"><title>A</title></head><body><p>Pierwszy</p><img src="data:image/png;base64,AA=="></body></html>`, d1)
	write("2024-03-05/b.htm", `<html><head><meta property="og:url" content="https://serwis.pl/b"><title>B</title></head><body>Drugi</body></html>`, d1)
	write("2024-03-20/c.html", `<html><head><title>C</title></head><body>Trzeci</body></html>`, d2)
	write("notes.txt", "ignored", d2)

	fr := &fakeRecorder{dir: t.TempDir()}
	var progress []int
	rep, err := Import(context.Background(), fr, dir, Options{BatchSize: 1, Progress: func(d, _ int) { progress = append(progress, d) },
		Writable: func(_ context.Context, ep epoch.ID) bool { return ep != epoch.Of(d2) },
		Now:      func() time.Time { return time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Pages != 3 || rep.Files != 3 || len(rep.Errors) != 0 || len(progress) != 3 {
		t.Fatalf("%+v", rep)
	}
	urls := map[string]epoch.ID{}
	for _, f := range fr.files {
		for _, p := range f.Pages {
			urls[p.URL] = f.Epoch
			if p.Source != Source || p.Text == "" {
				t.Fatalf("page %+v", p)
			}
		}
	}
	if urls["https://serwis.pl/a"] != "2024-W10" || urls["https://serwis.pl/b"] != "2024-W10" {
		t.Fatalf("%v", urls)
	}
	if urls["urn:legacy:2024-03-20/c.html"] != "2026-W41" {
		t.Fatalf("sealed epoch must redirect to current: %v", urls)
	}
}

func TestPageURL(t *testing.T) {
	if PageURL([]byte(`<link rel="canonical" href="javascript:x">`)) != "" {
		t.Fatal("non-http canonical accepted")
	}
}
