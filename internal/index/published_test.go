package index

import (
	"context"
	"testing"
	"time"

	"github.com/ogefest/hoarfrost/internal/recorder"
)

func TestPublished(t *testing.T) {
	cases := []struct {
		name string
		p    recorder.PageInfo
		want string
	}{
		{"space-separated schema.org date", recorder.PageInfo{
			Published: "2026-10-28",
			SchemaOrg: []map[string]any{{"@type": "NewsArticle", "datePublished": "2026-10-16 06:45:00+0200"}},
		}, "2026-10-16T04:45:00Z"},
		{"nested article, not the logo", recorder.PageInfo{
			Published: "1997-01-14",
			SchemaOrg: []map[string]any{{"@type": "WebPage",
				"publisher":  map[string]any{"@type": "Organization", "logo": map[string]any{"@type": "ImageObject", "datePublished": "1997-01-14"}},
				"mainEntity": map[string]any{"@type": "Article", "datePublished": "2026-02-24T14:30:00Z"}}},
		}, "2026-02-24T14:30:00Z"},
		{"extractor date", recorder.PageInfo{Published: "2026-05-06"}, "2026-05-06T00:00:00Z"},
	}
	for _, c := range cases {
		got, ok := published(c.p).(time.Time)
		if !ok || got.Format(time.RFC3339) != c.want {
			t.Errorf("%s: got %v, want %s", c.name, published(c.p), c.want)
		}
	}
}

func TestRepairPublished(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	article := []map[string]any{{"@type": "WebPage",
		"publisher":  map[string]any{"@type": "Organization", "logo": map[string]any{"@type": "ImageObject", "datePublished": "1997-01-14"}},
		"mainEntity": map[string]any{"@type": "Article", "datePublished": "2026-02-24T14:30:00Z"}}}
	a := capture("a.warc.gz", t0, recorder.PageInfo{URL: "https://pb.pl/artykul", Published: "1997-01-14", SchemaOrg: article, Source: "pb", Kind: "page"})
	b := capture("b.warc.gz", t0, recorder.PageInfo{URL: "https://inny.pl/strona", Published: "2026-05-06", Source: "inny", Kind: "page"})
	for _, cf := range []recorder.CaptureFile{a, b} {
		if err := db.AddCapture(ctx, cf, cf.Key()); err != nil {
			t.Fatal(err)
		}
	}
	// Pages indexed by a version that took the date of the logo.
	if _, err := db.db.ExecContext(ctx, `UPDATE pages SET published = '1997-01-14' WHERE uri = 'https://pb.pl/artykul'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE urls SET published = '1997-01-14' WHERE uri = 'https://pb.pl/artykul'`); err != nil {
		t.Fatal(err)
	}
	pubOf := func(table string) string {
		var p time.Time
		if err := db.db.QueryRowContext(ctx, `SELECT published FROM `+table+` WHERE uri = 'https://pb.pl/artykul'`).Scan(&p); err != nil {
			t.Fatal(err)
		}
		return p.UTC().Format(time.RFC3339)
	}
	fixes, err := db.RepairPublished(ctx, "", "", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(fixes) != 1 || fixes[0].URI != "https://pb.pl/artykul" || fixes[0].New.Format(time.RFC3339) != "2026-02-24T14:30:00Z" {
		t.Fatalf("dry run fixes %+v", fixes)
	}
	if pubOf("pages") != "1997-01-14T00:00:00Z" {
		t.Fatal("dry run changed the page")
	}
	if _, err := db.RepairPublished(ctx, "", "2026-W41", false, nil); err != nil {
		t.Fatal(err)
	}
	if pubOf("pages") != "2026-02-24T14:30:00Z" || pubOf("urls") != "2026-02-24T14:30:00Z" {
		t.Fatalf("page %s, url %s", pubOf("pages"), pubOf("urls"))
	}
	if fixes, _ := db.RepairPublished(ctx, "", "", true, nil); len(fixes) != 0 {
		t.Fatalf("second run fixes %+v", fixes)
	}
}

func TestPublishedDate(t *testing.T) {
	warsaw, err := time.LoadLocation("Europe/Warsaw")
	if err != nil {
		t.Skip(err)
	}
	old := time.Local
	time.Local = warsaw
	defer func() { time.Local = old }()
	midnight, _ := time.Parse("2006-01-02 15:04:05Z0700", "2026-11-04 00:00:00+0100")
	for in, want := range map[time.Time]string{
		midnight: "2026-11-04",
		time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC):   "2026-11-04",
		time.Date(2026, 11, 4, 23, 30, 0, 0, time.UTC): "2026-11-05",
	} {
		if got := PublishedDate(in); got != want {
			t.Errorf("%s: got %s, want %s", in, got, want)
		}
	}
}
