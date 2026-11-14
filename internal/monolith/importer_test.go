package monolith

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/recorder"
	"github.com/ogefest/hoarfrost/internal/warcio"
)

type mapOpener map[string][]byte

func (mapOpener) Prepare(context.Context, []Item) error { return nil }
func (m mapOpener) Open(it Item) ([]byte, error) {
	if b, ok := m[it.Path]; ok {
		return b, nil
	}
	return nil, os.ErrNotExist
}

func TestImport(t *testing.T) {
	logo := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("LOGO"), 1000))
	ep := epoch.MustParse("2026-W42")
	t0 := ep.Start().Add(10 * time.Hour)
	files := mapOpener{}
	var items []Item
	for i := range 5 {
		p := fmt.Sprintf("20261013/1000%02d_%d.html", i, i)
		photo := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{byte(i)}, 3000))
		files[p] = []byte(`<html><head><title>Article ` + fmt.Sprint(i) + `</title></head><body><img src="data:image/png;base64,` + logo +
			`"><p>Text of the article number ` + fmt.Sprint(i) + `.</p><img src="data:image/jpeg;base64,` + photo + `"></body></html>`)
		items = append(items, Item{ID: fmt.Sprint(i), URL: fmt.Sprintf("https://news.example/a/%d", i), Captured: t0.Add(time.Duration(i) * time.Minute), Path: p, Published: "2026-10-12"})
	}
	items = append(items, Item{ID: "dup", URL: items[0].URL, Captured: t0.Add(time.Hour), Path: items[0].Path})
	items = append(items, Item{ID: "missing", URL: "https://news.example/missing", Captured: t0.Add(time.Hour), Path: "20261013/nope.html"})
	root := t.TempDir()
	o := Options{Root: root, Spool: filepath.Join(root, ".spool"), Epoch: ep, Source: "press_media", BatchSize: 2, Workers: 2}

	rep, err := Import(context.Background(), append([]Item(nil), items[:3]...), files, o)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Pages != 3 || rep.Resources != 4 || rep.Reused != 2 {
		t.Fatalf("first run: %+v", rep)
	}
	// Resume with all items: the first three are done.
	rep, err = Import(context.Background(), append([]Item(nil), items...), files, o)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Resumed != 3 || rep.Pages != 2 || rep.Skipped != 1 || len(rep.Errors) != 1 || rep.Resources != 2 || rep.Reused != 2 {
		t.Fatalf("resume: %+v", rep)
	}

	dir := filepath.Join(root, filepath.FromSlash(ep.IncomingPrefix()))
	entries, _ := os.ReadDir(dir)
	var warcs, idx int
	logoRecords := 0
	pages := map[string]recorder.CapturedPage{}
	for _, e := range entries {
		switch {
		case strings.HasSuffix(e.Name(), ".warc.gz"):
			warcs++
			f, _ := os.Open(filepath.Join(dir, e.Name()))
			cf, err := recorder.Scan(bufio.NewReader(f), e.Name(), ep)
			f.Close()
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range cf.Records {
				if r.Type == warcio.TypeResource && strings.HasSuffix(r.URI, ".png") {
					logoRecords++
				}
			}
			for _, p := range cf.Pages {
				pages[p.URL] = p
			}
		case strings.HasSuffix(e.Name(), ".cdxj"):
			idx++
		}
	}
	if warcs != 3 || idx != 3 {
		t.Errorf("files: %d warc, %d cdxj", warcs, idx)
	}
	if logoRecords != 1 {
		t.Errorf("logo stored %d times", logoRecords)
	}
	p := pages["https://news.example/a/4"]
	if p.Extra[idKey] != "4" || p.Published != "2026-10-12" || p.Source != "press_media" || p.Engine != "monolith" || !strings.Contains(p.Text, "number 4") {
		t.Errorf("page metadata: %+v", p.PageInfo)
	}
	if !p.Captured.Equal(t0.Add(4 * time.Minute)) {
		t.Errorf("captured %s", p.Captured)
	}
	if left, _ := os.ReadDir(o.Spool); len(left) != 0 {
		t.Errorf("spool not empty: %d", len(left))
	}
}

func TestImportRefusesOtherWeek(t *testing.T) {
	ep := epoch.MustParse("2026-W42")
	_, err := Import(context.Background(), []Item{{ID: "1", Captured: ep.End()}}, mapOpener{}, Options{Root: t.TempDir(), Epoch: ep})
	if err == nil {
		t.Fatal("item from another week accepted")
	}
}

type archiveOpener struct {
	mu                  sync.Mutex
	running, maxRunning int
	prepared, released  map[string]bool
}

func (a *archiveOpener) Prepare(_ context.Context, items []Item) error {
	a.mu.Lock()
	a.running++
	a.maxRunning = max(a.maxRunning, a.running)
	a.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	a.mu.Lock()
	a.running--
	for _, it := range items {
		a.prepared[it.ID] = true
	}
	a.mu.Unlock()
	return nil
}

func (a *archiveOpener) Open(it Item) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.prepared[it.ID] {
		return nil, fmt.Errorf("%s opened before its archive was prepared", it.ID)
	}
	return []byte("<html><title>" + it.ID + "</title><p>body " + it.ID + "</p></html>"), nil
}

func (a *archiveOpener) Release(items []Item) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, it := range items {
		a.released[it.ID] = true
	}
}

func TestImportPipeline(t *testing.T) {
	ep := epoch.MustParse("2025-W04")
	var items []Item
	for i := range 40 {
		items = append(items, Item{ID: fmt.Sprint(i), URL: fmt.Sprintf("https://x.example/%d", i), Captured: ep.Start().Add(time.Duration(i) * time.Minute),
			Path: fmt.Sprintf("d/%d.html", i), Archive: fmt.Sprintf("2024%04d.7z", i%10)})
	}
	op := &archiveOpener{prepared: map[string]bool{}, released: map[string]bool{}}
	root := t.TempDir()
	rep, err := Import(context.Background(), items, op, Options{Root: root, Spool: filepath.Join(root, ".spool"), Epoch: ep, BatchSize: 7, Workers: 4, Extractors: 3})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Pages != 40 || len(rep.Errors) != 0 || rep.Files != 6 {
		t.Fatalf("report: %+v", rep)
	}
	if op.maxRunning < 2 || op.maxRunning > 3 {
		t.Errorf("archives prepared at once: %d", op.maxRunning)
	}
	if len(op.released) != 40 {
		t.Errorf("released %d files", len(op.released))
	}
}
