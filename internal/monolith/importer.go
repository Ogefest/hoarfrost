package monolith

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ogefest/hoarfrost/internal/archive"
	"github.com/ogefest/hoarfrost/internal/cdxj"
	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/extract"
	"github.com/ogefest/hoarfrost/internal/legacy"
	"github.com/ogefest/hoarfrost/internal/recorder"
	"github.com/ogefest/hoarfrost/internal/warcio"
)

// Item is one saved page to import.
type Item struct {
	// ID identifies the page in the source system; it is kept in the
	// capture metadata and makes an interrupted import resumable.
	ID       string
	URL      string
	Title    string
	Captured time.Time
	// Published is the publication date (YYYY-MM-DD) when known.
	Published string
	// Path is the location of the file in the source archive.
	Path string
	// Archive names the packed file holding Path, when it is packed.
	Archive string
}

// Opener returns the content of saved pages. Prepare is called with all
// items of the week before the first Open (to extract archives in bulk).
type Opener interface {
	Prepare(ctx context.Context, items []Item) error
	Open(it Item) ([]byte, error)
}

// Releaser is implemented by openers that can drop prepared files once
// their items are written.
type Releaser interface {
	Release(items []Item)
}

// Options configure an import.
type Options struct {
	// Root is the storage root the week is written into
	// (<Root>/incoming/<year>/W<week>/); it must be on the same file system
	// as Spool, so finished files appear there atomically.
	Root  string
	Spool string
	Epoch epoch.ID
	// Source is the source name of imported pages.
	Source string
	// BatchSize is the number of pages per WARC file.
	BatchSize int
	MinSize   int
	Workers   int
	// Extractors bounds archives prepared at the same time (default 2).
	Extractors int
	Progress   func(done, total int)
}

// Report summarizes an import.
type Report struct {
	Pages, Skipped, Resumed int
	Files                   int
	Resources, Reused       int
	PageBytes, ResBytes     int64
	SourceBytes             int64
	Errors                  []string
}

// Import writes the pages of one week as hoarfrost capture files (WARC and
// CDXJ) in Root. Items outside the week are refused. Pages with an ID
// already present in the week directory are skipped, so a stopped import
// continues where it ended. Inlined files are stored once per week: later
// pages refer to the stored URL.
func Import(ctx context.Context, items []Item, open Opener, o Options) (*Report, error) {
	if o.BatchSize <= 0 {
		o.BatchSize = 100
	}
	if o.MinSize <= 0 {
		o.MinSize = DefaultMinSize
	}
	if o.Workers <= 0 {
		o.Workers = runtime.GOMAXPROCS(0)
	}
	if o.Source == "" {
		o.Source = "monolith"
	}
	if o.Extractors <= 0 {
		o.Extractors = 2
	}
	for _, it := range items {
		if epoch.Of(it.Captured) != o.Epoch {
			return nil, fmt.Errorf("monolith: %s captured %s is outside %s", it.ID, it.Captured.UTC().Format(time.RFC3339), o.Epoch)
		}
	}
	dir := filepath.Join(o.Root, filepath.FromSlash(o.Epoch.IncomingPrefix()))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(o.Spool, 0o755); err != nil {
		return nil, err
	}
	w := &week{dir: dir, opts: o, stored: map[string]bool{}, digests: map[string]recorder.Ref{}, done: map[string]bool{}}
	rep := &Report{}
	if err := w.load(ctx); err != nil {
		return nil, err
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].Captured.Before(items[j].Captured) })
	var todo []Item
	urls := map[string]bool{}
	for _, url := range w.pageURLs {
		urls[url] = true
	}
	for _, it := range items {
		switch {
		case w.done[it.ID]:
			rep.Resumed++
		case it.URL != "" && urls[it.URL]:
			rep.Skipped++ // the same URL saved twice in one week
		default:
			urls[it.URL] = true
			todo = append(todo, it)
		}
	}
	if len(todo) == 0 {
		return rep, nil
	}
	return rep, w.run(ctx, todo, open, o, rep)
}

