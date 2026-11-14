// Command import-monolith is a one-off migration of the press_media archive
// (pages saved by monolith, listed in the media.articles table of the
// dataharvestlab database) into hoarfrost weeks. It is meant to run on the
// NAS that holds both press_media and the hoarfrost storage.
//
// For one ISO week it:
//
//  1. imports the pages saved that week as hoarfrost capture files (WARC +
//     CDXJ) into the staging directory, with inlined files split out of the
//     pages and stored once per week;
//  2. seals the week there with hoarfrost's own code (WACZ volumes, page
//     table, catalog, verification), using a scratch index schema;
//  3. gives the sealed files the ownership, modes and S3 gateway attributes
//     (user.etag, user.content-type) of objects stored through S3;
//  4. with -move, moves them into the hoarfrost storage (a rename on the
//     same file system); otherwise prints the commands that do it.
//
// hoarfrost shows the week after "hoarfrost reindex -epoch <week>", the
// Reindex action on its System page, or a restart.
//
// The capture time of a page is the time its file was written (kept in day
// folders and in .7z headers): the database has no reliable save time
// before March 2026. A manifest of all files is built once, in the staging
// directory.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/monolith"
)

// sevenZip is the 7-Zip executable.
var sevenZip = "7z"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "import-monolith:", err)
		os.Exit(1)
	}
}

type config struct {
	media, staging, storage string
	dsn, indexDSN           string
	tmp, source             string
	batch, workers, minSize int
	extractors              int
	move, moveOnly          bool
	manifestFile            string
	manifest                []entry
}

func run() error {
	var c config
	base := flag.String("base", "/storage/nas/projekty/dataharvestlab", "directory holding press_media/ and hoarfrost/")
	flag.StringVar(&c.media, "media", "", "press_media directory (default <base>/press_media)")
	flag.StringVar(&c.staging, "staging", "", "where weeks are built (default <base>/press_media/wacz_export); same file system as -storage")
	flag.StringVar(&c.storage, "storage", "", "hoarfrost storage root (default <base>/hoarfrost); weeks sealed there are skipped")
	flag.StringVar(&c.dsn, "db", os.Getenv("PRESS_MEDIA_DATABASE"), "dataharvestlab database URL, read only (default $PRESS_MEDIA_DATABASE)")
	flag.StringVar(&c.indexDSN, "index-db", os.Getenv("IMPORT_INDEX_DATABASE"), "PostgreSQL for the scratch index used while sealing, schema "+indexSchema+" is created and dropped (default $IMPORT_INDEX_DATABASE, else -db)")
	weeks := flag.String("epoch", "", "ISO week, or a range FROM..TO, e.g. 2026-W42 or 2019-W01..2026-W42")
	flag.IntVar(&c.batch, "batch", 100, "pages per WARC file")
	flag.IntVar(&c.workers, "workers", 0, "parallel page preparation (default: CPUs)")
	flag.IntVar(&c.extractors, "extractors", 2, "7z archives extracted at the same time, ahead of page preparation")
	flag.IntVar(&c.minSize, "min-size", monolith.DefaultMinSize, "smallest inlined file moved out of a page (bytes)")
	flag.StringVar(&c.tmp, "tmp", "", "directory for files extracted from .7z (default: -staging)")
	flag.StringVar(&c.source, "source", "press_media", "source name of imported pages")
	flag.StringVar(&sevenZip, "7z", "7z", "7-Zip executable (7z, 7za or 7zz)")
	flag.BoolVar(&c.move, "move", false, "move each sealed week into the hoarfrost storage")
	flag.BoolVar(&c.moveOnly, "move-only", false, "only move weeks already sealed in -staging (needs no database); implies -move")
	dry := flag.Bool("dry-run", false, "only list how many pages each week has")
	rescan := flag.Bool("rescan", false, "rebuild the manifest of press_media files")
	flag.Parse()
	if c.media == "" {
		c.media = filepath.Join(*base, "press_media")
	}
	if c.staging == "" {
		c.staging = filepath.Join(c.media, "wacz_export")
	}
	if c.storage == "" {
		c.storage = filepath.Join(*base, "hoarfrost")
	}
	if c.indexDSN == "" {
		c.indexDSN = c.dsn
	}
	if c.moveOnly {
		c.move = true
	}
	if *weeks == "" || (c.dsn == "" && !c.moveOnly) {
		flag.Usage()
		return errors.New("-epoch and -db are required")
	}
	eps, err := parseWeeks(*weeks)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := os.MkdirAll(c.staging, 0o755); err != nil {
		return err
	}
	c.manifestFile = filepath.Join(c.staging, "press_media-manifest.tsv")
	if *rescan {
		os.Remove(c.manifestFile)
	}
	for _, ep := range eps {
		if err := c.week(ctx, ep, *dry); err != nil {
			return fmt.Errorf("%s: %w", ep, err)
		}
	}
	return nil
}

func parseWeeks(s string) ([]epoch.ID, error) {
	from, to, isRange := strings.Cut(s, "..")
	a, err := epoch.Parse(from)
	if err != nil {
		return nil, err
	}
	if !isRange {
		return []epoch.ID{a}, nil
	}
	b, err := epoch.Parse(to)
	if err != nil {
		return nil, err
	}
	var out []epoch.ID
	for ep := a; !ep.Start().After(b.Start()); ep = ep.Next() {
		out = append(out, ep)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty range %s", s)
	}
	return out, nil
}

