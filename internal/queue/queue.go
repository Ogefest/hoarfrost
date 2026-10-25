// Package queue stores capture jobs and hands them out to workers under
// time-limited leases, respecting a per-domain concurrency limit.
package queue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/pgdb"
)

// Job states.
const (
	Queued = "queued"
	Leased = "leased"
	Done   = "done"
	Failed = "failed"
)

// Job is a row of the jobs table.
type Job struct {
	ID         int64
	URL        string
	Domain     string
	Source     string
	Storage    string
	Engine     string
	Priority   int
	Fallback   bool
	State      string
	Worker     string
	Epoch      epoch.ID
	LeaseUntil time.Time
	Attempts   int
	LastError  string
	Capture    string
	Created    time.Time
	Finished   time.Time
}

// NewJob describes a job to enqueue.
type NewJob struct {
	URL      string
	Source   string
	Storage  string
	Engine   string
	Priority int
	Fallback bool
}

// Queue is the job queue in app.sqlite.
type Queue struct {
	db          *sql.DB
	mu          sync.Mutex
	Now         func() time.Time
	MaxAttempts int
	// Backoff is the delay before a failed job is retried (multiplied by attempts).
	Backoff time.Duration

	servedMu sync.Mutex
	served   map[string]time.Time // last lease per source, for taking turns
}

// New wraps an app.sqlite connection.
func New(db *sql.DB) *Queue {
	return &Queue{db: db, Now: time.Now, MaxAttempts: 3, Backoff: 5 * time.Minute, served: map[string]time.Time{}}
}

