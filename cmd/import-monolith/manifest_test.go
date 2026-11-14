package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/ogefest/hoarfrost/internal/epoch"
)

func TestManifest(t *testing.T) {
	if _, err := exec.LookPath("7z"); err != nil {
		t.Skip("7z not installed")
	}
	media := t.TempDir()
	write := func(rel string, mtime time.Time) {
		p := filepath.Join(media, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("<html>"+rel+"</html>"), 0o644)
		os.Chtimes(p, mtime, mtime)
	}
	packed := time.Date(2025, 1, 23, 19, 6, 11, 0, time.UTC)
	write("20240315/130000_321222.html", packed)
	write("20240315/140000_321223.html", packed.Add(time.Hour))
	if out, err := exec.Command("7z", "a", "-t7z", filepath.Join(media, "20240315.7z"), filepath.Join(media, "20240315")).CombinedOutput(); err != nil {
		t.Fatalf("7z: %v\n%s", err, out)
	}
	os.RemoveAll(filepath.Join(media, "20240315"))
	folder := time.Date(2025, 1, 15, 8, 0, 0, 0, time.UTC)
	write("20250115/080000_900001.html", folder)
	write("20250115/080000_321222.html", folder) // also in the archive: the folder wins
	write("20250115/notes.txt", folder)

	file := filepath.Join(t.TempDir(), "manifest.tsv")
	all, err := loadManifest(context.Background(), media, file)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("entries = %+v", all)
	}
	byID := map[int64]entry{}
	for _, e := range all {
		byID[e.ID] = e
	}
	if e := byID[321223]; e.Archive != "20240315.7z" || e.Path != "20240315/140000_321223.html" || !e.Saved.Equal(packed.Add(time.Hour)) {
		t.Errorf("archived entry = %+v", e)
	}
	if e := byID[321222]; e.Archive != "" || !e.Saved.Equal(folder) {
		t.Errorf("duplicate entry = %+v", e)
	}
	w := weekEntries(all, epoch.Of(packed))
	if len(w) != 1 || w[0].ID != 321223 {
		t.Errorf("week entries = %+v", w)
	}
	if again, err := loadManifest(context.Background(), media, file); err != nil || len(again) != 3 {
		t.Errorf("reloaded: %v %d", err, len(again))
	}
}

func TestParseWeeks(t *testing.T) {
	eps, err := parseWeeks("2025-W52..2026-W02")
	if err != nil || len(eps) != 3 || eps[0] != "2025-W52" || eps[2] != "2026-W02" {
		t.Fatalf("%v %v", eps, err)
	}
}
