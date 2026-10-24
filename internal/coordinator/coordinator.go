// Package coordinator is the single writer of an instance: it polls
// sources, owns the queue, answers deduplication lookups, indexes completed
// captures and seals epochs.
package coordinator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ogefest/hoarfrost/internal/archive"
	"github.com/ogefest/hoarfrost/internal/blocklist"
	"github.com/ogefest/hoarfrost/internal/config"
	"github.com/ogefest/hoarfrost/internal/engine"
	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/index"
	"github.com/ogefest/hoarfrost/internal/queue"
	"github.com/ogefest/hoarfrost/internal/recorder"
	"github.com/ogefest/hoarfrost/internal/sources"
	"github.com/ogefest/hoarfrost/internal/sourcestore"
	"github.com/ogefest/hoarfrost/internal/storage"
	"github.com/ogefest/hoarfrost/internal/worker"
)

// Priorities of jobs by origin (lower runs first).
const (
	PriorityAPI        = 10
	PriorityFallback   = 50
	PriorityAttachment = 150
)

// Options configure a Coordinator.
type Options struct {
	Library *archive.Library
	Queue   *queue.Queue
	Fetcher *sources.Fetcher
	// Sources keeps the capture configuration (app.sqlite).
	Sources       *sourcestore.Store
	LeaseDuration time.Duration
	SealGrace     time.Duration
	Log           *slog.Logger
	// BlocklistDir caches downloaded block lists (default: next to the spool).
	BlocklistDir string
}

// SourceStatus is the result of the last poll of a source.
type SourceStatus = sourcestore.Status

// ErrUnknownSource is returned when a named source does not exist.
var ErrUnknownSource = errors.New("coordinator: unknown source")

// WorkerStatus describes a worker seen recently.
type WorkerStatus struct {
	Name     string
	LastSeen time.Time
	Leases   int
}

// Coordinator implements worker.Coordinator for local and remote workers.
type Coordinator struct {
	lib     *archive.Library
	idx     *index.DB
	queue   *queue.Queue
	fetcher *sources.Fetcher
	log     *slog.Logger
	opts    Options
	Now     func() time.Time
	Metrics *Metrics

	sources atomic.Pointer[config.Sources]
	srcMu   sync.Mutex
	blocks  atomic.Pointer[blocklist.Updater]

	epochMu sync.Mutex

	mu      sync.Mutex
	workers map[string]*WorkerStatus
	sealErr map[string]string
}