// Domain returns the lower-cased host of a URL.
func Domain(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func (q *Queue) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Add enqueues jobs and returns their IDs.
func (q *Queue) Add(ctx context.Context, jobs ...NewJob) ([]int64, error) {
	var ids []int64
	now := q.Now().UTC()
	err := q.tx(ctx, func(tx *sql.Tx) error {
		ids = ids[:0]
		for _, j := range jobs {
			if j.Priority == 0 {
				j.Priority = 100
			}
			if j.Storage == "" {
				j.Storage = "default"
			}
			var id int64
			if err := tx.QueryRowContext(ctx, `INSERT INTO jobs(url, domain, source, storage, engine, priority, fallback, created)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`, j.URL, Domain(j.URL), j.Source, j.Storage, j.Engine, j.Priority, j.Fallback, now).Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return nil
	})
	return ids, err
}

// LeaseRequest asks for jobs.
type LeaseRequest struct {
	Worker  string
	Engines []string
	// Storages limits jobs to storages the worker can write (all when empty).
	Storages     []string
	Max          int
	MaxPerDomain int
	Duration     time.Duration
}

// Lease hands out up to Max jobs. A job whose lease expired is handed out
// again first; after MaxAttempts expirations it fails. Among queued jobs the
// lowest priority number wins; within a priority, sources take turns: the
// source with the fewest running jobs, then the one served least recently,
// gets its oldest job. A long backlog of one source (a large sitemap) thus
// does not hold up others (a feed with today's articles). The epoch of a
// capture is fixed at lease time.
func (q *Queue) Lease(ctx context.Context, r LeaseRequest) ([]Job, error) {
	if r.Max <= 0 {
		r.Max = 1
	}
	if r.MaxPerDomain <= 0 {
		r.MaxPerDomain = 2
	}
	if len(r.Engines) == 0 {
		return nil, errors.New("queue: no engines in lease request")
	}
	now := q.Now()
	var out []Job
	err := q.tx(ctx, func(tx *sql.Tx) error {
		out = out[:0]
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET state = 'failed', finished = $1, last_error = 'lease expired ' || attempts || ' times'
			WHERE state = 'leased' AND lease_until < $2 AND attempts >= $3`, now, now, q.MaxAttempts); err != nil {
			return err
		}
		for len(out) < r.Max {
			id, err := q.pick(ctx, tx, r, now)
			if err != nil {
				return err
			}
			if id == 0 {
				break
			}
			ep := epoch.Of(now)
			row := tx.QueryRowContext(ctx, `UPDATE jobs SET state = 'leased', worker = $1, epoch = $2, lease_until = $3, attempts = attempts + 1
				WHERE id = $4 RETURNING `+jobCols, r.Worker, string(ep), now.Add(r.Duration), id)
			j, err := scanJob(row)
			if err != nil {
				return err
			}
			out = append(out, j)
		}
		return nil
	})
	if err == nil {
		q.servedMu.Lock()
		for _, j := range out {
			q.served[j.Source] = now
		}
		q.servedMu.Unlock()
	}
	return out, err
}

// queueHead is the first job of a (source, domain, storage, engine) group
// of queued jobs.
type queueHead struct {
	source, domain, storage, engine string
	priority                        int
	created                         time.Time
	id                              int64
}

// runningJobs counts running jobs per source and per domain.
func runningJobs(ctx context.Context, tx *sql.Tx, now time.Time) (perSource, perDomain map[string]int, err error) {
	rows, err := tx.QueryContext(ctx, `SELECT source, domain, count(*) FROM jobs
		WHERE state = 'leased' AND lease_until >= $1 GROUP BY source, domain`, now)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	perSource, perDomain = map[string]int{}, map[string]int{}
	for rows.Next() {
		var source, domain string
		var n int
		if err := rows.Scan(&source, &domain, &n); err != nil {
			return nil, nil, err
		}
		perSource[source] += n
		perDomain[domain] += n
	}
	return perSource, perDomain, rows.Err()
}

// queueHeads returns the first job of every group of queued jobs, found by
// skipping through the jobs_fair index: one row per group, not per job.
func queueHeads(ctx context.Context, tx *sql.Tx) ([]queueHead, error) {
	rows, err := tx.QueryContext(ctx, `WITH RECURSIVE h AS (
		  (SELECT source, domain, storage, engine, priority, created, id FROM jobs WHERE state = 'queued'
		   ORDER BY source, domain, storage, engine, priority, created, id LIMIT 1)
		  UNION ALL
		  SELECT n.* FROM h, LATERAL (SELECT source, domain, storage, engine, priority, created, id FROM jobs
		    WHERE state = 'queued' AND (source, domain, storage, engine) > (h.source, h.domain, h.storage, h.engine)
		    ORDER BY source, domain, storage, engine, priority, created, id LIMIT 1) n
		) SELECT source, domain, storage, engine, priority, created, id FROM h`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []queueHead
	for rows.Next() {
		var h queueHead
		if err := rows.Scan(&h.source, &h.domain, &h.storage, &h.engine, &h.priority, &h.created, &h.id); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// pick returns the ID of the next job to lease, 0 when there is none.
func (q *Queue) pick(ctx context.Context, tx *sql.Tx, r LeaseRequest, now time.Time) (int64, error) {
	running, perDomain, err := runningJobs(ctx, tx, now)
	if err != nil {
		return 0, err
	}
	full := []string{} // not nil: "<> ALL(NULL)" matches nothing
	for d, n := range perDomain {
		if n >= r.MaxPerDomain {
			full = append(full, d)
		}
	}

	// Jobs of crashed workers first, in queue order.
	storageCond, args := "", []any{now, now}
	for _, e := range r.Engines {
		args = append(args, e)
	}
	if len(r.Storages) > 0 {
		storageCond = "AND storage IN (" + strings.TrimSuffix(strings.Repeat("?,", len(r.Storages)), ",") + ")"
		for _, s := range r.Storages {
			args = append(args, s)
		}
	}
	args = append(args, full)
	var id int64
	err = tx.QueryRowContext(ctx, pgdb.Rebind(`SELECT id FROM jobs
		WHERE state = 'leased' AND lease_until < ? AND (not_before IS NULL OR not_before <= ?)
		  AND engine IN (`+strings.TrimSuffix(strings.Repeat("?,", len(r.Engines)), ",")+`) `+storageCond+`
		  AND domain <> ALL(?)
		ORDER BY priority, created, id LIMIT 1 FOR UPDATE SKIP LOCKED`), args...).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}

	all, err := queueHeads(ctx, tx)
	if err != nil {
		return 0, err
	}
	var heads []queueHead
	for _, h := range all {
		if perDomain[h.domain] < r.MaxPerDomain && slices.Contains(r.Engines, h.engine) &&
			(len(r.Storages) == 0 || slices.Contains(r.Storages, h.storage)) {
			heads = append(heads, h)
		}
	}
	// Queue order inside a source; the first head of a source sets its
	// priority.
	sort.Slice(heads, func(i, j int) bool {
		a, b := heads[i], heads[j]
		if a.priority != b.priority {
			return a.priority < b.priority
		}
		if !a.created.Equal(b.created) {
			return a.created.Before(b.created)
		}
		return a.id < b.id
	})
	first := map[string]int{}
	var sources []string
	for i, h := range heads {
		if _, ok := first[h.source]; !ok {
			first[h.source] = i
			sources = append(sources, h.source)
		}
	}
	q.servedMu.Lock()
	served := maps.Clone(q.served)
	q.servedMu.Unlock()
	sort.SliceStable(sources, func(i, j int) bool {
		a, b := sources[i], sources[j]
		if pa, pb := heads[first[a]].priority, heads[first[b]].priority; pa != pb {
			return pa < pb
		}
		if running[a] != running[b] {
			return running[a] < running[b]
		}
		return served[a].Before(served[b])
	})
	for _, source := range sources {
		for _, h := range heads[first[source]:] {
			if h.source != source {
				continue
			}
			err := tx.QueryRowContext(ctx, `SELECT id FROM jobs
				WHERE state = 'queued' AND source = $1 AND domain = $2 AND storage = $3 AND engine = $4
				  AND (not_before IS NULL OR not_before <= $5)
				ORDER BY priority, created, id LIMIT 1 FOR UPDATE SKIP LOCKED`, h.source, h.domain, h.storage, h.engine, now).Scan(&id)
			if err == nil {
				return id, nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return 0, err
			}
		}
	}
	return 0, nil
}

// Outcome reports the end of a capture attempt.
type Outcome struct {
	Error   string
	Retry   bool
	Capture string
}

// ErrNotFound is returned for unknown jobs.
var ErrNotFound = errors.New("queue: job not found")

// Finish records the outcome of a job. The first successful completion wins;
// later completions of a finished job are ignored and reported as false.
func (q *Queue) Finish(ctx context.Context, id int64, o Outcome) (bool, error) {
	now := q.Now()
	changed := false
	err := q.tx(ctx, func(tx *sql.Tx) error {
		var state string
		var attempts int
		if err := tx.QueryRowContext(ctx, `SELECT state, attempts FROM jobs WHERE id = $1`, id).Scan(&state, &attempts); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if state == Done {
			return nil
		}
		changed = true
		var err error
		switch {
		case o.Error == "":
			_, err = tx.ExecContext(ctx, `UPDATE jobs SET state = 'done', finished = $1, capture = $2, last_error = NULL, lease_until = NULL WHERE id = $3`,
				now, o.Capture, id)
		case o.Retry && attempts < q.MaxAttempts:
			_, err = tx.ExecContext(ctx, `UPDATE jobs SET state = 'queued', last_error = $1, lease_until = NULL, not_before = $2, capture = coalesce($3, capture) WHERE id = $4`,
				o.Error, now.Add(q.Backoff*time.Duration(attempts)), nullable(o.Capture), id)
		default:
			_, err = tx.ExecContext(ctx, `UPDATE jobs SET state = 'failed', finished = $1, last_error = $2, lease_until = NULL, capture = coalesce($3, capture) WHERE id = $4`,
				now, o.Error, nullable(o.Capture), id)
		}
		return err
	})
	return changed, err
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Release returns a leased job to the queue without counting the attempt.
func (q *Queue) Release(ctx context.Context, id int64, reason string) error {
	return q.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE jobs SET state = 'queued', lease_until = NULL, epoch = NULL,
			attempts = GREATEST(attempts - 1, 0), last_error = $1 WHERE id = $2 AND state = 'leased'`, reason, id)
		return err
	})
}

// Retry requeues a failed job with a fresh attempt counter.
func (q *Queue) Retry(ctx context.Context, id int64) error {
	return q.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE jobs SET state = 'queued', attempts = 0, not_before = NULL, lease_until = NULL, finished = NULL
			WHERE id = $1 AND state = 'failed'`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// RetryAllFailed requeues every failed job.
func (q *Queue) RetryAllFailed(ctx context.Context) (int64, error) {
	var n int64
	err := q.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE jobs SET state = 'queued', attempts = 0, not_before = NULL, lease_until = NULL, finished = NULL WHERE state = 'failed'`)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	return n, err
}

