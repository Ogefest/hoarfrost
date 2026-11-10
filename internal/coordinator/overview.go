package coordinator

import (
	"context"
	"sync"
	"time"

	"github.com/ogefest/hoarfrost/internal/index"
	"github.com/ogefest/hoarfrost/internal/queue"
)

// overviewDays is the length of the per-day series in the overview.
const overviewDays = 30

// Overview is a snapshot of archive and queue totals for the dashboard, the
// sources page and metrics. Computing it aggregates the whole pages and jobs
// tables (seconds on a large archive), so it is cached for
// Options.OverviewTTL and refreshed in the background.
type Overview struct {
	Index index.Stats
	Queue map[string]int
	// QueueBySource counts the unfinished jobs of every source.
	QueueBySource map[string]queue.SourceCounts
	At            time.Time
}

type overviewCache struct {
	mu      sync.Mutex
	cur     *Overview
	err     error
	pending chan struct{} // closed when the running refresh ends
}

// Overview returns the cached totals. A stale snapshot is returned at once
// while a refresh runs in the background; only the first call waits.
// Callers must not modify the returned slices and maps.
func (c *Coordinator) Overview(ctx context.Context) (Overview, error) {
	oc := &c.overview
	oc.mu.Lock()
	cur := oc.cur
	if cur != nil && c.Now().Sub(cur.At) < c.opts.OverviewTTL {
		oc.mu.Unlock()
		return *cur, nil
	}
	done := oc.pending
	if done == nil {
		done = make(chan struct{})
		oc.pending = done
		go c.refreshOverview(done)
	}
	oc.mu.Unlock()
	if cur != nil {
		return *cur, nil
	}
	select {
	case <-done:
	case <-ctx.Done():
		return Overview{}, ctx.Err()
	}
	oc.mu.Lock()
	defer oc.mu.Unlock()
	if oc.cur == nil {
		return Overview{}, oc.err
	}
	return *oc.cur, nil
}

func (c *Coordinator) refreshOverview(done chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	o := Overview{At: c.Now()}
	st, err := c.idx.Stats(ctx, overviewDays)
	if err == nil {
		o.Index = st
		o.Queue, err = c.queue.Counts(ctx)
	}
	if err == nil {
		o.QueueBySource, err = c.queue.CountsBySource(ctx)
	}
	oc := &c.overview
	oc.mu.Lock()
	if err == nil {
		oc.cur = &o
	} else {
		c.log.Warn("computing overview", "err", err)
	}
	oc.err = err
	oc.pending = nil
	oc.mu.Unlock()
	close(done)
}
