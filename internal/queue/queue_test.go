package queue

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ogefest/hoarfrost/internal/pgdb/pgtest"

	"github.com/ogefest/hoarfrost/internal/appdb"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func newQueue(t *testing.T) (*Queue, *clock) {
	appURL, appSchema := pgtest.Schema(t, "app")
	db, err := appdb.Open(context.Background(), appURL, appSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	c := &clock{t: time.Date(2026, 10, 11, 23, 59, 0, 0, time.UTC)}
	q := New(db)
	q.Now = c.now
	return q, c
}

var req = LeaseRequest{Worker: "w1", Engines: []string{"static", "http"}, Max: 10, MaxPerDomain: 10, Duration: 10 * time.Minute}

func TestLeaseOrderEpochAndEngines(t *testing.T) {
	q, _ := newQueue(t)
	ctx := context.Background()
	q.Add(ctx, NewJob{URL: "https://a.pl/1", Source: "a", Engine: "static"},
		NewJob{URL: "https://b.pl/1", Source: "b", Engine: "chrome"},
		NewJob{URL: "https://a.pl/urgent", Source: "api", Engine: "static", Priority: 10})
	jobs, err := q.Lease(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || jobs[0].URL != "https://a.pl/urgent" || jobs[0].State != Leased || jobs[0].Attempts != 1 {
		t.Fatalf("jobs %+v", jobs)
	}
	if jobs[0].Epoch != "2026-W41" || jobs[0].Domain != "a.pl" {
		t.Fatalf("epoch/domain %+v", jobs[0])
	}
	chrome, _ := q.Lease(ctx, LeaseRequest{Worker: "w2", Engines: []string{"chrome"}, Duration: time.Minute})
	if len(chrome) != 1 || chrome[0].URL != "https://b.pl/1" {
		t.Fatalf("chrome %+v", chrome)
	}
	if _, err := q.Lease(ctx, LeaseRequest{}); err == nil {
		t.Fatal("lease without engines must fail")
	}
}

func TestPerDomainLimit(t *testing.T) {
	q, _ := newQueue(t)
	ctx := context.Background()
	for i := range 5 {
		q.Add(ctx, NewJob{URL: fmt.Sprintf("https://a.pl/%d", i), Engine: "static"})
	}
	q.Add(ctx, NewJob{URL: "https://b.pl/1", Engine: "static"})
	r := req
	r.MaxPerDomain = 2
	jobs, _ := q.Lease(ctx, r)
	if len(jobs) != 3 {
		t.Fatalf("leased %d", len(jobs))
	}
	more, _ := q.Lease(ctx, r)
	if len(more) != 0 {
		t.Fatal("domain limit exceeded")
	}
	q.Finish(ctx, jobs[0].ID, Outcome{})
	more, _ = q.Lease(ctx, r)
	if len(more) != 1 || more[0].Domain != "a.pl" {
		t.Fatalf("after finish %+v", more)
	}
}

func TestLeaseExpiryAndEpochAtLease(t *testing.T) {
	q, c := newQueue(t)
	ctx := context.Background()
	q.Add(ctx, NewJob{URL: "https://a.pl/1", Engine: "static"})
	jobs, _ := q.Lease(ctx, req)
	if jobs[0].Epoch != "2026-W41" {
		t.Fatal(jobs[0].Epoch)
	}
	if n, _ := q.ActiveLeases(ctx, "default", "2026-W41"); n != 1 {
		t.Fatal("active leases")
	}
	c.add(5 * time.Minute)
	if again, _ := q.Lease(ctx, req); len(again) != 0 {
		t.Fatal("unexpired lease handed out twice")
	}
	c.add(6 * time.Minute)
	if n, _ := q.ActiveLeases(ctx, "default", "2026-W41"); n != 0 {
		t.Fatal("expired lease still active")
	}
	again, _ := q.Lease(ctx, LeaseRequest{Worker: "w2", Engines: []string{"static"}, Duration: time.Minute})
	if len(again) != 1 || again[0].Worker != "w2" || again[0].Attempts != 2 || again[0].Epoch != "2026-W42" {
		t.Fatalf("re-lease %+v", again)
	}
	c.add(2 * time.Minute)
	q.Lease(ctx, req)
	c.add(11 * time.Minute)
	if got, _ := q.Lease(ctx, req); len(got) != 0 {
		t.Fatal("job exceeded max attempts but was leased")
	}
	j, _ := q.Get(ctx, jobs[0].ID)
	if j.State != Failed || j.LastError == "" {
		t.Fatalf("job %+v", j)
	}
}

func TestFinishFirstWinsAndRetry(t *testing.T) {
	q, c := newQueue(t)
	ctx := context.Background()
	ids, _ := q.Add(ctx, NewJob{URL: "https://a.pl/1", Engine: "static"})
	q.Lease(ctx, req)
	if ok, err := q.Finish(ctx, ids[0], Outcome{Capture: "incoming/x"}); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if ok, _ := q.Finish(ctx, ids[0], Outcome{Error: "late"}); ok {
		t.Fatal("second completion must not change a done job")
	}
	j, _ := q.Get(ctx, ids[0])
	if j.State != Done || j.Capture != "incoming/x" {
		t.Fatalf("%+v", j)
	}

	ids, _ = q.Add(ctx, NewJob{URL: "https://a.pl/2", Engine: "static"})
	q.Lease(ctx, req)
	q.Finish(ctx, ids[0], Outcome{Error: "timeout", Retry: true})
	j, _ = q.Get(ctx, ids[0])
	if j.State != Queued || j.LastError != "timeout" {
		t.Fatalf("%+v", j)
	}
	if got, _ := q.Lease(ctx, req); len(got) != 0 {
		t.Fatal("backoff ignored")
	}
	c.add(6 * time.Minute)
	got, _ := q.Lease(ctx, req)
	if len(got) != 1 {
		t.Fatal("job not retried after backoff")
	}
	q.Finish(ctx, ids[0], Outcome{Error: "HTTP 404", Retry: false})
	j, _ = q.Get(ctx, ids[0])
	if j.State != Failed {
		t.Fatal(j.State)
	}
	if err := q.Retry(ctx, ids[0]); err != nil {
		t.Fatal(err)
	}
	j, _ = q.Get(ctx, ids[0])
	if j.State != Queued || j.Attempts != 0 {
		t.Fatalf("%+v", j)
	}
	if err := q.Retry(ctx, ids[0]); err != ErrNotFound {
		t.Fatal("retrying a queued job")
	}
	if _, err := q.Finish(ctx, 999, Outcome{}); err != ErrNotFound {
		t.Fatal(err)
	}
}

func TestReleaseCountsAndList(t *testing.T) {
	q, c := newQueue(t)
	ctx := context.Background()
	ids, _ := q.Add(ctx, NewJob{URL: "https://a.pl/1", Engine: "static"}, NewJob{URL: "https://a.pl/2", Engine: "static"})
	q.Lease(ctx, req)
	q.Release(ctx, ids[0], "shutdown")
	j, _ := q.Get(ctx, ids[0])
	if j.State != Queued || j.Attempts != 0 || j.Epoch != "" {
		t.Fatalf("%+v", j)
	}
	q.Finish(ctx, ids[1], Outcome{})
	counts, _ := q.Counts(ctx)
	if counts[Queued] != 1 || counts[Done] != 1 || counts[Failed] != 0 {
		t.Fatal(counts)
	}
	if p, _ := q.Pending(ctx, "https://a.pl/1"); !p {
		t.Fatal("pending")
	}
	list, _ := q.List(ctx, "", 10, 0)
	if len(list) != 2 {
		t.Fatal(len(list))
	}
	c.add(48 * time.Hour)
	if n, _ := q.Purge(ctx, 24*time.Hour); n != 1 {
		t.Fatal("purge")
	}
}

// TestConcurrentLeases checks that concurrent workers never receive the
// same job twice and that every job is handed out.
func TestConcurrentLeases(t *testing.T) {
	q, _ := newQueue(t)
	ctx := context.Background()
	const n = 200
	for i := range n {
		q.Add(ctx, NewJob{URL: fmt.Sprintf("https://d%d.pl/%d", i%7, i), Engine: "static"})
	}
	var mu sync.Mutex
	seen := map[int64]string{}
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			for {
				jobs, err := q.Lease(ctx, LeaseRequest{Worker: name, Engines: []string{"static"}, Max: 3, MaxPerDomain: 3, Duration: time.Hour})
				if err != nil {
					t.Error(err)
					return
				}
				if len(jobs) == 0 {
					return
				}
				for _, j := range jobs {
					mu.Lock()
					if prev, dup := seen[j.ID]; dup {
						t.Errorf("job %d leased to %s and %s", j.ID, prev, name)
					}
					seen[j.ID] = name
					mu.Unlock()
					q.Finish(ctx, j.ID, Outcome{})
				}
			}
		}(fmt.Sprintf("w%d", w))
	}
	wg.Wait()
	if len(seen) != n {
		t.Fatalf("leased %d of %d", len(seen), n)
	}
}

