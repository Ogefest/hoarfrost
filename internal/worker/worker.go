// Package worker runs captures: it leases jobs from the coordinator,
// captures them with an engine, uploads the WARC file to incoming/ and
// reports the result. Workers are stateless and may come and go at any time.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ogefest/hoarfrost/internal/blocklist"
	"github.com/ogefest/hoarfrost/internal/engine"
	"github.com/ogefest/hoarfrost/internal/engine/static"
	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/recorder"
	"github.com/ogefest/hoarfrost/internal/storage"
)

// LeaseRequest asks the coordinator for work.
type LeaseRequest struct {
	Worker   string   `json:"worker"`
	Engines  []string `json:"engines"`
	Storages []string `json:"storages,omitempty"`
	Max      int      `json:"max"`
}

// Completion reports a finished capture attempt.
type Completion struct {
	JobID   int64                 `json:"jobId"`
	Worker  string                `json:"worker"`
	Key     string                `json:"key,omitempty"`
	Capture *recorder.CaptureFile `json:"capture,omitempty"`
	Error   string                `json:"error,omitempty"`
	// Reason classifies Error for metrics (Reason* constants or http_<status>).
	Reason string `json:"reason,omitempty"`
	Retry  bool   `json:"retry,omitempty"`
	// NeedsFallback asks for a new capture with the source's fallback engine.
	NeedsFallback bool     `json:"needsFallback,omitempty"`
	Discovered    []string `json:"discovered,omitempty"`
}

// ErrEpochClosed is returned by Complete when the capture's epoch is being
// or has been sealed; the job is returned to the queue.
var ErrEpochClosed = errors.New("epoch closed")

// Coordinator is the worker's view of the coordinator.
type Coordinator interface {
	Lease(ctx context.Context, r LeaseRequest) ([]engine.Job, error)
	Complete(ctx context.Context, c Completion) error
	// Lookup finds a committed record with the digest in a storage and epoch.
	Lookup(ctx context.Context, storage string, ep epoch.ID, digest string) (*recorder.Ref, error)
	// Blocklist returns the current global block lists.
	Blocklist(ctx context.Context) (*blocklist.Set, error)
}

// Worker captures jobs in parallel slots.
type Worker struct {
	Name  string
	Coord Coordinator
	// Storages maps storage names to backends; jobs name their storage.
	Storages map[string]storage.Storage
	Engines  engine.Registry
	SpoolDir string
	Slots    int
	Idle     time.Duration
	Log      *slog.Logger
	// Recorder customizes recorder options (tests, TLS settings).
	Recorder func(*recorder.Options)

	mu     sync.Mutex
	active map[int64]string
	blocks *blocklist.Set
}

// blocklist returns the global block lists in the version a job requires,
// fetching them from the coordinator when the cached copy is outdated.
func (w *Worker) blocklist(ctx context.Context, version string) (*blocklist.Set, error) {
	w.mu.Lock()
	cur := w.blocks
	w.mu.Unlock()
	if version == "" {
		return blocklist.Empty(), nil
	}
	if cur != nil && cur.Version() == version {
		return cur, nil
	}
	set, err := w.Coord.Blocklist(ctx)
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	w.blocks = set
	w.mu.Unlock()
	return set, nil
}

// blockRule combines the source's own block list with the global lists.
// The host of the archived page itself is never blocked by global lists.
func blockRule(job engine.Job, set *blocklist.Set) func(string) bool {
	pageHost := ""
	if u, err := url.Parse(job.URL); err == nil {
		pageHost = strings.ToLower(u.Hostname())
	}
	return func(host string) bool {
		if static.HostMatches(host, job.Options.Block) {
			return true
		}
		return !strings.EqualFold(host, pageHost) && set.Blocked(host)
	}
}

// DefaultEngines returns the engines available without a browser.
func DefaultEngines() engine.Registry {
	return engine.Registry{engine.HTTP: engine.HTTPEngine{}, engine.Static: static.Engine{}}
}

// Active returns the URLs being captured.
func (w *Worker) Active() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, u := range w.active {
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}

// Run processes jobs until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	if w.Slots <= 0 {
		w.Slots = 1
	}
	if w.Idle == 0 {
		w.Idle = 5 * time.Second
	}
	if w.Log == nil {
		w.Log = slog.Default()
	}
	w.active = map[int64]string{}
	var wg sync.WaitGroup
	for i := range w.Slots {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			w.loop(ctx, slot)
		}(i)
	}
	wg.Wait()
	return nil
}

func (w *Worker) loop(ctx context.Context, slot int) {
	log := w.Log.With("worker", w.Name, "slot", slot)
	for ctx.Err() == nil {
		jobs, err := w.Coord.Lease(ctx, LeaseRequest{Worker: w.Name, Engines: w.Engines.Names(), Storages: w.storageNames(), Max: 1})
		if err != nil {
			if ctx.Err() == nil {
				log.Warn("lease failed", "err", err)
			}
			sleep(ctx, w.Idle)
			continue
		}
		if len(jobs) == 0 {
			sleep(ctx, w.Idle+time.Duration(rand.Int64N(int64(w.Idle/2)+1)))
			continue
		}
		for _, job := range jobs {
			w.process(ctx, job, log)
		}
	}
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// RunOnce leases and processes at most one job; it reports whether a job
// was processed.
func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	if w.Log == nil {
		w.Log = slog.Default()
	}
	if w.active == nil {
		w.active = map[int64]string{}
	}
	jobs, err := w.Coord.Lease(ctx, LeaseRequest{Worker: w.Name, Engines: w.Engines.Names(), Storages: w.storageNames(), Max: 1})
	if err != nil || len(jobs) == 0 {
		return false, err
	}
	for _, j := range jobs {
		w.process(ctx, j, w.Log)
	}
	return true, nil
}