const jobCols = `id, url, domain, source, storage, engine, priority, fallback, state, coalesce(worker,''), coalesce(epoch,''),
	lease_until, attempts, coalesce(last_error,''), coalesce(capture,''), created, finished`

func scanJob(row interface{ Scan(...any) error }) (Job, error) {
	var j Job
	var ep string
	var lease, finished sql.NullTime
	err := row.Scan(&j.ID, &j.URL, &j.Domain, &j.Source, &j.Storage, &j.Engine, &j.Priority, &j.Fallback, &j.State, &j.Worker, &ep,
		&lease, &j.Attempts, &j.LastError, &j.Capture, &j.Created, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return j, ErrNotFound
	}
	j.Epoch = epoch.ID(ep)
	j.Created = j.Created.UTC()
	if lease.Valid {
		j.LeaseUntil = lease.Time.UTC()
	}
	if finished.Valid {
		j.Finished = finished.Time.UTC()
	}
	return j, err
}

// Get returns a job by ID.
func (q *Queue) Get(ctx context.Context, id int64) (Job, error) {
	return scanJob(q.db.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE id = $1`, id))
}

// List returns jobs in a state (all states when empty), newest first for
// finished jobs and in pick order for pending ones.
func (q *Queue) List(ctx context.Context, state string, limit, offset int) ([]Job, error) {
	if limit <= 0 {
		limit = 100
	}
	order := "coalesce(finished, created) DESC, id DESC"
	if state == Queued || state == Leased {
		order = "priority, created, id"
	}
	where, args := "", []any{}
	if state != "" {
		where, args = "WHERE state = ?", append(args, state)
	}
	rows, err := q.db.QueryContext(ctx, pgdb.Rebind(`SELECT `+jobCols+` FROM jobs `+where+` ORDER BY `+order+` LIMIT ? OFFSET ?`),
		append(args, limit, offset)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// Counts returns the number of jobs per state.
func (q *Queue) Counts(ctx context.Context) (map[string]int, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT state, count(*) FROM jobs GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{Queued: 0, Leased: 0, Done: 0, Failed: 0}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		out[s] = n
	}
	return out, rows.Err()
}

// ActiveLeases counts unexpired leases in an epoch of a storage.
func (q *Queue) ActiveLeases(ctx context.Context, storage string, ep epoch.ID) (int, error) {
	var n int
	err := q.db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE state = 'leased' AND storage = $1 AND epoch = $2 AND lease_until >= $3`,
		storage, string(ep), q.Now()).Scan(&n)
	return n, err
}

// PendingOf returns the URLs among urls that are queued or leased for a
// storage.
func (q *Queue) PendingOf(ctx context.Context, storage string, urls []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(urls) == 0 {
		return out, nil
	}
	if storage == "" {
		storage = "default"
	}
	rows, err := q.db.QueryContext(ctx, `SELECT DISTINCT url FROM jobs WHERE url = ANY($1) AND storage = $2 AND state IN ('queued','leased')`, urls, storage)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out[u] = true
	}
	return out, rows.Err()
}

// Pending reports whether a URL is queued or leased.
func (q *Queue) Pending(ctx context.Context, rawURL string) (bool, error) {
	var x int
	err := q.db.QueryRowContext(ctx, `SELECT 1 FROM jobs WHERE url = $1 AND state IN ('queued','leased') LIMIT 1`, rawURL).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Purge deletes finished jobs older than age.
func (q *Queue) Purge(ctx context.Context, age time.Duration) (int64, error) {
	var n int64
	err := q.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM jobs WHERE state = 'done' AND finished < $1`, q.Now().Add(-age))
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	return n, err
}

func (j Job) String() string {
	return fmt.Sprintf("job %d %s (%s/%s)", j.ID, j.URL, j.Source, j.Engine)
}
