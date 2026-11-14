package monolith

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// DirArchive opens pages of a directory of day folders (YYYYMMDD/file.html),
// where older days are packed as YYYYMMDD.7z holding the same folder.
type DirArchive struct {
	Dir string
	// Tmp receives files extracted from .7z archives.
	Tmp string
	// SevenZip is the 7z executable (default "7z").
	SevenZip string
}

// Prepare extracts from .7z archives the files that are not in folders,
// each archive once.
func (d *DirArchive) Prepare(ctx context.Context, items []Item) error {
	byArchive := map[string][]string{}
	for _, it := range items {
		name := it.Archive
		if name == "" {
			if _, err := os.Stat(filepath.Join(d.Dir, filepath.FromSlash(it.Path))); err == nil {
				continue
			}
			day, _, ok := strings.Cut(it.Path, "/")
			if !ok {
				continue
			}
			name = day + ".7z"
		}
		byArchive[name] = append(byArchive[name], it.Path)
	}
	names := make([]string, 0, len(byArchive))
	for n := range byArchive {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) > 0 {
		if err := os.MkdirAll(d.Tmp, 0o755); err != nil {
			return err
		}
	}
	exe := d.SevenZip
	if exe == "" {
		exe = "7z"
	}
	for _, n := range names {
		archive := filepath.Join(d.Dir, n)
		if _, err := os.Stat(archive); err != nil {
			continue // reported per file by Open
		}
		list := filepath.Join(d.Tmp, n+".list")
		if err := os.WriteFile(list, []byte(strings.Join(byArchive[n], "\n")+"\n"), 0o644); err != nil {
			return err
		}
		cmd := exec.CommandContext(ctx, exe, "x", "-y", "-bd", "-o"+d.Tmp, archive, "@"+list)
		out, err := cmd.CombinedOutput()
		os.Remove(list)
		if err != nil && ctx.Err() == nil {
			// Missing files are reported by Open; a broken archive stops.
			if !strings.Contains(string(out), "No files to process") {
				return fmt.Errorf("monolith: 7z x %s: %v\n%s", archive, err, tail(out))
			}
		}
	}
	return ctx.Err()
}

// Open reads a page from the extracted files of its archive, or from its
// folder.
func (d *DirArchive) Open(it Item) ([]byte, error) {
	rel := filepath.FromSlash(it.Path)
	first, second := filepath.Join(d.Dir, rel), filepath.Join(d.Tmp, rel)
	if it.Archive != "" {
		first, second = second, first
	}
	b, err := os.ReadFile(first)
	if errors.Is(err, fs.ErrNotExist) {
		b, err = os.ReadFile(second)
	}
	if err == nil && len(b) == 0 {
		err = errors.New("empty file")
	}
	return b, err
}

// Release removes the files extracted for items.
func (d *DirArchive) Release(items []Item) {
	if d.Tmp == "" {
		return
	}
	for _, it := range items {
		os.Remove(filepath.Join(d.Tmp, filepath.FromSlash(it.Path)))
	}
}

func tail(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 2000 {
		s = "…" + s[len(s)-2000:]
	}
	return s
}