func (w *Worker) process(ctx context.Context, job engine.Job, log *slog.Logger) {
	log = log.With("job", job.ID, "url", job.URL, "engine", job.Engine)
	w.mu.Lock()
	w.active[job.ID] = job.URL
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		delete(w.active, job.ID)
		w.mu.Unlock()
	}()

	c := w.capture(ctx, job)
	if ctx.Err() != nil && c.Key == "" {
		return
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if err := w.Coord.Complete(cctx, c); err != nil {
		if errors.Is(err, ErrEpochClosed) {
			log.Warn("capture rejected, epoch closed", "key", c.Key)
		} else {
			log.Error("complete failed", "err", err)
		}
		return
	}
	if c.Error != "" {
		log.Warn("capture failed", "err", c.Error)
	} else {
		log.Info("captured", "key", c.Key, "records", len(c.Capture.Records))
	}
}

func (w *Worker) capture(ctx context.Context, job engine.Job) Completion {
	c := Completion{JobID: job.ID, Worker: w.Name}
	eng, err := w.Engines.Get(job.Engine)
	if err != nil {
		c.Error, c.Reason = err.Error(), ReasonEngine
		return c
	}
	if job.Epoch == "" {
		job.Epoch = epoch.Of(time.Now())
	}
	if job.Storage == "" {
		job.Storage = DefaultStorage
	}
	if _, err := w.storage(job.Storage); err != nil {
		c.Error, c.Reason = err.Error(), ReasonStorage
		return c
	}
	set, err := w.blocklist(ctx, job.Options.BlocklistVersion)
	if err != nil {
		c.Error, c.Reason, c.Retry = fmt.Sprintf("block lists: %v", err), ReasonBlocklist, true
		return c
	}
	job.Blocked = blockRule(job, set)
	opts := recorder.Options{
		Storage: job.Storage, Epoch: job.Epoch, Dedup: dedupIn{w.Coord, job.Storage}, SpoolDir: w.SpoolDir, Source: job.Source, Engine: job.Engine,
		Worker: w.Name, UserAgent: job.Options.UserAgent, MaxResponseSize: job.Options.MaxResponseSize,
		Blocked: job.Blocked,
	}
	if set.Len() > 0 {
		opts.Info = map[string]string{"blocklist": set.Version()}
	}
	if job.Options.Timeout > 0 {
		opts.RequestTimeout = job.Options.Timeout
	}
	if w.Recorder != nil {
		w.Recorder(&opts)
	}
	rec, err := recorder.New(opts)
	if err != nil {
		c.Error, c.Reason, c.Retry = err.Error(), ReasonEngine, true
		return c
	}
	res, capErr := eng.Capture(ctx, job, rec)
	var se *engine.StatusError
	if capErr != nil && !errors.As(capErr, &se) {
		rec.Abort()
		c.Error, c.Reason, c.Retry = capErr.Error(), reasonOf(capErr), ctx.Err() == nil
		return c
	}
	cf, err := rec.Close()
	if err != nil {
		rec.Abort()
		c.Error, c.Reason, c.Retry = err.Error(), ReasonEngine, true
		return c
	}
	defer os.Remove(cf.Path)
	if err := w.upload(ctx, cf); err != nil {
		c.Error, c.Reason, c.Retry = fmt.Sprintf("upload: %v", err), ReasonStorage, true
		return c
	}
	c.Key, c.Capture = cf.Key(), &cf
	c.NeedsFallback, c.Discovered = res.NeedsFallback, res.Discovered
	if se != nil {
		c.Error, c.Reason, c.Retry = se.Error(), reasonOf(se), se.Status >= 500 || se.Status == 429
	}
	return c
}

func (w *Worker) upload(ctx context.Context, cf recorder.CaptureFile) error {
	st, err := w.storage(cf.Storage)
	if err != nil {
		return err
	}
	f, err := os.Open(cf.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	return st.Put(ctx, cf.Key(), f, cf.Size)
}

func (w *Worker) storage(name string) (storage.Storage, error) {
	if name == "" {
		name = DefaultStorage
	}
	if st, ok := w.Storages[name]; ok {
		return st, nil
	}
	return nil, fmt.Errorf("worker: storage %q is not configured on this worker", name)
}

func (w *Worker) storageNames() []string {
	names := make([]string, 0, len(w.Storages))
	for n := range w.Storages {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// DefaultStorage names the storage used by jobs without one.
const DefaultStorage = "default"

// dedupIn limits deduplication to one storage.
type dedupIn struct {
	c       Coordinator
	storage string
}

func (d dedupIn) Lookup(ctx context.Context, ep epoch.ID, digest string) (*recorder.Ref, error) {
	return d.c.Lookup(ctx, d.storage, ep, digest)
}
