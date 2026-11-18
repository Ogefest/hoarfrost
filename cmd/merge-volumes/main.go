// Command merge-volumes adds the sealed weeks of one hoarfrost storage
// directory to the same weeks of another, as additional volumes: the WACZ
// files are moved (renamed) unchanged, numbered after the week's own
// volumes, and the week's catalog, SHA256SUMS and collection.json are
// rewritten to list them. It is how the press_media files left out by the
// first import were put into the main archive.
//
// Without -apply it only prints what it would do. With -apply it keeps a
// copy of every catalog, SHA256SUMS and collection.json it changes in
// -backup. A merged week is left "sealed" without a page table: hoarfrost
// rebuilds the week's index when it starts (the catalog hash changed),
// writes the page table, and the week is verified again from the System
// page. Run it as root on the storage host, like the import, so the files
// get the owner and attributes of objects stored through the S3 gateway.
package main

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/ogefest/hoarfrost/internal/archive"
	"github.com/ogefest/hoarfrost/internal/index"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "merge-volumes:", err)
		os.Exit(1)
	}
}

func run() error {
	dst := flag.String("into", "", "storage root receiving the volumes (the main archive)")
	src := flag.String("from", "", "storage root holding the weeks to add")
	backup := flag.String("backup", "", "directory for copies of the changed files (required with -apply)")
	apply := flag.Bool("apply", false, "make the changes (default: only print them)")
	flag.Parse()
	if *dst == "" || *src == "" || (*apply && *backup == "") {
		flag.Usage()
		return errors.New("-into and -from are required, and -backup with -apply")
	}
	cats, err := filepath.Glob(filepath.Join(*src, "catalog", "*", "W*.json"))
	if err != nil {
		return err
	}
	sort.Strings(cats)
	if len(cats) == 0 {
		fmt.Println("nothing to merge in", *src)
	}
	for _, p := range cats {
		var c archive.Catalog
		if err := readJSON(p, &c); err != nil {
			return err
		}
		if err := mergeWeek(*dst, *src, *backup, &c, *apply); err != nil {
			return fmt.Errorf("%s: %w", c.Epoch, err)
		}
	}
	if *apply {
		// The source keeps nothing once every week is merged.
		removeEmptyDirs(*src)
	}
	return nil
}

func mergeWeek(dstRoot, srcRoot, backupRoot string, add *archive.Catalog, apply bool) error {
	ep := add.Epoch
	var cat archive.Catalog
	if err := readJSON(filepath.Join(dstRoot, ep.CatalogKey()), &cat); err != nil {
		return fmt.Errorf("the week is not sealed in %s: %w", dstRoot, err)
	}
	if len(cat.Volumes) == 0 || len(add.Volumes) == 0 {
		return errors.New("both weeks must be sealed in volumes")
	}
	if add.State != index.StateVerified {
		return fmt.Errorf("the week to add is %s, not verified", add.State)
	}
	have := map[string]bool{}
	for _, v := range cat.Volumes {
		have[v.SHA256] = true
	}
	n := len(cat.Volumes)
	type move struct {
		from     archive.Volume
		to       archive.Volume
		manifest *archive.Manifest
	}
	var moves []move
	for _, v := range add.Volumes {
		if have[v.SHA256] {
			fmt.Printf("%s: %s already in the week\n", ep, filepath.Base(v.Key))
			continue
		}
		n++
		m, err := readManifest(filepath.Join(srcRoot, v.CapturesList))
		if err != nil {
			return err
		}
		if m.Package != v.Key || m.SHA256 != v.SHA256 || m.Size != v.Size {
			return fmt.Errorf("%s does not describe %s", v.CapturesList, v.Key)
		}
		to := v
		to.Key, to.CapturesList = ep.VolumeKey(n), ep.VolumeCapturesKey(n)
		for _, k := range []string{to.Key, to.CapturesList} {
			if _, err := os.Stat(filepath.Join(dstRoot, k)); err == nil {
				return fmt.Errorf("%s already exists", k)
			}
		}
		moves = append(moves, move{from: v, to: to, manifest: m})
	}
	if len(moves) == 0 {
		if apply {
			return removeWeek(srcRoot, add)
		}
		return nil
	}

	next := cat
	next.Volumes = append([]archive.Volume(nil), cat.Volumes...)
	for _, mv := range moves {
		next.Volumes = append(next.Volumes, mv.to)
		next.Size += mv.to.Size
		next.Captures += mv.manifest.CaptureCount
		next.WARCFiles += mv.manifest.WARCFiles
		next.Records += mv.manifest.Indexed
		next.Revisits += mv.manifest.Revisits
		next.Pages += mv.manifest.Pages
	}
	sums := archive.VolumeSums(next.Volumes)
	coll, err := archive.VolumeCollection(ep, next.Volumes)
	if err != nil {
		return err
	}
	h := sha256.Sum256(sums)
	next.SHA256 = hex.EncodeToString(h[:])
	// The week changed: it is verified again and its page table rebuilt
	// from the new index.
	next.State, next.Verification, next.Parquet = index.StateSealed, nil, nil

	for _, mv := range moves {
		fmt.Printf("%s: %s (%d pages, %.1f MB) -> %s\n", ep, filepath.Base(mv.from.Key), mv.to.Pages, float64(mv.to.Size)/1e6, filepath.Base(mv.to.Key))
	}
	fmt.Printf("%s: %d -> %d volume(s), %d -> %d pages\n", ep, len(cat.Volumes), len(next.Volumes), cat.Pages, next.Pages)
	if !apply {
		return nil
	}

	bdir := filepath.Join(backupRoot, string(ep))
	if err := os.MkdirAll(bdir, 0o755); err != nil {
		return err
	}
	for _, k := range []string{ep.CatalogKey(), cat.Sums, cat.Collection} {
		if err := copyFile(filepath.Join(dstRoot, k), filepath.Join(bdir, filepath.Base(k))); err != nil {
			return fmt.Errorf("backup: %w", err)
		}
	}
	// Volumes and their captures lists first, the week's lists after, the
	// catalog last: until it is written the week is what it was.
	for _, mv := range moves {
		mv.manifest.Package = mv.to.Key
		var b bytes.Buffer
		if err := writeManifest(&b, mv.manifest); err != nil {
			return err
		}
		if err := putFile(dstRoot, mv.to.CapturesList, b.Bytes()); err != nil {
			return err
		}
		if err := os.Rename(filepath.Join(srcRoot, mv.from.Key), filepath.Join(dstRoot, mv.to.Key)); err != nil {
			return err
		}
		if err := gatewayAttrs(filepath.Join(dstRoot, mv.to.Key)); err != nil {
			return err
		}
	}
	if err := putFile(dstRoot, next.Sums, sums); err != nil {
		return err
	}
	if err := putFile(dstRoot, next.Collection, coll); err != nil {
		return err
	}
	b, err := json.MarshalIndent(&next, "", "  ")
	if err != nil {
		return err
	}
	if err := putFile(dstRoot, ep.CatalogKey(), append(b, '\n')); err != nil {
		return err
	}
	fmt.Printf("%s: merged\n", ep)
	return removeWeek(srcRoot, add)
}