// run imports the pages through a pipeline: archives are extracted a few
// at a time and ahead of the work (folders need none), every worker
// prepares pages as soon as their file is there, and finished pages are
// written in batches whichever archive they came from. Extracted files
// are released once their page is written, so the temporary space holds
// about Extractors archives.
func (w *week) run(ctx context.Context, todo []Item, open Opener, o Options, rep *Report) error {
	sort.SliceStable(todo, func(i, j int) bool { return todo[i].Archive < todo[j].Archive })
	var groups [][]Item
	for start := 0; start < len(todo); {
		end := start
		for end < len(todo) && todo[end].Archive == todo[start].Archive {
			end++
		}
		groups = append(groups, todo[start:end])
		start = end
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		failMu  sync.Mutex
		failErr error
	)
	fail := func(err error) {
		failMu.Lock()
		if failErr == nil {
			failErr = err
		}
		failMu.Unlock()
		cancel()
	}
	failed := func() error {
		failMu.Lock()
		defer failMu.Unlock()
		return failErr
	}

	ready := make(chan Item, o.Workers*2)
	go func() {
		defer close(ready)
		var wg sync.WaitGroup
		sem := make(chan struct{}, o.Extractors)
		for _, g := range groups {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				wg.Wait()
				return
			}
			wg.Add(1)
			go func(g []Item) {
				defer wg.Done()
				defer func() { <-sem }()
				if err := open.Prepare(ctx, g); err != nil {
					fail(err)
					return
				}
				for _, it := range g {
					select {
					case ready <- it:
					case <-ctx.Done():
						return
					}
				}
			}(g)
		}
		wg.Wait()
	}()

	results := make(chan page, o.Workers*2)
	var workers sync.WaitGroup
	for range o.Workers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for it := range ready {
				results <- preparePage(it, open, o)
			}
		}()
	}
	go func() { workers.Wait(); close(results) }()

	release := func(items []Item) {
		if r, ok := open.(Releaser); ok {
			r.Release(items)
		}
	}
	var batch []page
	done := 0
	flush := func() {
		if len(batch) == 0 || failed() != nil {
			return
		}
		if err := w.write(ctx, batch, rep); err != nil {
			fail(err)
			return
		}
		items := make([]Item, len(batch))
		for i, p := range batch {
			items[i] = p.Item
		}
		release(items)
		done += len(batch)
		batch = batch[:0]
		if o.Progress != nil {
			o.Progress(done+len(rep.Errors), len(todo))
		}
	}
	for p := range results {
		if failed() != nil {
			continue // drain until the workers stop
		}
		if p.err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s (%s): %v", p.ID, p.Path, p.err))
			release([]Item{p.Item})
			continue
		}
		batch = append(batch, p)
		if len(batch) >= o.BatchSize {
			flush()
		}
	}
	flush()
	if err := failed(); err != nil {
		return err
	}
	return ctx.Err()
}

// page is an item read, split and extracted.
type page struct {
	Item
	html     []byte
	res      []Resource
	ex       extract.Result
	srcBytes int64
	err      error
}

// preparePage reads, splits and extracts one page; extraction of article
// text is the expensive part.
func preparePage(it Item, open Opener, o Options) page {
	p := page{Item: it}
	body, err := open.Open(it)
	if err == nil && p.URL == "" {
		// No URL from the source system: the page's own canonical or
		// og:url.
		if p.URL = legacy.PageURL(body); p.URL == "" {
			err = errors.New("no URL known and none in the page")
		}
	}
	if err != nil {
		p.err = err
		return p
	}
	p.srcBytes = int64(len(body))
	p.html, p.res = Split(body, o.MinSize)
	p.ex = extract.HTML(p.html, "text/html; charset=utf-8", p.URL)
	return p
}

// week is the state of the week directory: what is already stored there.
type week struct {
	dir      string
	opts     Options
	stored   map[string]bool         // resource URLs stored in the week
	digests  map[string]recorder.Ref // payload digests of stored records
	done     map[string]bool         // item IDs imported
	pageURLs []string
}

// Lookup implements recorder.Dedup over the files already in the week.
func (w *week) Lookup(_ context.Context, _ epoch.ID, digest string) (*recorder.Ref, error) {
	if ref, ok := w.digests[digest]; ok {
		return &ref, nil
	}
	return nil, nil
}

