package sealrun

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ogefest/hoarfrost/internal/appdb"
	"github.com/ogefest/hoarfrost/internal/pgdb/pgtest"
)

func open(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	appURL, appSchema := pgtest.Schema(t, "app")
	db, err := appdb.Open(context.Background(), appURL, appSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Date(2026, 10, 26, 1, 0, 0, 0, time.UTC)
	s := New(db)
	s.Now = func() time.Time { return now }
	return s, &now
}

func TestRunsFailuresAndInterruption(t *testing.T) {
	ctx := context.Background()
	s, now := open(t)

	ok, _ := s.Start(ctx, "default", "2026-W42", "a", time.Time{})
	s.Finish(ctx, ok, nil)
	failed, _ := s.Start(ctx, "default", "2026-W43", "a", time.Time{})
	s.Finish(ctx, failed, errors.New("disk full"))
	*now = now.Add(time.Hour)
	killed, _ := s.Start(ctx, "default", "2026-W43", "a", time.Time{})
	s.Progress(ctx, killed, "packaging", 40, 100)
	*now = now.Add(time.Minute)
	alive, _ := s.Start(ctx, "prasa", "2026-W43", "b", time.Time{})
	if _, err := s.Start(ctx, "prasa", "2026-W43", "c", now.Add(-time.Minute)); !errors.Is(err, ErrRunning) {
		t.Fatalf("second start of a running week: %v", err)
	}

	if a, _ := s.Alive(ctx, "default", "2026-W43", now.Add(-5*time.Minute)); !a {
		t.Fatal("running seal not alive")
	}
	*now = now.Add(10 * time.Minute)
	s.Beat(ctx, alive)
	ended, err := s.Interrupt(ctx, now.Add(-3*time.Minute), "no heartbeat")
	if err != nil || len(ended) != 1 || ended[0].ID != killed || ended[0].Error != "no heartbeat during packaging" || ended[0].Done != 40 {
		t.Fatalf("interrupted %+v %v", ended, err)
	}
	if a, _ := s.Alive(ctx, "default", "2026-W43", now.Add(-3*time.Minute)); a {
		t.Fatal("interrupted seal still alive")
	}

	f, err := s.Failures(ctx)
	if err != nil || len(f) != 1 {
		t.Fatalf("failures %+v %v", f, err)
	}
	w43 := f[Key{"default", "2026-W43"}]
	if w43.Attempts != 2 || w43.Last.Outcome != OutcomeInterrupted || w43.Last.Phase != "packaging" {
		t.Fatalf("W43 %+v", w43)
	}

	again, _ := s.Start(ctx, "default", "2026-W43", "a", now.Add(-3*time.Minute))
	s.Finish(ctx, again, nil)
	s.Finish(ctx, again, errors.New("finishing twice changes nothing"))
	// A run taken for interrupted that finishes after all records it.
	if _, err := s.Interrupt(ctx, now.Add(time.Hour), "no heartbeat"); err != nil {
		t.Fatal(err)
	}
	s.Finish(ctx, alive, nil)
	runs, _ := s.Recent(ctx, 10)
	for _, r := range runs {
		if r.ID == alive && (r.Outcome != OutcomeOK || r.Error != "") {
			t.Fatalf("late finish %+v", r)
		}
	}
	if f, _ := s.Failures(ctx); len(f) != 0 {
		t.Fatalf("a successful seal clears failures: %+v", f)
	}
	s.Record(ctx, "default", "2026-W40", "a", OutcomeInterrupted, "found sealing")
	runs, err = s.Recent(ctx, 10)
	if err != nil || len(runs) != 6 || runs[0].Epoch != "2026-W40" || runs[0].Running() {
		t.Fatalf("recent %+v %v", runs, err)
	}
	for _, r := range runs {
		if r.Running() {
			t.Fatalf("run %+v", r)
		}
	}
}