// removeWeek deletes what is left of a merged week in the source.
func removeWeek(root string, c *archive.Catalog) error {
	keys := []string{c.Sums, c.Collection, c.Epoch.ParquetKey(), c.Epoch.CatalogKey()}
	for _, v := range c.Volumes {
		keys = append(keys, v.CapturesList, v.Key)
	}
	for _, k := range keys {
		if k == "" {
			continue
		}
		if err := os.Remove(filepath.Join(root, k)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	os.Remove(filepath.Join(root, strings.TrimSuffix(c.Epoch.VolumeDir(), "/")))
	return nil
}

func removeEmptyDirs(root string) {
	var dirs []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		os.Remove(dirs[i]) // fails, as it should, unless empty
	}
}

func readJSON(p string, v any) error {
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func readManifest(p string) (*archive.Manifest, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(bufio.NewReader(f))
	var m archive.Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	if m.Format != archive.ManifestFormat {
		return nil, fmt.Errorf("%s: format %q", p, m.Format)
	}
	for {
		var c archive.ManifestCapture
		if err := dec.Decode(&c); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		m.Captures = append(m.Captures, c)
	}
	if len(m.Captures) != m.CaptureCount {
		return nil, fmt.Errorf("%s: %d of %d captures", p, len(m.Captures), m.CaptureCount)
	}
	return &m, nil
}

// writeManifest encodes a captures list as archive.WriteManifest does.
func writeManifest(w io.Writer, m *archive.Manifest) error {
	m.Format, m.CaptureCount = archive.ManifestFormat, len(m.Captures)
	enc := json.NewEncoder(w)
	if err := enc.Encode(m); err != nil {
		return err
	}
	for _, c := range m.Captures {
		if err := enc.Encode(c); err != nil {
			return err
		}
	}
	return nil
}

// putFile replaces root/key atomically and gives it the gateway attributes.
func putFile(root, key string, data []byte) error {
	p := filepath.Join(root, filepath.FromSlash(key))
	tmp := p + ".merge-tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := gatewayAttrs(tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, p)
}

func copyFile(from, to string) error {
	b, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	return os.WriteFile(to, b, 0o644)
}

// gatewayAttrs gives a file what objects stored through the S3 gateway
// (versitygw, posix backend) have: mode 0644, owner root when run as root,
// and the user.etag (MD5) and user.content-type attributes.
func gatewayAttrs(p string) error {
	if err := os.Chmod(p, 0o644); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(p, 0, 0); err != nil {
			return err
		}
	}
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	h := md5.New()
	_, err = io.Copy(h, f)
	f.Close()
	if err != nil {
		return err
	}
	if err := syscall.Setxattr(p, "user.etag", []byte(`"`+hex.EncodeToString(h.Sum(nil))+`"`), 0); err != nil {
		return fmt.Errorf("xattr on %s: %w", p, err)
	}
	return syscall.Setxattr(p, "user.content-type", []byte("application/octet-stream"), 0)
}
