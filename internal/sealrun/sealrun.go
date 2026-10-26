// Package sealrun records attempts to seal weeks in the application
// database, so that failed seals and seals interrupted by a stopped process
// (out of memory, crash, restart) stay visible after restarts and can be
// retried.
package sealrun

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/ogefest/hoarfrost/internal/epoch"
)

// Outcomes of finished runs.
const (
	OutcomeOK          = "ok"
	OutcomeFailed      = "failed"
	OutcomeInterrupted = "interrupted"
)

// Run is one attempt to seal a week of a storage.
type Run struct {
	ID        int64
	Storage   string
	Epoch     epoch.ID
	Host      string
	Started   time.Time
	Heartbeat time.Time
	Finished  time.Time // zero while running
	Phase     string
	Done      int64
	Total     int64
	Outcome   string // empty while running
	Error     string
}

// Running reports whether the run has not finished.
func (r Run) Running() bool { return r.Outcome == "" }

// Failure describes a week whose latest seal attempt did not succeed.
type Failure struct {
	// Attempts counts the failed attempts since the last successful one.
	Attempts int
	Last     Run
}

// Key identifies a week of a storage.
type Key struct {
	Storage string
	Epoch   epoch.ID
}

// Store keeps the runs in the seal_runs table (see appdb).
type Store struct {
	db  *sql.DB
	Now func() time.Time
}

// New returns a store on the application database.
func New(db *sql.DB) *Store { return &Store{db: db, Now: time.Now} }

// ErrRunning is returned by Start when the week is being sealed.
var ErrRunning = errors.New("sealrun: the week is being sealed")

// Start records a new run unless a run of the week is going on with a
// heartbeat since liveAfter, in this process or another one; then it
// returns ErrRunning. Two processes cannot start the same week at once.
func (s *Store) Start(ctx context.Context, storage string, ep epoch.ID, host string, liveAfter time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('seal_runs/' || $1 || '/' || $2))`, storage, string(ep)); err != nil {
		return 0, err
	}
	var x int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM seal_runs WHERE storage = $1 AND epoch = $2 AND outcome IS NULL AND heartbeat >= $3 LIMIT 1`,
		storage, string(ep), liveAfter.UTC()).Scan(&x)
	if err == nil {
		return 0, ErrRunning
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	now := s.Now().UTC()
	var id int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO seal_runs(storage, epoch, host, started, heartbeat) VALUES ($1,$2,$3,$4,$4) RETURNING id`,
		storage, string(ep), host, now).Scan(&id); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// Progress records the phase of a running run; it also counts as a
// heartbeat.
func (s *Store) Progress(ctx context.Context, id int64, phase string, done, total int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE seal_runs SET phase = $2, done = $3, total = $4, heartbeat = $5 WHERE id = $1 AND outcome IS NULL`,
		id, phase, done, total, s.Now().UTC())
	return err
}

// Beat shows that the process running the seal is alive.
func (s *Store) Beat(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE seal_runs SET heartbeat = $2 WHERE id = $1 AND outcome IS NULL`, id, s.Now().UTC())
	return err
}

// Finish records the outcome of a run: ok when err is nil. It also
// replaces "interrupted" set while the process could not show it was
// alive (e.g. the database was unreachable).
func (s *Store) Finish(ctx context.Context, id int64, err error) error {
	outcome, msg := OutcomeOK, sql.NullString{}
	if err != nil {
		outcome, msg = OutcomeFailed, sql.NullString{String: err.Error(), Valid: true}
	}
	now := s.Now().UTC()
	_, dberr := s.db.ExecContext(ctx, `UPDATE seal_runs SET outcome = $2, error = $3, finished = $4, heartbeat = $4
		WHERE id = $1 AND (outcome IS NULL OR outcome = '`+OutcomeInterrupted+`')`, id, outcome, msg, now)
	return dberr
}

// Interrupt ends running runs whose last heartbeat is older than before:
// the process running them stopped. It returns the runs it ended.
func (s *Store) Interrupt(ctx context.Context, before time.Time, reason string) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `UPDATE seal_runs SET outcome = $2, finished = heartbeat,
		error = $3 || CASE WHEN phase <> '' THEN ' during ' || phase ELSE '' END
		WHERE outcome IS NULL AND heartbeat < $1 RETURNING `+cols, before.UTC(), OutcomeInterrupted, reason)
	if err != nil {
		return nil, err
	}
	return scanRuns(rows)
}

// Record adds a finished run, e.g. for a seal interrupted before runs were
// recorded.
func (s *Store) Record(ctx context.Context, storage string, ep epoch.ID, host, outcome, msg string) error {
	now := s.Now().UTC()
	_, err := s.db.ExecContext(ctx, `INSERT INTO seal_runs(storage, epoch, host, started, heartbeat, finished, outcome, error)
		VALUES ($1,$2,$3,$4,$4,$4,$5,$6)`, storage, string(ep), host, now, outcome, msg)
	return err
}

// Alive reports whether a run of the week is going on with a heartbeat
// since after.
func (s *Store) Alive(ctx context.Context, storage string, ep epoch.ID, after time.Time) (bool, error) {
	var x int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM seal_runs WHERE storage = $1 AND epoch = $2 AND outcome IS NULL AND heartbeat >= $3 LIMIT 1`,
		storage, string(ep), after.UTC()).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Recent lists the latest runs, newest first.
func (s *Store) Recent(ctx context.Context, limit int) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+cols+` FROM seal_runs ORDER BY started DESC, id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return scanRuns(rows)
}

// Failures returns the weeks whose latest finished run did not succeed,
// with the number of failed attempts since their last successful one.
func (s *Store) Failures(ctx context.Context) (map[Key]Failure, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+cols+` FROM seal_runs WHERE outcome IS NOT NULL ORDER BY storage, epoch, started, id`)
	if err != nil {
		return nil, err
	}
	runs, err := scanRuns(rows)
	if err != nil {
		return nil, err
	}
	out := map[Key]Failure{}
	for _, r := range runs {
		k := Key{r.Storage, r.Epoch}
		if r.Outcome == OutcomeOK {
			delete(out, k)
			continue
		}
		f := out[k]
		f.Attempts++
		f.Last = r
		out[k] = f
	}
	return out, nil
}

const cols = `id, storage, epoch, host, started, heartbeat, finished, phase, done, total, coalesce(outcome, ''), coalesce(error, '')`

func scanRuns(rows *sql.Rows) ([]Run, error) {
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var ep string
		var finished sql.NullTime
		if err := rows.Scan(&r.ID, &r.Storage, &ep, &r.Host, &r.Started, &r.Heartbeat, &finished, &r.Phase, &r.Done, &r.Total,
			&r.Outcome, &r.Error); err != nil {
			return nil, err
		}
		r.Epoch = epoch.ID(ep)
		r.Started, r.Heartbeat = r.Started.UTC(), r.Heartbeat.UTC()
		if finished.Valid {
			r.Finished = finished.Time.UTC()
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