// load reads the capture files already in the week directory (a resumed
// import), writing a missing CDXJ index.
func (w *week) load(ctx context.Context) error {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !strings.HasSuffix(e.Name(), ".warc.gz") {
			continue
		}
		p := filepath.Join(w.dir, e.Name())
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		cf, err := recorder.Scan(bufio.NewReaderSize(f, 1<<20), e.Name(), w.opts.Epoch)
		f.Close()
		if err != nil {
			return fmt.Errorf("monolith: scan %s: %w", p, err)
		}
		w.remember(cf)
		idx := archive.IndexKey(p)
		if _, err := os.Stat(idx); errors.Is(err, fs.ErrNotExist) {
			if err := writeIndex(idx, cf); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *week) remember(cf recorder.CaptureFile) {
	for _, r := range cf.Records {
		if r.Type != warcio.TypeResource {
			continue
		}
		if strings.HasPrefix(r.URI, "https://"+Host+"/") {
			w.stored[r.URI] = true
		}
		if r.Digest != "" {
			if _, ok := w.digests[r.Digest]; !ok {
				w.digests[r.Digest] = recorder.Ref{RecordID: r.ID, URI: r.URI, Date: r.Date}
			}
		}
	}
	for _, p := range cf.Pages {
		if id := p.Extra[idKey]; id != "" {
			w.done[id] = true
		}
		w.pageURLs = append(w.pageURLs, p.URL)
	}
}

// idKey names the item ID in the capture metadata.
const idKey = "importId"

// write stores a batch as one capture file.
func (w *week) write(ctx context.Context, pages []page, rep *Report) error {
	var first time.Time
	for _, p := range pages {
		if p.err == nil {
			first = p.Captured
			break
		}
	}
	if first.IsZero() {
		for _, p := range pages {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s (%s): %v", p.ID, p.Path, p.err))
		}
		return nil
	}
	now := first
	rec, err := recorder.New(recorder.Options{
		Epoch: w.opts.Epoch, Dedup: w, SpoolDir: w.opts.Spool, Source: w.opts.Source,
		Engine: "monolith", Worker: "import-monolith", Now: func() time.Time { return now },
		Info: map[string]string{"description": "pages saved by monolith, imported with inlined files split into resources"},
	})
	if err != nil {
		return err
	}
	var added []string
	for _, p := range pages {
		if p.err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s (%s): %v", p.ID, p.Path, p.err))
			continue
		}
		now = p.Captured
		if err := w.writePage(ctx, rec, p, rep, &added); err != nil {
			rec.Abort()
			for _, u := range added {
				delete(w.stored, u)
			}
			return err
		}
	}
	cf, err := rec.Close()
	if err != nil {
		return err
	}
	final := filepath.Join(w.dir, cf.Name)
	// The index first: a WARC file in the directory is always complete and
	// indexed, and is what a resumed import trusts.
	if err := writeIndex(archive.IndexKey(final), cf); err != nil {
		os.Remove(cf.Path)
		return err
	}
	if err := os.Rename(cf.Path, final); err != nil {
		os.Remove(cf.Path)
		os.Remove(archive.IndexKey(final))
		return err
	}
	w.remember(cf)
	rep.Files++
	return nil
}

func (w *week) writePage(ctx context.Context, rec *recorder.Recorder, p page, rep *Report, added *[]string) error {
	info, err := rec.WriteResource(ctx, recorder.ResourceRecord{
		URI: p.URL, Date: p.Captured, ContentType: "text/html; charset=utf-8", Body: p.html,
	})
	if err != nil {
		return err
	}
	rep.PageBytes += int64(len(p.html))
	rep.SourceBytes += p.srcBytes
	for _, r := range p.res {
		if w.stored[r.URL] {
			rep.Reused++
			continue
		}
		if _, err := rec.WriteResource(ctx, recorder.ResourceRecord{
			URI: r.URL, Date: p.Captured, ContentType: r.ContentType, Body: r.Body, ConcurrentTo: info.ID,
		}); err != nil {
			return err
		}
		w.stored[r.URL] = true
		*added = append(*added, r.URL)
		rep.Resources++
		rep.ResBytes += int64(len(r.Body))
	}
	pi := recorder.PageInfo{
		URL: p.URL, RecordID: info.ID, Title: p.ex.Title, Text: p.ex.Text, Captured: p.Captured,
		Published: p.Published, Kind: "page", Mime: "text/html", Status: 200, Resources: len(p.res),
		Extra: map[string]string{idKey: p.ID, "legacyPath": p.Path},
	}
	if pi.Published == "" {
		pi.Published = p.ex.Published
	}
	if pi.Title == "" {
		pi.Title = p.Title
	}
	p.ex.Fill(&pi)
	if _, err := rec.WriteMetadata(pi); err != nil {
		return err
	}
	rep.Pages++
	return nil
}

func writeIndex(path string, cf recorder.CaptureFile) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	bw := bufio.NewWriter(f)
	err = cdxj.Write(bw, cf.CDXJ())
	if err == nil {
		err = bw.Flush()
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// String formats sizes of a report for people.
func (r *Report) String() string {
	mb := func(n int64) string { return strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64) + " MB" }
	return fmt.Sprintf("pages %d (resumed %d, skipped %d) in %d file(s); source %s; html %s; resources %d new %s, %d reused",
		r.Pages, r.Resumed, r.Skipped, r.Files, mb(r.SourceBytes), mb(r.PageBytes), r.Resources, mb(r.ResBytes), r.Reused)
}
