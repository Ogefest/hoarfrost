package main

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/ogefest/hoarfrost/internal/archive"
	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/index"
	"github.com/ogefest/hoarfrost/internal/storage"
)

// indexSchema is the scratch index used while sealing a week.
const indexSchema = "hoarfrost_monolith_import"

// seal indexes and seals the week in the staging directory with hoarfrost's
// own code, then removes the week's incoming files there: the hoarfrost
// storage receives only the sealed week.
func (c *config) seal(ctx context.Context, ep epoch.ID) error {
	if err := dropSchema(ctx, c.indexDSN); err != nil {
		return err
	}
	defer dropSchema(context.WithoutCancel(ctx), c.indexDSN)
	idx, err := index.Open(ctx, c.indexDSN, indexSchema)
	if err != nil {
		return err
	}
	defer idx.Close()
	st, err := storage.NewFS(c.staging)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	arc := archive.New(st, idx, filepath.Join(c.staging, ".seal-spool"), log)
	defer arc.Close()
	fmt.Fprintf(os.Stderr, "%s: indexing\n", ep)
	if _, err := arc.Reindex(ctx, archive.ReindexOptions{Epoch: ep, Weeks: archive.OpenWeeks}); err != nil {
		return err
	}
	cat, err := arc.SealWith(ctx, ep, archive.SealOptions{Progress: func(phase string, done, total int64) {
		if total > 0 {
			fmt.Fprintf(os.Stderr, "\r%s: %s %d%%   ", ep, phase, done*100/total)
		}
	}})
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return err
	}
	if cat.State != index.StateVerified {
		return fmt.Errorf("sealed but not verified (%s)", cat.State)
	}
	fmt.Printf("%s: sealed %d volume(s), %.1f GB, %d pages, %d records (%d revisits), verified\n",
		ep, len(cat.Packages()), float64(cat.Size)/1e9, cat.Pages, cat.Records, cat.Revisits)
	return os.RemoveAll(filepath.Join(c.staging, filepath.FromSlash(ep.IncomingPrefix())))
}

func dropSchema(ctx context.Context, dsn string) error {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return err
	}
	db := stdlib.OpenDB(*cfg)
	defer db.Close()
	_, err = db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+pgx.Identifier{indexSchema}.Sanitize()+` CASCADE`)
	return err
}

// weekFiles lists the sealed files of a week, relative to root, in the
// order they are moved: volumes, the page table, the catalog last (it
// marks the week sealed).
func weekFiles(root string, ep epoch.ID) ([]string, error) {
	dir := strings.TrimSuffix(ep.VolumeDir(), "/")
	entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(dir)))
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		files = append(files, dir+"/"+e.Name())
	}
	sort.Strings(files)
	return append(files, ep.ParquetKey(), ep.CatalogKey()), nil
}

// finish gives the sealed files what objects stored through the S3 gateway
// (versitygw, posix backend) have: owner root when run as root, mode 0644
// (directories 0755), and the user.etag (MD5 of a single-part upload) and
// user.content-type attributes.
func finish(root string, ep epoch.ID) ([]string, error) {
	files, err := weekFiles(root, ep)
	if err != nil {
		return nil, err
	}
	asRoot := os.Geteuid() == 0
	dir := filepath.Join(root, filepath.FromSlash(strings.TrimSuffix(ep.VolumeDir(), "/")))
	if err := os.Chmod(dir, 0o755); err != nil {
		return nil, err
	}
	if asRoot {
		if err := os.Chown(dir, 0, 0); err != nil {
			return nil, err
		}
	}
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.Chmod(p, 0o644); err != nil {
			return nil, err
		}
		if asRoot {
			if err := os.Chown(p, 0, 0); err != nil {
				return nil, err
			}
		}
		sum, err := md5File(p)
		if err != nil {
			return nil, err
		}
		if err := syscall.Setxattr(p, "user.etag", []byte(`"`+sum+`"`), 0); err != nil {
			return nil, fmt.Errorf("xattr on %s: %w", p, err)
		}
		if err := syscall.Setxattr(p, "user.content-type", []byte("application/octet-stream"), 0); err != nil {
			return nil, fmt.Errorf("xattr on %s: %w", p, err)
		}
	}
	return files, nil
}

func md5File(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// moveWeek renames the sealed week into the hoarfrost storage; the volume
// directory goes as a whole, the catalog last.
func moveWeek(staging, dst string, ep epoch.ID, files []string) error {
	dir := strings.TrimSuffix(ep.VolumeDir(), "/")
	for _, k := range []string{dir, ep.ParquetKey(), ep.CatalogKey()} {
		if exists(dst, k) {
			return fmt.Errorf("%s already exists in %s; nothing moved", k, dst)
		}
	}
	for _, k := range []string{dir, ep.ParquetKey(), ep.CatalogKey()} {
		to := filepath.Join(dst, filepath.FromSlash(k))
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			return err
		}
		if err := os.Rename(filepath.Join(staging, filepath.FromSlash(k)), to); err != nil {
			return err
		}
	}
	fmt.Printf("%s moved into %s (%d files); reindex the week in hoarfrost\n", ep, dst, len(files))
	return nil
}

func moveCommands(staging, dst string, ep epoch.ID) string {
	dir := strings.TrimSuffix(ep.VolumeDir(), "/")
	var b strings.Builder
	fmt.Fprintf(&b, "mkdir -p %s %s\n", filepath.Dir(filepath.Join(dst, dir)), filepath.Dir(filepath.Join(dst, ep.CatalogKey())))
	for _, k := range []string{dir, ep.ParquetKey(), ep.CatalogKey()} {
		fmt.Fprintf(&b, "mv -n %s %s\n", filepath.Join(staging, k), filepath.Dir(filepath.Join(dst, k))+"/")
	}
	return b.String()
}
