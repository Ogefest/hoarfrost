package sourcestore

import (
	"context"
	"testing"
	"time"

	"github.com/ogefest/hoarfrost/internal/pgdb/pgtest"

	"github.com/ogefest/hoarfrost/internal/appdb"
	"github.com/ogefest/hoarfrost/internal/config"
)

func open(t *testing.T) *Store {
	t.Helper()
	appURL, appSchema := pgtest.Schema(t, "app")
	db, err := appdb.Open(context.Background(), appURL, appSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return New(db)
}

func TestEmptyStore(t *testing.T) {
	cfg, err := open(t).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	urls, _, _ := cfg.BlocklistSettings()
	if len(cfg.Sources) != 0 || cfg.Blocklists != nil || len(urls) != 1 {
		t.Fatalf("empty store %+v", cfg)
	}
}

func TestSaveLoadAndPollState(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	min := 300
	cfg := &config.Sources{
		Defaults:   config.Defaults{Settings: config.Settings{Engine: "static", Poll: config.Duration(2 * time.Hour)}, MaxPerDomain: 3},
		Blocklists: &config.Blocklists{URLs: []string{}},
		Sources: []config.Source{
			{Name: "b", Feeds: []string{"https://b.pl/rss"}, Settings: config.Settings{MinTextChars: &min, MaxSize: 5 << 20}},
			{Name: "a", Sitemaps: []string{"https://a.pl/sitemap.xml"}, Settings: config.Settings{Poll: config.Duration(30 * time.Minute)}},
		},
	}
	if err := s.Save(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Sources) != 2 || got.Sources[0].Name != "a" || got.MaxPerDomain() != 3 ||
		got.Resolve("a").Poll != 30*time.Minute || got.Resolve("b").Poll != 2*time.Hour ||
		got.Resolve("b").Options.MinTextChars != 300 || got.Resolve("b").Options.MaxResponseSize != 5<<20 {
		t.Fatalf("loaded %+v", got)
	}
	if urls, _, _ := got.BlocklistSettings(); len(urls) != 0 {
		t.Fatalf("disabled block lists came back as %v", urls)
	}

	at := time.UnixMilli(time.Now().UnixMilli())
	if err := s.RecordPoll(ctx, "a", Status{LastPoll: at, Found: 7, New: 2, Error: "boom"}); err != nil {
		t.Fatal(err)
	}
	// Editing a source keeps its poll state; removing one drops it.
	got.Sources[0].URLs = []string{"https://a.pl/x"}
	got.Sources = got.Sources[:1]
	got.Blocklists = nil
	if err := s.Save(ctx, got); err != nil {
		t.Fatal(err)
	}
	st, err := s.Statuses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if a := st["a"]; len(st) != 1 || !a.LastPoll.Equal(at) || a.Found != 7 || a.New != 2 || a.Error != "boom" {
		t.Fatalf("statuses %+v", st)
	}
	again, _ := s.Load(ctx)
	if len(again.Sources) != 1 || len(again.Sources[0].URLs) != 1 || again.Blocklists != nil {
		t.Fatalf("after edit %+v", again)
	}
	if err := s.RecordPoll(ctx, "b", Status{LastPoll: at}); err != nil {
		t.Fatal("poll of a deleted source must be ignored:", err)
	}

	// The last successful poll survives later failures.
	if !st["a"].LastSuccess.IsZero() {
		t.Fatalf("a failed poll counted as success: %+v", st["a"])
	}
	ok, failed := at.Add(time.Minute), at.Add(2*time.Minute)
	if err := s.RecordPoll(ctx, "a", Status{LastPoll: ok, Found: 3}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordPoll(ctx, "a", Status{LastPoll: failed, Error: "sitemap: HTTP 503"}); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Statuses(ctx)
	if a := st["a"]; !a.LastPoll.Equal(failed) || !a.LastSuccess.Equal(ok) || a.Error == "" {
		t.Fatalf("after failure %+v", a)
	}
}