func TestLeaseByStorage(t *testing.T) {
	q, _ := newQueue(t)
	ctx := context.Background()
	q.Add(ctx, NewJob{URL: "https://a.pl/1", Engine: "static", Storage: "prasa"}, NewJob{URL: "https://a.pl/2", Engine: "static"})
	r := req
	r.Storages = []string{"default"}
	jobs, _ := q.Lease(ctx, r)
	if len(jobs) != 1 || jobs[0].Storage != "default" {
		t.Fatalf("%+v", jobs)
	}
	r.Storages = nil
	jobs, _ = q.Lease(ctx, r)
	if len(jobs) != 1 || jobs[0].Storage != "prasa" {
		t.Fatalf("%+v", jobs)
	}
}

// TestSourcesTakeTurns: a long backlog of one source does not hold up a
// source added later, and priority still comes first.
func TestSourcesTakeTurns(t *testing.T) {
	q, c := newQueue(t)
	ctx := context.Background()
	for i := range 50 {
		q.Add(ctx, NewJob{URL: fmt.Sprintf("https://d%d.duzy.pl/%d", i%5, i), Source: "sitemap", Engine: "static"})
	}
	c.add(time.Minute)
	q.Add(ctx, NewJob{URL: "https://rss.pl/1", Source: "rss", Engine: "static"},
		NewJob{URL: "https://rss.pl/2", Source: "rss", Engine: "static"})
	c.add(time.Minute)
	q.Add(ctx, NewJob{URL: "https://inny.pl/teraz", Source: "api", Engine: "static", Priority: 10})
	r := req
	r.Max = 1
	var got []string
	for range 6 {
		c.add(time.Second)
		jobs, err := q.Lease(ctx, r)
		if err != nil || len(jobs) != 1 {
			t.Fatalf("lease %v %v", jobs, err)
		}
		got = append(got, jobs[0].Source)
	}
	if want := "[api sitemap rss sitemap rss sitemap]"; fmt.Sprint(got) != want {
		t.Fatalf("order %v, want %s", got, want)
	}

	// Within a source the oldest job goes first; full domains are skipped.
	jobs, _ := q.List(ctx, Leased, 10, 0)
	var sitemap []string
	for _, j := range jobs {
		if j.Source == "sitemap" {
			sitemap = append(sitemap, j.URL)
		}
	}
	slices.Sort(sitemap)
	if want := "[https://d0.duzy.pl/0 https://d1.duzy.pl/1 https://d2.duzy.pl/2]"; fmt.Sprint(sitemap) != want {
		t.Fatalf("sitemap jobs %v", sitemap)
	}
	r.MaxPerDomain, r.Max = 1, 10
	more, _ := q.Lease(ctx, r)
	if len(more) != 2 || more[0].Domain == more[1].Domain {
		t.Fatalf("per-domain limit: %+v", more)
	}
}