func exists(root, key string) bool {
	_, err := os.Stat(filepath.Join(root, filepath.FromSlash(key)))
	return err == nil
}

// week runs all steps for one week; each step is skipped when done.
func (c *config) week(ctx context.Context, ep epoch.ID, dry bool) error {
	if exists(c.storage, ep.CatalogKey()) {
		fmt.Fprintf(os.Stderr, "%s is already in %s, skipping\n", ep, c.storage)
		return nil
	}
	if exists(c.storage, ep.IncomingPrefix()) {
		return fmt.Errorf("an open week of hoarfrost (incoming captures in %s); not imported", c.storage)
	}
	if !exists(c.staging, ep.CatalogKey()) {
		if c.moveOnly {
			fmt.Fprintf(os.Stderr, "%s is not sealed in %s yet, skipping\n", ep, c.staging)
			return nil
		}
		if c.manifest == nil {
			var err error
			if c.manifest, err = loadManifest(ctx, c.media, c.manifestFile); err != nil {
				return err
			}
		}
		entries := weekEntries(c.manifest, ep)
		items, missing, err := load(ctx, c.dsn, entries)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "%s: %d file(s) saved between %s and %s, %d without a row in media.articles (URL taken from the page)\n", ep, len(entries),
			ep.Start().Format(time.RFC3339), ep.End().Format(time.RFC3339), missing)
		if dry || len(items) == 0 {
			return nil
		}
		if err := c.importWeek(ctx, ep, items); err != nil {
			return err
		}
		if err := c.seal(ctx, ep); err != nil {
			return err
		}
	} else if dry {
		fmt.Fprintf(os.Stderr, "%s is sealed in %s, waiting to be moved\n", ep, c.staging)
		return nil
	}
	files, err := finish(c.staging, ep)
	if err != nil {
		return err
	}
	if c.move {
		return moveWeek(c.staging, c.storage, ep, files)
	}
	fmt.Printf("# %s is ready; move it into hoarfrost with:\n%s", ep, moveCommands(c.staging, c.storage, ep))
	return nil
}

func (c *config) importWeek(ctx context.Context, ep epoch.ID, items []monolith.Item) error {
	tmp := c.tmp
	if tmp == "" {
		tmp = c.staging // extracted days can take gigabytes
	}
	x, err := os.MkdirTemp(tmp, ".import-monolith-7z-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(x)
	started := time.Now()
	rep, err := monolith.Import(ctx, items, &monolith.DirArchive{Dir: c.media, Tmp: x, SevenZip: sevenZip}, monolith.Options{
		Root: c.staging, Spool: filepath.Join(c.staging, ".import-monolith-spool"), Epoch: ep, Source: c.source,
		BatchSize: c.batch, MinSize: c.minSize, Workers: c.workers, Extractors: c.extractors,
		Progress: func(done, total int) {
			fmt.Fprintf(os.Stderr, "\r%s: %d/%d pages, %s", ep, done, total, time.Since(started).Round(time.Second))
		},
	})
	fmt.Fprintln(os.Stderr)
	if rep != nil {
		fmt.Printf("%s: %s\n", ep, rep)
		for _, e := range rep.Errors {
			fmt.Println("  error:", e)
		}
	}
	return err
}

// load joins the files saved in a week with their rows in media.articles
// (URL, title, publication date); the capture time is the file time.
func load(ctx context.Context, dsn string, entries []entry) (items []monolith.Item, missing int, err error) {
	if len(entries) == 0 {
		return nil, 0, nil
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, 0, err
	}
	cfg.RuntimeParams["default_transaction_read_only"] = "on"
	cfg.RuntimeParams["timezone"] = "UTC"
	db := stdlib.OpenDB(*cfg)
	defer db.Close()
	ids := make([]int64, len(entries))
	for i, e := range entries {
		ids[i] = e.ID
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id, link, COALESCE(title, ''), publication_date
		FROM media.articles WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	// Publication dates are days in Poland, where the articles appeared.
	warsaw, err := time.LoadLocation("Europe/Warsaw")
	if err != nil {
		return nil, 0, err
	}
	type row struct {
		link, title string
		published   sql.NullTime
	}
	byID := map[int64]row{}
	for rows.Next() {
		var id int64
		var r row
		if err := rows.Scan(&id, &r.link, &r.title, &r.published); err != nil {
			return nil, 0, err
		}
		byID[id] = r
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	for _, e := range entries {
		r, ok := byID[e.ID]
		if !ok {
			missing++
		}
		it := monolith.Item{ID: fmt.Sprint(e.ID), URL: r.link, Title: r.title, Captured: e.Saved, Path: e.Path, Archive: e.Archive}
		// hoarfrost drops fragments before its seen-URL check; the same
		// form here lets polled URLs match imported pages.
		if u, err := url.Parse(it.URL); err == nil && u.Fragment != "" {
			u.Fragment, u.RawFragment = "", ""
			it.URL = u.String()
		}
		if strings.Contains(it.URL, "news.google.") {
			continue
		}
		if r.published.Valid {
			it.Published = r.published.Time.In(warsaw).Format(time.DateOnly)
		}
		items = append(items, it)
	}
	return items, missing, nil
}
