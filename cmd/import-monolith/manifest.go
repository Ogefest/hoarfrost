package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ogefest/hoarfrost/internal/epoch"
)

// The manifest lists every saved page of press_media with the time the
// file was written, which is when monolith saved the page: the database has
// no reliable save time before March 2026 (updated_at was not maintained).
// Day folders (YYYYMMDD/) are listed with stat, packed days (YYYYMMDD.7z)
// from their headers without extracting.

type entry struct {
	ID      int64
	Path    string // YYYYMMDD/HHMMSS_<id>.html
	Archive string // YYYYMMDD.7z, or "" for a file in a folder
	Saved   time.Time
	Size    int64
}

var (
	dayName  = regexp.MustCompile(`^\d{8}$`)
	pageName = regexp.MustCompile(`^\d{6}_(\d+)\.html$`)
)

func pageID(path string) (int64, bool) {
	m := pageName.FindStringSubmatch(filepath.Base(path))
	if m == nil {
		return 0, false
	}
	id, err := strconv.ParseInt(m[1], 10, 64)
	return id, err == nil
}

// loadManifest reads the manifest file, building it first when missing.
func loadManifest(ctx context.Context, media, file string) ([]entry, error) {
	if _, err := os.Stat(file); errors.Is(err, fs.ErrNotExist) {
		if err := buildManifest(ctx, media, file); err != nil {
			return nil, err
		}
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []entry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Split(sc.Text(), "\t")
		if len(parts) != 5 {
			return nil, fmt.Errorf("manifest %s: bad line %q", file, sc.Text())
		}
		id, _ := strconv.ParseInt(parts[0], 10, 64)
		saved, err := time.Parse(time.RFC3339, parts[3])
		if err != nil {
			return nil, err
		}
		size, _ := strconv.ParseInt(parts[4], 10, 64)
		out = append(out, entry{ID: id, Path: parts[1], Archive: parts[2], Saved: saved, Size: size})
	}
	return out, sc.Err()
}

func buildManifest(ctx context.Context, media, file string) error {
	dirs, err := os.ReadDir(media)
	if err != nil {
		return err
	}
	var out []entry
	seen := map[int64]bool{}
	add := func(e entry) {
		// A page saved twice (folder and archive) is listed once; folders
		// are read first, archives may hold older copies.
		if !seen[e.ID] {
			seen[e.ID] = true
			out = append(out, e)
		}
	}
	for _, d := range dirs {
		if !d.IsDir() || !dayName.MatchString(d.Name()) {
			continue
		}
		files, err := os.ReadDir(filepath.Join(media, d.Name()))
		if err != nil {
			return err
		}
		for _, f := range files {
			id, ok := pageID(f.Name())
			if !ok {
				continue
			}
			info, err := f.Info()
			if err != nil {
				return err
			}
			add(entry{ID: id, Path: d.Name() + "/" + f.Name(), Saved: info.ModTime().UTC(), Size: info.Size()})
		}
	}
	n := 0
	for _, d := range dirs {
		name := d.Name()
		if d.IsDir() || !strings.HasSuffix(name, ".7z") || !dayName.MatchString(strings.TrimSuffix(name, ".7z")) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		n++
		fmt.Fprintf(os.Stderr, "\rmanifest: %s (%d archives)", name, n)
		list, err := list7z(ctx, filepath.Join(media, name))
		if err != nil {
			return err
		}
		for _, e := range list {
			e.Archive = name
			add(e)
		}
	}
	fmt.Fprintln(os.Stderr)
	sort.Slice(out, func(i, j int) bool { return out[i].Saved.Before(out[j].Saved) })
	tmp := file + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, e := range out {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%d\n", e.ID, e.Path, e.Archive, e.Saved.UTC().Format(time.RFC3339), e.Size)
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "manifest: %d pages in %s\n", len(out), file)
	return os.Rename(tmp, file)
}

// list7z lists the pages in a 7z archive with their modification times.
// 7z prints times in the local time zone of the machine. The header block
// of the archive itself ("Path = <archive>") is not a page and is dropped.
func list7z(ctx context.Context, archive string) ([]entry, error) {
	out, err := exec.CommandContext(ctx, sevenZip, "l", "-slt", archive).Output()
	if err != nil {
		return nil, fmt.Errorf("7z l %s: %w", archive, err)
	}
	var res []entry
	var cur entry
	var isDir bool
	flush := func() {
		if cur.Path != "" && !isDir {
			if id, ok := pageID(cur.Path); ok && !cur.Saved.IsZero() {
				cur.ID = id
				res = append(res, cur)
			}
		}
		cur, isDir = entry{}, false
	}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimRight(line, "\r"), " = ")
		switch {
		case !ok && strings.TrimSpace(line) == "":
			flush()
		case k == "Path":
			flush()
			cur.Path = filepath.ToSlash(strings.TrimPrefix(v, "./"))
		case k == "Size":
			cur.Size, _ = strconv.ParseInt(v, 10, 64)
		case k == "Modified":
			if t, err := time.ParseInLocation("2006-01-02 15:04:05", strings.SplitN(v, ".", 2)[0], time.Local); err == nil {
				cur.Saved = t.UTC()
			}
		case k == "Folder" && v == "+":
			isDir = true
		}
	}
	flush()
	return res, nil
}

// weekEntries returns the manifest entries saved in a week.
func weekEntries(all []entry, ep epoch.ID) []entry {
	start, end := ep.Start(), ep.End()
	i := sort.Search(len(all), func(i int) bool { return !all[i].Saved.Before(start) })
	var out []entry
	for ; i < len(all) && all[i].Saved.Before(end); i++ {
		out = append(out, all[i])
	}
	return out
}
