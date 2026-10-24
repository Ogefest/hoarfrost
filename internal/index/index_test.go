package index

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ogefest/hoarfrost/internal/cdxj"
	"github.com/ogefest/hoarfrost/internal/pgdb/pgtest"
	"github.com/ogefest/hoarfrost/internal/recorder"
	"github.com/ogefest/hoarfrost/internal/warcio"
)

var t0 = time.Date(2026, 10, 6, 8, 0, 0, 0, time.UTC)

func openDB(t *testing.T) *DB {
	t.Helper()
	url, schema := pgtest.Schema(t, "index")
	db, err := Open(context.Background(), url, schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func capture(name string, at time.Time, page recorder.PageInfo, recs ...recorder.RecordInfo) recorder.CaptureFile {
	for i := range recs {
		if recs[i].Date.IsZero() {
			recs[i].Date = at
		}
	}
	page.Captured = at
	return recorder.CaptureFile{
		Name: name, Epoch: "2026-W41", Size: 1000, Records: recs,
		Pages: []recorder.CapturedPage{{MetadataID: "<meta-" + name + ">", PageInfo: page}},
	}
}

func fixture(t *testing.T, db *DB) {
	ctx := context.Background()
	a := capture("a.warc.gz", t0, recorder.PageInfo{URL: "https://serwis.pl/budzet", RecordID: "<r1>", Title: "Radni przyjęli budżet",
		Text: "Głosowanie w radzie miasta nad budżetem", Source: "serwis", Engine: "static", Kind: "page"},
		recorder.RecordInfo{ID: "<r0>", Type: warcio.TypeRequest, URI: "https://serwis.pl/budzet"},
		recorder.RecordInfo{ID: "<r1>", Type: warcio.TypeResponse, URI: "https://serwis.pl/budzet", Mime: "text/html", Status: 200, Digest: "sha1:PAGE1", Offset: 100, Length: 50},
		recorder.RecordInfo{ID: "<r2>", Type: warcio.TypeResponse, URI: "https://serwis.pl/style.css", Mime: "text/css", Status: 200, Digest: "sha1:CSS", Offset: 150, Length: 40},
		recorder.RecordInfo{ID: "<m1>", Type: warcio.TypeMetadata, URI: "https://serwis.pl/budzet"},
	)
	b := capture("b.warc.gz", t0.Add(24*time.Hour), recorder.PageInfo{URL: "https://www.serwis.pl/tramwaj", RecordID: "<r3>", Title: "Nowa linia tramwajowa",
		Text: "Przetarg na tramwaj", Source: "serwis", Engine: "chrome", Fallback: true, Kind: "page"},
		recorder.RecordInfo{ID: "<r3>", Type: warcio.TypeResponse, URI: "https://www.serwis.pl/tramwaj", Mime: "text/html", Status: 200, Digest: "sha1:PAGE2", Offset: 100, Length: 60},
		recorder.RecordInfo{ID: "<r4>", Type: warcio.TypeRevisit, URI: "https://serwis.pl/style.css", Mime: "text/css", Status: 200, Digest: "sha1:CSS", RefersTo: "<r2>", Offset: 160, Length: 30},
	)
	c := capture("c.warc.gz", t0.Add(48*time.Hour), recorder.PageInfo{URL: "https://inny.pl/raport.pdf", Title: "Raport", Text: "Łódź raport roczny", Source: "inny", Engine: "http", Kind: "attachment"},
		recorder.RecordInfo{ID: "<r5>", Type: warcio.TypeResponse, URI: "https://inny.pl/raport.pdf", Mime: "application/pdf", Status: 200, Digest: "sha1:PDF", Offset: 100, Length: 70},
	)
	for _, cf := range []recorder.CaptureFile{a, b, c} {
		if err := db.AddCapture(ctx, cf, cf.Key()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLookupDedup(t *testing.T) {
	db := openDB(t)
	fixture(t, db)
	ctx := context.Background()
	ref, err := db.Lookup(ctx, "", "2026-W41", "sha1:CSS")
	if err != nil || ref == nil || ref.RecordID != "<r2>" || ref.URI != "https://serwis.pl/style.css" || !ref.Date.Equal(t0) {
		t.Fatalf("lookup %+v %v", ref, err)
	}
	if ref, _ := db.Lookup(ctx, "", "2026-W42", "sha1:CSS"); ref != nil {
		t.Fatal("dedup must be scoped to the epoch")
	}
	if ref, _ := db.Lookup(ctx, "", "2026-W41", "sha1:NOPE"); ref != nil {
		t.Fatal("unexpected hit")
	}
	if _, err := db.Record(ctx, "<r0>"); err != ErrNotFound {
		t.Fatal("request records are not indexed")
	}
}

func TestClosestAndPayload(t *testing.T) {
	db := openDB(t)
	fixture(t, db)
	ctx := context.Background()
	r, err := db.Closest(ctx, "http://www.serwis.pl/style.css", t0.Add(30*time.Hour))
	if err != nil || r.ID != "<r4>" {
		t.Fatalf("closest %+v %v", r, err)
	}
	p, err := db.Payload(ctx, r)
	if err != nil || p.ID != "<r2>" {
		t.Fatalf("payload %+v %v", p, err)
	}
	r, _ = db.Closest(ctx, "https://serwis.pl/style.css", t0)
	if r.ID != "<r2>" {
		t.Fatal(r.ID)
	}
	// An exact URL wins over an equivalent one closer in time: the redirect
	// serwis.pl/tramwaj -> www.serwis.pl/tramwaj must not resolve to itself.
	redirect := capture("d.warc.gz", t0.Add(25*time.Hour), recorder.PageInfo{URL: "https://serwis.pl/tramwaj"},
		recorder.RecordInfo{ID: "<r6>", Type: warcio.TypeResponse, URI: "https://serwis.pl/tramwaj", Mime: "text/html", Status: 301, Offset: 100, Length: 20})
	if err := db.AddCapture(ctx, redirect, redirect.Key()); err != nil {
		t.Fatal(err)
	}
	if r, _ = db.Closest(ctx, "https://www.serwis.pl/tramwaj", t0.Add(25*time.Hour)); r.ID != "<r3>" {
		t.Fatal(r.ID)
	}
	if r, _ = db.Closest(ctx, "https://serwis.pl/tramwaj", t0.Add(24*time.Hour)); r.ID != "<r6>" {
		t.Fatal(r.ID)
	}
	tl, err := db.Timeline(ctx, "https://serwis.pl/style.css")
	if err != nil || len(tl) != 2 || tl[0].ID != "<r4>" {
		t.Fatalf("timeline %+v", tl)
	}
	if _, err := db.Closest(ctx, "https://nothing.pl/", time.Time{}); err != ErrNotFound {
		t.Fatal(err)
	}
}

func TestSearch(t *testing.T) {
	db := openDB(t)
	fixture(t, db)
	ctx := context.Background()
	cases := []struct {
		q    SearchQuery
		want []string
	}{
		{SearchQuery{Q: "budzet"}, []string{"https://serwis.pl/budzet"}},
		{SearchQuery{Q: "BUDŻET"}, []string{"https://serwis.pl/budzet"}},
		{SearchQuery{Q: "lodz"}, []string{"https://inny.pl/raport.pdf"}},
		{SearchQuery{Q: "tramw"}, []string{"https://www.serwis.pl/tramwaj"}},
		{SearchQuery{Q: `"; DROP TABLE pages; --`}, nil},
		{SearchQuery{Source: "serwis"}, []string{"https://www.serwis.pl/tramwaj", "https://serwis.pl/budzet"}},
		{SearchQuery{Domain: "serwis.pl"}, []string{"https://www.serwis.pl/tramwaj", "https://serwis.pl/budzet"}},
		{SearchQuery{Kind: "attachment"}, []string{"https://inny.pl/raport.pdf"}},
		{SearchQuery{From: t0.Add(time.Hour), To: t0.Add(47 * time.Hour)}, []string{"https://www.serwis.pl/tramwaj"}},
		{SearchQuery{Limit: 1, Offset: 1}, []string{"https://www.serwis.pl/tramwaj"}},
	}
	for _, c := range cases {
		pages, total, err := db.Search(ctx, c.q)
		if err != nil {
			t.Fatalf("%+v: %v", c.q, err)
		}
		var got []string
		for _, p := range pages {
			got = append(got, p.URI)
		}
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%+v: got %v want %v", c.q, got, c.want)
		}
		if c.q.Limit == 0 && total != len(c.want) {
			t.Errorf("%+v: total %d", c.q, total)
		}
	}
	pages, _, _ := db.Search(ctx, SearchQuery{Q: "budzet"})
	if pages[0].Title != "Radni przyjęli budżet" || pages[0].TextLen == 0 || pages[0].Epoch != "2026-W41" {
		t.Fatalf("page %+v", pages[0])
	}
	if err := db.UpdatePageText(ctx, pages[0].ID, pages[0].Title, "tekst z ocr"); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.CountMatches(ctx, "ocr"); n != 1 {
		t.Fatal("updated text not searchable")
	}
	if n, _ := db.CountMatches(ctx, "glosowanie"); n != 0 {
		t.Fatal("old text still searchable")
	}
}

func TestSeen(t *testing.T) {
	db := openDB(t)
	fixture(t, db)
	ctx := context.Background()
	if ok, _ := db.Seen(ctx, "", "https://serwis.pl/budzet"); !ok {
		t.Fatal("captured page must be seen")
	}
	fresh, err := db.MarkSeen(ctx, "", []string{"https://serwis.pl/budzet", "https://serwis.pl/nowy", "https://serwis.pl/nowy"}, t0)
	if err != nil || len(fresh) != 1 || fresh[0] != "https://serwis.pl/nowy" {
		t.Fatalf("fresh %v %v", fresh, err)
	}
	if fresh, _ := db.MarkSeen(ctx, "prasa", []string{"https://serwis.pl/nowy"}, t0); len(fresh) != 1 {
		t.Fatal("seen URLs must be tracked per storage")
	}
}

func TestIdempotentAndDeleteEpoch(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	fixture(t, db)
	before, _ := db.Dump(ctx)
	fixture(t, db)
	after, _ := db.Dump(ctx)
	if before != after {
		t.Fatal("re-adding captures changed the index")
	}
	if n, _ := db.CountMatches(ctx, "budzet"); n != 1 {
		t.Fatalf("duplicate FTS rows: %d", n)
	}
	if err := db.DeleteEpoch(ctx, "", "2026-W41"); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.CountMatches(ctx, "budzet"); n != 0 {
		t.Fatal("FTS rows left after delete")
	}
	fixture(t, db)
	again, _ := db.Dump(ctx)
	if again != before {
		t.Fatalf("rebuild differs:\n%s\n---\n%s", again, before)
	}
}

func TestRelocateAndEpochs(t *testing.T) {
	db := openDB(t)
	fixture(t, db)
	ctx := context.Background()
	if err := db.Relocate(ctx, map[string]Location{"<r1>": {"weekly/2026/W41.wacz", 5000, 50, "serwis.warc.gz"}},
		map[string]Location{"<meta-a.warc.gz>": {"weekly/2026/W41.wacz", 7000, 70, ""}}, map[string]string{"a.warc.gz": "serwis.warc.gz"}); err != nil {
		t.Fatal(err)
	}
	r, _ := db.Record(ctx, "<r1>")
	if r.Location != "weekly/2026/W41.wacz" || r.Offset != 5000 || r.File != "serwis.warc.gz" {
		t.Fatalf("%+v", r)
	}
	pages, _ := db.PageCaptures(ctx, "https://serwis.pl/budzet")
	if pages[0].File != "serwis.warc.gz" || pages[0].Location != "weekly/2026/W41.wacz" || pages[0].Offset != 7000 {
		t.Fatalf("page file %s", pages[0].File)
	}
	if err := db.Relocate(ctx, map[string]Location{"<nope>": {}}, nil, nil); err == nil {
		t.Fatal("unknown record must fail")
	}

	e, _ := db.Epoch(ctx, "", "2026-W41")
	if e.State != StateOpen {
		t.Fatal(e.State)
	}
	db.SetEpoch(ctx, EpochInfo{Epoch: "2026-W40", State: StateVerified, Size: 10, SealedAt: t0})
	db.SetEpoch(ctx, EpochInfo{Epoch: "2026-W40", State: StateVerified, Size: 20, SealedAt: t0, SHA256: "abc"})
	eps, err := db.Epochs(ctx)
	if err != nil || len(eps) != 2 || eps[0].Epoch != "2026-W41" || eps[1].Size != 20 || !eps[1].SealedAt.Equal(t0) {
		t.Fatalf("epochs %+v %v", eps, err)
	}
	files, _ := db.Files(ctx, "", "2026-W41")
	if len(files) != 3 {
		t.Fatalf("files %v", files)
	}
}

func TestStats(t *testing.T) {
	db := openDB(t)
	fixture(t, db)
	s, err := db.Stats(context.Background(), 100000)
	if err != nil {
		t.Fatal(err)
	}
	if s.Pages != 3 || s.Revisits != 1 || s.DedupSaved != 40 || s.Bytes != 3000 || len(s.PerDay) != 3 {
		t.Fatalf("stats %+v", s)
	}
	if len(s.Sources) != 2 || s.Sources[1].Source != "serwis" || s.Sources[1].FallbackRatio() != 0.5 {
		t.Fatalf("sources %+v", s.Sources)
	}
}

// assertEpochStats checks the maintained totals against a full recount.
func assertEpochStats(t *testing.T, db *DB, records, revisits, saved int64) {
	t.Helper()
	ctx := context.Background()
	var got, want string
	q := `SELECT coalesce(string_agg(storage || '/' || epoch || ':' || records || ',' || revisits || ',' || saved, ' ' ORDER BY storage, epoch), '') FROM (%s) s`
	if err := db.db.QueryRowContext(ctx, fmt.Sprintf(q, `SELECT * FROM epoch_stats`)).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRowContext(ctx, fmt.Sprintf(q, epochStatsSelect+` GROUP BY r.storage, r.epoch`)).Scan(&want); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("epoch_stats %q, recount %q", got, want)
	}
	s, err := db.Stats(ctx, 0)
	if err != nil || s.Records != records || s.Revisits != revisits || s.DedupSaved != saved {
		t.Fatalf("stats %d/%d/%d, want %d/%d/%d (%v)", s.Records, s.Revisits, s.DedupSaved, records, revisits, saved, err)
	}
}

func TestEpochStatsMaintained(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	fixture(t, db)
	assertEpochStats(t, db, 5, 1, 40)
	fixture(t, db)
	assertEpochStats(t, db, 5, 1, 40)

	// A rebuild from a package index may insert a revisit before its original.
	recs, _ := db.EpochRecords(ctx, "", "2026-W41")
	sort.Slice(recs, func(i, j int) bool { return recs[i].Type > recs[j].Type })
	if recs[0].Type != warcio.TypeRevisit {
		t.Fatal("fixture has no revisit")
	}
	if err := db.DeleteEpoch(ctx, "", "2026-W41"); err != nil {
		t.Fatal(err)
	}
	assertEpochStats(t, db, 0, 0, 0)
	if err := db.AddRecords(ctx, recs, nil); err != nil {
		t.Fatal(err)
	}
	assertEpochStats(t, db, 5, 1, 40)
}

func TestFTSQuery(t *testing.T) {
	if got := FTSQuery(`Ala "ma" Łódź*`); got != `'ala' & 'ma' & 'lodz':*` {
		t.Fatal(got)
	}
	if FTSQuery(" ;; ") != "" {
		t.Fatal("empty")
	}
}

func TestVolume(t *testing.T) {
	db := openDB(t)
	fixture(t, db)
	rows, err := db.Volume(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]VolumeRow{}
	for _, r := range rows {
		got[r.Category] = r
	}
	if got["html"].Bytes != 110 || got["css"].Records != 2 || got["css"].Revisits != 1 || got["document"].Bytes != 70 {
		t.Fatalf("%+v", rows)
	}
	if rows, _ := db.Volume(context.Background(), "2026-W40"); len(rows) != 0 {
		t.Fatal("epoch filter")
	}
}

func TestStoragesAreIsolated(t *testing.T) {
	db := openDB(t)
	fixture(t, db)
	ctx := context.Background()
	other := capture("o.warc.gz", t0, recorder.PageInfo{URL: "https://serwis.pl/inny", RecordID: "<o1>", Title: "Inny magazyn", Text: "osobny bucket", Kind: "page"},
		recorder.RecordInfo{ID: "<o1>", Type: warcio.TypeResponse, URI: "https://serwis.pl/inny", Mime: "text/html", Status: 200, Digest: "sha1:OTHER", Offset: 100, Length: 10},
		recorder.RecordInfo{ID: "<o2>", Type: warcio.TypeResponse, URI: "https://serwis.pl/style.css", Mime: "text/css", Status: 200, Digest: "sha1:CSS", Offset: 110, Length: 40},
	)
	other.Storage = "prasa"
	if err := db.AddCapture(ctx, other, other.Key()); err != nil {
		t.Fatal(err)
	}
	if ref, _ := db.Lookup(ctx, "prasa", "2026-W41", "sha1:CSS"); ref == nil || ref.RecordID != "<o2>" {
		t.Fatalf("dedup in prasa found %+v", ref)
	}
	if ref, _ := db.Dedup("").Lookup(ctx, "2026-W41", "sha1:CSS"); ref == nil || ref.RecordID != "<r2>" {
		t.Fatal("dedup must stay inside the default storage")
	}
	if ref, _ := db.Lookup(ctx, "prasa", "2026-W41", "sha1:PAGE1"); ref != nil {
		t.Fatal("dedup crossed storages")
	}
	pages, n, _ := db.Search(ctx, SearchQuery{Storage: "prasa"})
	if n != 1 || pages[0].Storage != "prasa" {
		t.Fatalf("search by storage %+v", pages)
	}
	if _, n, _ := db.Search(ctx, SearchQuery{}); n != 4 {
		t.Fatalf("search across storages: %d", n)
	}
	r, _ := db.Record(ctx, "<o1>")
	if r.Storage != "prasa" {
		t.Fatal(r.Storage)
	}
	eps, _ := db.Epochs(ctx)
	if len(eps) != 2 || eps[0].Storage != "default" || eps[1].Storage != "prasa" {
		t.Fatalf("epochs %+v", eps)
	}
	db.SetEpoch(ctx, EpochInfo{Storage: "prasa", Epoch: "2026-W41", State: StateSealed})
	if e, _ := db.Epoch(ctx, "", "2026-W41"); e.State != StateOpen {
		t.Fatal("epoch state leaked between storages")
	}
	db.DeleteEpoch(ctx, "prasa", "2026-W41")
	if _, n, _ := db.Search(ctx, SearchQuery{}); n != 3 {
		t.Fatal("delete removed other storages")
	}
}

func TestOpenRecreatesOtherSchemaVersion(t *testing.T) {
	ctx := context.Background()
	url, schema := pgtest.Schema(t, "index")
	db, err := Open(ctx, url, schema)
	if err != nil {
		t.Fatal(err)
	}
	fixture(t, db)
	// Layout of an older release.
	if _, err := db.db.Exec(`UPDATE index_meta SET version = 1; DROP TABLE pages`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	if db, err = Open(ctx, url, schema); err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if n, _ := db.CountMatches(ctx, "budzet"); n != 0 {
		t.Fatal("old index kept")
	}
	fixture(t, db)
	if n, err := db.CountMatches(ctx, "budzet"); n != 1 || err != nil {
		t.Fatalf("index with old layout was not recreated: %d %v", n, err)
	}
	var v int
	if err := db.db.QueryRow("SELECT version FROM index_meta").Scan(&v); err != nil || v != schemaVersion {
		t.Fatalf("version = %d, %v", v, err)
	}
}

func structuredPage(url, title string, at time.Time) recorder.PageInfo {
	return recorder.PageInfo{URL: url, Title: title, Text: "Treść", Kind: "page", Captured: at,
		Description: "Opis " + title, Authors: []string{"Anna Kowalska"}, Published: "2026-10-01",
		SchemaOrg: []map[string]any{
			{"@type": "NewsArticle", "headline": title, "datePublished": "2026-10-09T08:15:00+02:00"},
			{"@type": []any{"Person", "Thing"}, "name": "Anna Kowalska"},
		}}
}

func TestURLsKeepLatestCaptureMetadata(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	u := "https://serwis.pl/artykul"
	if fresh, _ := db.MarkSeen(ctx, "", []string{u, "https://serwis.pl/inny"}, t0.Add(-time.Hour)); len(fresh) != 2 || fresh[0] != u {
		t.Fatalf("fresh %v", fresh)
	}
	newer := capture("n.warc.gz", t0.Add(2*time.Hour), structuredPage(u, "Nowszy tytuł", time.Time{}),
		recorder.RecordInfo{ID: "<n1>", Type: warcio.TypeResponse, URI: u, Digest: "sha1:N", Length: 10})
	older := capture("o.warc.gz", t0, structuredPage(u, "Starszy tytuł", time.Time{}),
		recorder.RecordInfo{ID: "<o1>", Type: warcio.TypeResponse, URI: u, Digest: "sha1:O", Length: 10})
	older.Pages[0].SchemaOrg = nil
	for _, cf := range []recorder.CaptureFile{newer, older} {
		if err := db.AddCapture(ctx, cf, cf.Key()); err != nil {
			t.Fatal(err)
		}
	}
	info, err := db.URL(ctx, "", u)
	if err != nil {
		t.Fatal(err)
	}
	if info.Title != "Nowszy tytuł" || info.Description != "Opis Nowszy tytuł" || !info.LastCaptured.Equal(t0.Add(2*time.Hour)) ||
		!info.FirstSeen.Equal(t0.Add(-time.Hour)) {
		t.Fatalf("url %+v", info)
	}
	if !info.Published.Equal(time.Date(2026, 10, 9, 6, 15, 0, 0, time.UTC)) {
		t.Errorf("published %v (schema.org datePublished wins over the extracted date)", info.Published)
	}
	if strings.Join(info.SchemaTypes, ",") != "NewsArticle,Person,Thing" || strings.Join(info.Authors, ",") != "Anna Kowalska" {
		t.Errorf("types %v authors %v", info.SchemaTypes, info.Authors)
	}
	if !strings.Contains(string(info.SchemaOrg), `"headline": "Nowszy tytuł"`) {
		t.Errorf("schema_org %s", info.SchemaOrg)
	}
	pages, err := db.PageCaptures(ctx, u)
	if err != nil || len(pages) != 2 || pages[0].URLID != info.ID || pages[1].URLID != info.ID {
		t.Fatalf("captures %+v %v", pages, err)
	}
	if pages[0].Description != "Opis Nowszy tytuł" || pages[0].Published != "2026-10-09" || len(pages[0].SchemaTypes) != 3 ||
		pages[1].Published != "2026-10-01" || pages[1].SchemaOrg != nil {
		t.Errorf("page metadata %+v / %+v", pages[0], pages[1])
	}
	if n, _ := db.CountMatches(ctx, "opis nowszy"); n != 1 {
		t.Errorf("description not searchable: %d", n)
	}
	if _, err := db.URL(ctx, "inny", u); err != ErrNotFound {
		t.Errorf("urls are per storage: %v", err)
	}
}

func TestDeleteEpochCascades(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	fixture(t, db)
	if err := db.DeleteEpoch(ctx, "", "2026-W41"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"records", "pages", "files", "epoch_stats", "epochs"} {
		var n int
		if err := db.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s: %d rows left (%v)", table, n, err)
		}
	}
	if seen, _ := db.Seen(ctx, "", "https://serwis.pl/budzet"); !seen {
		t.Error("URLs must stay known after an epoch is dropped")
	}
	var fks int
	db.db.QueryRow(`SELECT count(*) FROM information_schema.table_constraints WHERE constraint_type = 'FOREIGN KEY' AND table_schema = current_schema()`).Scan(&fks)
	if fks < 6 {
		t.Errorf("%d foreign keys", fks)
	}
}

func TestClosestRechecksSURTHash(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	fixture(t, db)
	target := cdxj.SURT("https://serwis.pl/budzet")
	// A different URL whose hash collides with the target's.
	if _, err := db.db.Exec(`INSERT INTO records (record_id, storage, epoch, uri, surt, surt_hash, date, type, location, "offset", length, file)
		VALUES ('<collision>', 'default', '2026-W41', 'https://x.pl/', 'pl,x)/', $1, $2, 'response', 'l', 0, 1, 'f')`,
		surtHash(target), t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	r, err := db.Closest(ctx, "https://serwis.pl/budzet", t0.Add(time.Minute))
	if err != nil || r.ID != "<r1>" {
		t.Fatalf("closest %s %v", r.ID, err)
	}
	tl, _ := db.Timeline(ctx, "https://serwis.pl/budzet")
	if len(tl) != 1 || !tl[0].Date.Equal(t0) {
		t.Fatalf("timeline %+v", tl)
	}
}