// New creates a coordinator and loads the capture configuration.
func New(o Options) (*Coordinator, error) {
	if o.LeaseDuration == 0 {
		o.LeaseDuration = 10 * time.Minute
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Fetcher == nil {
		o.Fetcher = sources.NewFetcher("")
	}
	c := &Coordinator{
		lib: o.Library, idx: o.Library.Index, queue: o.Queue, fetcher: o.Fetcher,
		log: o.Log, opts: o, Now: time.Now, workers: map[string]*WorkerStatus{},
		sealErr: map[string]string{},
	}
	c.Metrics = newMetrics(c)
	if o.BlocklistDir == "" {
		def, _ := o.Library.Archive("")
		o.BlocklistDir = filepath.Join(filepath.Dir(filepath.Dir(def.SpoolDir)), "blocklists")
		c.opts.BlocklistDir = o.BlocklistDir
	}
	if o.Sources == nil {
		return nil, errors.New("coordinator: Options.Sources is required")
	}
	src, err := o.Sources.Load(context.Background())
	if err != nil {
		return nil, err
	}
	if err := c.checkStorages(src); err != nil {
		// Not fatal: the configuration is fixed in the UI; such sources
		// are not polled until then.
		c.log.Warn("capture configuration", "err", err)
	}
	c.sources.Store(src)
	c.configureBlocklists(src)
	return c, nil
}

// configureBlocklists (re)creates the block list updater when the
// configuration changes; cached lists are loaded immediately.
func (c *Coordinator) configureBlocklists(src *config.Sources) {
	urls, refresh, allow := src.BlocklistSettings()
	if cur := c.blocks.Load(); cur != nil && slices.Equal(cur.URLs, urls) && slices.Equal(cur.Allow, allow) && cur.Refresh == refresh {
		return
	}
	u := blocklist.NewUpdater(urls, allow, c.opts.BlocklistDir, refresh, c.log)
	if err := u.Load(); err != nil {
		c.log.Warn("loading cached block lists", "err", err)
	}
	c.blocks.Store(u)
}

// Blocklist returns the current global block lists.
func (c *Coordinator) Blocklist(context.Context) (*blocklist.Set, error) {
	return c.blocks.Load().Set(), nil
}

// BlocklistStatuses describes the configured block lists.
func (c *Coordinator) BlocklistStatuses() []blocklist.Status { return c.blocks.Load().Statuses() }

// UpdateBlocklists downloads the block lists now.
func (c *Coordinator) UpdateBlocklists(ctx context.Context) error { return c.blocks.Load().Update(ctx) }

// Sources returns the current capture configuration.
func (c *Coordinator) Sources() *config.Sources { return c.sources.Load() }

// Library returns the archives of all storages.
func (c *Coordinator) Library() *archive.Library { return c.lib }

// Storages lists configured storage names, the default one first.
func (c *Coordinator) Storages() []string { return c.lib.Names() }

// checkStorages verifies that the configuration only names known storages.
func (c *Coordinator) checkStorages(src *config.Sources) error {
	for _, name := range src.StorageNames() {
		if !c.lib.Has(name) {
			return fmt.Errorf("coordinator: storage %q is not configured (HOARFROST_STORAGES)", name)
		}
	}
	return nil
}

// Queue returns the job queue.
func (c *Coordinator) Queue() *queue.Queue { return c.queue }

// Lookup finds a committed record with the digest in a storage and epoch.
func (c *Coordinator) Lookup(ctx context.Context, storageName string, ep epoch.ID, digest string) (*recorder.Ref, error) {
	return c.idx.Lookup(ctx, storageName, ep, digest)
}

func (c *Coordinator) seen(name string, leases int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w, ok := c.workers[name]
	if !ok {
		w = &WorkerStatus{Name: name}
		c.workers[name] = w
	}
	w.LastSeen = c.Now()
	w.Leases += leases
}

// Workers lists workers seen in the last hour.
func (c *Coordinator) Workers() []WorkerStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []WorkerStatus
	for _, w := range c.workers {
		if c.Now().Sub(w.LastSeen) < time.Hour {
			out = append(out, *w)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Lease hands out jobs with resolved per-source options.
func (c *Coordinator) Lease(ctx context.Context, r worker.LeaseRequest) ([]engine.Job, error) {
	src := c.Sources()
	jobs, err := c.queue.Lease(ctx, queue.LeaseRequest{
		Worker: r.Worker, Engines: r.Engines, Storages: r.Storages, Max: r.Max, MaxPerDomain: src.MaxPerDomain(), Duration: c.opts.LeaseDuration,
	})
	if err != nil {
		return nil, err
	}
	c.seen(r.Worker, len(jobs))
	out := make([]engine.Job, len(jobs))
	blocks := c.blocks.Load().Set()
	for i, j := range jobs {
		res := src.Resolve(j.Source)
		if blocks.Len() > 0 {
			res.Options.BlocklistVersion = blocks.Version()
		}
		out[i] = engine.Job{ID: j.ID, URL: j.URL, Source: j.Source, Storage: j.Storage, Engine: j.Engine, Epoch: j.Epoch, Options: res.Options, Fallback: j.Fallback}
	}
	return out, nil
}

// Complete indexes a capture and records the job outcome. Captures for an
// epoch that is being sealed are rejected and the job is requeued.
func (c *Coordinator) Complete(ctx context.Context, comp worker.Completion) error {
	c.seen(comp.Worker, 0)
	job, err := c.queue.Get(ctx, comp.JobID)
	if err != nil {
		return err
	}
	if comp.Capture != nil {
		if err := c.commit(ctx, *comp.Capture, comp.Key); err != nil {
			if errors.Is(err, worker.ErrEpochClosed) {
				if rerr := c.queue.Release(ctx, comp.JobID, "epoch closed during capture"); rerr != nil {
					c.log.Error("release job", "job", comp.JobID, "err", rerr)
				}
			}
			return err
		}
		c.Metrics.capture(job.Source, comp.Capture)
	}
	changed, err := c.queue.Finish(ctx, comp.JobID, queue.Outcome{Error: comp.Error, Retry: comp.Retry, Capture: comp.Key})
	if err != nil {
		return err
	}
	if comp.Error != "" {
		c.Metrics.failure(job.Source, comp.Reason)
	}
	if !changed {
		return nil
	}
	src := c.Sources()
	res := src.Resolve(job.Source)
	if comp.NeedsFallback && !job.Fallback && res.Options.Fallback != "" {
		if _, err := c.queue.Add(ctx, queue.NewJob{URL: job.URL, Source: job.Source, Storage: job.Storage, Engine: res.Options.Fallback,
			Priority: PriorityFallback, Fallback: true}); err != nil {
			return err
		}
		c.Metrics.fallback(job.Source)
	}
	if len(comp.Discovered) > 0 {
		fresh, err := c.idx.MarkSeen(ctx, job.Storage, comp.Discovered, c.Now())
		if err != nil {
			return err
		}
		var add []queue.NewJob
		for _, u := range fresh {
			add = append(add, queue.NewJob{URL: u, Source: job.Source, Storage: job.Storage, Engine: engine.HTTP, Priority: PriorityAttachment})
		}
		if _, err := c.queue.Add(ctx, add...); err != nil {
			return err
		}
	}
	return nil
}

// commit indexes a capture stored under key after checking the epoch is
// still open and the object exists, and stores its CDXJ next to it.
func (c *Coordinator) commit(ctx context.Context, cf recorder.CaptureFile, key string) error {
	if key != cf.Key() {
		return fmt.Errorf("coordinator: capture key %q does not match %q", key, cf.Key())
	}
	arc, err := c.lib.Archive(cf.Storage)
	if err != nil {
		return err
	}
	st, err := arc.Storage.Stat(ctx, key)
	if err != nil {
		return fmt.Errorf("coordinator: capture %s: %w", key, err)
	}
	if st.Size != cf.Size {
		return fmt.Errorf("coordinator: capture %s has %d bytes, reported %d", key, st.Size, cf.Size)
	}
	c.epochMu.Lock()
	defer c.epochMu.Unlock()
	info, err := c.idx.Epoch(ctx, cf.Storage, cf.Epoch)
	if err != nil {
		return err
	}
	if info.State != index.StateOpen {
		return fmt.Errorf("%w: %s is %s", worker.ErrEpochClosed, cf.Epoch, info.State)
	}
	if err := arc.WriteCaptureIndex(ctx, cf); err != nil {
		return fmt.Errorf("coordinator: index of %s: %w", key, err)
	}
	return c.idx.AddCapture(ctx, cf, key)
}

// Record writes a capture directly on the coordinator (uploads, imports).
// fn writes records; the file is stored in the named storage ("" is the
// default) in epoch ep (the current epoch when empty) and indexed.
func (c *Coordinator) Record(ctx context.Context, storageName string, ep epoch.ID, source, engineName string, fn func(*recorder.Recorder) error) (recorder.CaptureFile, error) {
	if ep == "" {
		ep = epoch.Of(c.Now())
	}
	storageName = index.StorageOr(storageName)
	arc, err := c.lib.Archive(storageName)
	if err != nil {
		return recorder.CaptureFile{}, err
	}
	rec, err := recorder.New(recorder.Options{
		Storage: storageName, Epoch: ep, Dedup: c.idx.Dedup(storageName), SpoolDir: arc.SpoolDir, Source: source,
		Engine: engineName, Worker: "coordinator", Now: c.Now,
	})
	if err != nil {
		return recorder.CaptureFile{}, err
	}
	if err := fn(rec); err != nil {
		rec.Abort()
		return recorder.CaptureFile{}, err
	}
	cf, err := rec.Close()
	if err != nil {
		return cf, err
	}
	defer os.Remove(cf.Path)
	f, err := os.Open(cf.Path)
	if err != nil {
		return cf, err
	}
	err = arc.Storage.Put(ctx, cf.Key(), f, cf.Size)
	f.Close()
	if err != nil {
		return cf, err
	}
	if err := c.commit(ctx, cf, cf.Key()); err != nil {
		return cf, err
	}
	c.Metrics.capture(source, &cf)
	return cf, nil
}

// URLRequest asks for an immediate capture of a URL.
type URLRequest struct {
	URL    string
	Source string
	Engine string
	// Storage overrides the source's storage.
	Storage string
}

// AddURL queues a URL immediately, bypassing the seen-URL filter. Engine and
// storage default to the source's settings.
func (c *Coordinator) AddURL(ctx context.Context, r URLRequest) (int64, error) {
	if err := config.ValidateURL(r.URL); err != nil {
		return 0, err
	}
	if r.Source == "" {
		r.Source = "api"
	}
	res := c.Sources().Resolve(r.Source)
	if r.Engine == "" {
		r.Engine = res.Engine
	}
	if r.Storage == "" {
		r.Storage = res.Storage
	}
	if !c.lib.Has(r.Storage) {
		return 0, fmt.Errorf("unknown storage %q", r.Storage)
	}
	if _, err := c.idx.MarkSeen(ctx, r.Storage, []string{r.URL}, c.Now()); err != nil {
		return 0, err
	}
	ids, err := c.queue.Add(ctx, queue.NewJob{URL: r.URL, Source: r.Source, Storage: r.Storage, Engine: r.Engine, Priority: PriorityAPI})
	if err != nil {
		return 0, err
	}
	return ids[0], nil
}

// PollSources discovers new URLs of sources whose interval elapsed (all
// enabled sources when force is set) and queues those not seen before.
func (c *Coordinator) PollSources(ctx context.Context, force bool) error {
	src := c.Sources()
	statuses, err := c.opts.Sources.Statuses(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, s := range src.Sources {
		if !s.IsEnabled() {
			continue
		}
		if last := statuses[s.Name].LastPoll; !force && !last.IsZero() && c.Now().Sub(last) < src.Resolve(s.Name).Poll {
			continue
		}
		if err := c.poll(ctx, src, s); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", s.Name, err))
		}
	}
	return errors.Join(errs...)
}

// PollSource polls one source now, also when it is disabled.
func (c *Coordinator) PollSource(ctx context.Context, name string) error {
	src := c.Sources()
	s, ok := src.Find(name)
	if !ok {
		return ErrUnknownSource
	}
	return c.poll(ctx, src, s)
}

// poll discovers URLs of a source, queues the new ones and records the
// result in the database.
func (c *Coordinator) poll(ctx context.Context, src *config.Sources, s config.Source) error {
	res := src.Resolve(s.Name)
	st := SourceStatus{LastPoll: c.Now()}
	var pollErr error
	if !c.lib.Has(res.Storage) {
		pollErr = fmt.Errorf("storage %q is not configured (HOARFROST_STORAGES)", res.Storage)
	} else {
		items, err := c.fetcher.Discover(ctx, s, res.MaxAge)
		pollErr = err
		urls := make([]string, len(items))
		for i, it := range items {
			urls[i] = it.URL
		}
		fresh, err := c.idx.MarkSeen(ctx, res.Storage, urls, c.Now())
		if err != nil {
			return err
		}
		// URLs seen but not captured yet are forgotten when the index is
		// rebuilt; the queue still has them.
		pending, err := c.queue.PendingOf(ctx, res.Storage, fresh)
		if err != nil {
			return err
		}
		var jobs []queue.NewJob
		for _, u := range fresh {
			if !pending[u] {
				jobs = append(jobs, queue.NewJob{URL: u, Source: s.Name, Storage: res.Storage, Engine: res.Engine, Priority: res.Priority})
			}
		}
		if _, err := c.queue.Add(ctx, jobs...); err != nil {
			return err
		}
		st.Found, st.New = len(items), len(jobs)
		if len(jobs) > 0 {
			c.log.Info("source polled", "source", s.Name, "found", len(items), "new", len(jobs))
		}
	}
	if pollErr != nil {
		st.Error = pollErr.Error()
		c.Metrics.pollError(s.Name)
	}
	if err := c.opts.Sources.RecordPoll(ctx, s.Name, st); err != nil {
		return err
	}
	return pollErr
}

// SourceStatuses returns the last poll result per source.
func (c *Coordinator) SourceStatuses(ctx context.Context) map[string]SourceStatus {
	st, err := c.opts.Sources.Statuses(ctx)
	if err != nil {
		c.log.Error("reading source statuses", "err", err)
		return map[string]SourceStatus{}
	}
	return st
}

// TestSource discovers URLs of a source, which need not be saved yet,
// without queueing them.
func (c *Coordinator) TestSource(ctx context.Context, s config.Source) (found, fresh int, err error) {
	trial := c.Sources().Clone()
	trial.Sources = slices.DeleteFunc(trial.Sources, func(x config.Source) bool { return x.Name == s.Name })
	trial.Sources = append(trial.Sources, s)
	res := trial.Resolve(s.Name)
	if !c.lib.Has(res.Storage) {
		return 0, 0, fmt.Errorf("storage %q is not configured (HOARFROST_STORAGES)", res.Storage)
	}
	items, err := c.fetcher.Discover(ctx, s, res.MaxAge)
	for _, it := range items {
		seen, serr := c.idx.Seen(ctx, res.Storage, it.URL)
		if serr != nil {
			return len(items), fresh, serr
		}
		if !seen {
			fresh++
		}
	}
	return len(items), fresh, err
}

// UpdateSources validates, saves and archives a new capture configuration.
func (c *Coordinator) UpdateSources(ctx context.Context, s *config.Sources) error {
	c.srcMu.Lock()
	defer c.srcMu.Unlock()
	return c.updateSources(ctx, s)
}

func (c *Coordinator) updateSources(ctx context.Context, s *config.Sources) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if err := c.checkStorages(s); err != nil {
		return err
	}
	if err := c.opts.Sources.Save(ctx, s); err != nil {
		return err
	}
	c.sources.Store(s)
	c.configureBlocklists(s)
	return c.SnapshotSources(ctx)
}

// changeSources applies fn to a copy of the configuration and saves it.
func (c *Coordinator) changeSources(ctx context.Context, fn func(next *config.Sources) error) error {
	c.srcMu.Lock()
	defer c.srcMu.Unlock()
	next := c.Sources().Clone()
	if err := fn(next); err != nil {
		return err
	}
	return c.updateSources(ctx, next)
}

// SaveSource adds a source or replaces the one with the same name.
func (c *Coordinator) SaveSource(ctx context.Context, src config.Source) error {
	return c.changeSources(ctx, func(next *config.Sources) error {
		for i := range next.Sources {
			if next.Sources[i].Name == src.Name {
				next.Sources[i] = src
				return nil
			}
		}
		next.Sources = append(next.Sources, src)
		sort.Slice(next.Sources, func(i, j int) bool { return next.Sources[i].Name < next.Sources[j].Name })
		return nil
	})
}

// DeleteSource removes a source. Its archived pages and queued jobs stay.
func (c *Coordinator) DeleteSource(ctx context.Context, name string) error {
	return c.changeSources(ctx, func(next *config.Sources) error {
		for i := range next.Sources {
			if next.Sources[i].Name == name {
				next.Sources = append(next.Sources[:i], next.Sources[i+1:]...)
				return nil
			}
		}
		return ErrUnknownSource
	})
}

// SetSourceEnabled enables or disables polling of a source.
func (c *Coordinator) SetSourceEnabled(ctx context.Context, name string, enabled bool) error {
	return c.changeSources(ctx, func(next *config.Sources) error {
		for i := range next.Sources {
			if next.Sources[i].Name == name {
				next.Sources[i].Enabled = &enabled
				return nil
			}
		}
		return ErrUnknownSource
	})
}

// SetDefaults replaces the settings every source inherits.
func (c *Coordinator) SetDefaults(ctx context.Context, d config.Defaults) error {
	return c.changeSources(ctx, func(next *config.Sources) error {
		next.Defaults = d
		return nil
	})
}

// SetBlocklists replaces the block list settings; nil restores the default
// list.
func (c *Coordinator) SetBlocklists(ctx context.Context, b *config.Blocklists) error {
	return c.changeSources(ctx, func(next *config.Sources) error {
		next.Blocklists = b
		return nil
	})
}

// SnapshotSources stores the current configuration in every storage if it
// differs from the latest copy there, so each archive documents how it was
// collected.
func (c *Coordinator) SnapshotSources(ctx context.Context) error {
	b, err := c.Sources().Marshal()
	if err != nil || len(c.Sources().Sources) == 0 {
		return err
	}
	for _, arc := range c.lib.All() {
		objs, err := arc.Storage.List(ctx, "config/sources/")
		if err != nil {
			return err
		}
		if len(objs) > 0 {
			last, err := storage.ReadAll(ctx, arc.Storage, objs[len(objs)-1].Key)
			if err == nil && string(last) == string(b) {
				continue
			}
		}
		if err := storage.PutBytes(ctx, arc.Storage, config.SnapshotKey(c.Now()), b); err != nil {
			return err
		}
	}
	return nil
}

// ErrActiveLeases is returned when an epoch still has captures running.
var ErrActiveLeases = errors.New("coordinator: epoch has active leases")

// Seal closes an epoch of a storage. Unless force is set it refuses while
// leases of the epoch are active.
func (c *Coordinator) Seal(ctx context.Context, storageName string, ep epoch.ID, force bool) (*archive.Catalog, error) {
	arc, err := c.lib.Archive(storageName)
	if err != nil {
		return nil, err
	}
	storageName = index.StorageOr(storageName)
	c.epochMu.Lock()
	n, err := c.queue.ActiveLeases(ctx, storageName, ep)
	if err != nil {
		c.epochMu.Unlock()
		return nil, err
	}
	if n > 0 && !force {
		c.epochMu.Unlock()
		return nil, fmt.Errorf("%w: %d", ErrActiveLeases, n)
	}
	if err := c.idx.SetEpoch(ctx, index.EpochInfo{Storage: storageName, Epoch: ep, State: index.StateSealing}); err != nil {
		c.epochMu.Unlock()
		return nil, err
	}
	c.epochMu.Unlock()
	cat, err := arc.Seal(ctx, ep)
	key := storageName + "/" + string(ep)
	c.mu.Lock()
	if err != nil {
		c.sealErr[key] = err.Error()
	} else {
		delete(c.sealErr, key)
	}
	c.mu.Unlock()
	return cat, err
}

// SealDue seals every open epoch whose week ended more than the grace
// period ago and has no active leases.
func (c *Coordinator) SealDue(ctx context.Context) error {
	eps, err := c.idx.Epochs(ctx)
	if err != nil {
		return err
	}
	now := c.Now()
	var errs []error
	for _, e := range eps {
		if e.State != index.StateOpen || now.Before(e.Epoch.End().Add(c.opts.SealGrace)) {
			continue
		}
		if _, err := c.Seal(ctx, e.Storage, e.Epoch, false); err != nil && !errors.Is(err, ErrActiveLeases) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Start prepares storage and the index: README, configuration snapshot and
// rebuilding of missing index parts.
func (c *Coordinator) Start(ctx context.Context) error {
	for _, arc := range c.lib.All() {
		if err := arc.WriteReadme(ctx); err != nil {
			return fmt.Errorf("coordinator: readme of %s: %w", arc.Name, err)
		}
	}
	if err := c.SnapshotSources(ctx); err != nil {
		return fmt.Errorf("coordinator: config snapshot: %w", err)
	}
	for _, arc := range c.lib.All() {
		rep, err := arc.Reindex(ctx, archive.ReindexOptions{})
		if err != nil {
			return fmt.Errorf("coordinator: reindex of %s: %w", arc.Name, err)
		}
		if len(rep.Epochs) > 0 {
			c.log.Info("index rebuilt from storage", "storage", arc.Name, "epochs", rep.Epochs, "captures", rep.Captures)
		}
	}
	return nil
}

// writeMissingIndexes adds CDXJ files to incoming captures stored without
// one and page tables to sealed weeks without one; it runs in the
// background because older weeks may hold many files.
func (c *Coordinator) writeMissingIndexes(ctx context.Context) {
	for _, arc := range c.lib.All() {
		n, err := arc.WriteMissingIndexes(ctx)
		if err != nil && ctx.Err() == nil {
			c.log.Warn("writing missing capture indexes", "storage", arc.Name, "err", err)
		}
		if n > 0 {
			c.log.Info("capture indexes written", "storage", arc.Name, "files", n)
		}
		eps, err := arc.WriteMissingParquet(ctx)
		if err != nil && ctx.Err() == nil {
			c.log.Warn("writing missing page tables", "storage", arc.Name, "err", err)
		}
		if len(eps) > 0 {
			c.log.Info("page tables written", "storage", arc.Name, "epochs", eps)
		}
	}
}

// Run executes periodic tasks until ctx is cancelled.
func (c *Coordinator) Run(ctx context.Context, tick time.Duration) {
	if tick == 0 {
		tick = 30 * time.Second
	}
	go c.writeMissingIndexes(ctx)
	t := time.NewTicker(tick)
	defer t.Stop()
	var lastCleanup time.Time
	for {
		if u := c.blocks.Load(); u.Due() {
			if err := u.Update(ctx); err != nil && ctx.Err() == nil {
				c.log.Warn("block lists", "err", err)
			}
		}
		if err := c.PollSources(ctx, false); err != nil && ctx.Err() == nil {
			c.log.Warn("source poll", "err", err)
		}
		if err := c.SealDue(ctx); err != nil && ctx.Err() == nil {
			c.log.Error("seal", "err", err)
		}
		if c.Now().Sub(lastCleanup) > time.Hour {
			lastCleanup = c.Now()
			for _, arc := range c.lib.All() {
				if _, err := arc.Cleanup(ctx); err != nil && ctx.Err() == nil {
					c.log.Error("cleanup", "storage", arc.Name, "err", err)
				}
			}
			if _, err := c.queue.Purge(ctx, 90*24*time.Hour); err != nil && ctx.Err() == nil {
				c.log.Error("queue purge", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// SealErrors returns the last seal error per "storage/epoch".
func (c *Coordinator) SealErrors() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]string{}
	for k, v := range c.sealErr {
		out[k] = v
	}
	return out
}
