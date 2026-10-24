package appdb

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ogefest/hoarfrost/internal/pgdb"
	"github.com/ogefest/hoarfrost/internal/pgdb/pgtest"
)

func TestOpenIsIdempotent(t *testing.T) {
	ctx := context.Background()
	url, schema := pgtest.Schema(t, "appdb")
	db, err := Open(ctx, url, schema)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO users(login, pwhash, role) VALUES ('admin', 'x', 'admin');
INSERT INTO jobs(url, domain, source, engine) VALUES ('https://example.org/', 'example.org', 'api', 'static')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	db, err = Open(ctx, url, schema)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var users int
	var storage string
	if err := db.QueryRow(`SELECT count(*) FROM users`).Scan(&users); err != nil || users != 1 {
		t.Fatalf("users = %d, %v", users, err)
	}
	if err := db.QueryRow(`SELECT storage FROM jobs`).Scan(&storage); err != nil || storage != "default" {
		t.Fatalf("storage = %q, %v", storage, err)
	}
}

// TestMigratesTimesToTimestamps upgrades data written by the first schema
// (RFC 3339 text, Unix seconds and milliseconds) and checks every time.
func TestMigratesTimesToTimestamps(t *testing.T) {
	ctx := context.Background()
	url, schema := pgtest.Schema(t, "appdb")
	db, err := pgdb.Open(ctx, url, schema)
	if err != nil {
		t.Fatal(err)
	}
	if err := pgdb.Migrate(ctx, db, migrations[:1]); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 12, 6, 50, 27, 0, time.UTC)
	for _, q := range []string{
		`INSERT INTO users(id, login, pwhash, role, created) VALUES (1, 'admin', 'x', 'admin', '2026-10-12T06:50:27Z')`,
		`INSERT INTO sessions(id, user_id, expires, csrf) VALUES ('s', 1, '2026-10-12T06:50:27Z', 'c')`,
		`INSERT INTO api_tokens(user_id, token_hash, name, created, last_used) VALUES (1, 'h', 'n', '2026-10-12T06:50:27Z', NULL)`,
		fmt.Sprintf(`INSERT INTO jobs(url, domain, source, engine, created, finished, lease_until, not_before)
			VALUES ('https://a.pl/', 'a.pl', 'api', 'static', %[1]d, %[1]d, %[2]d, 0), ('https://b.pl/', 'b.pl', 'api', 'static', %[1]d, NULL, NULL, %[2]d)`,
			at.Unix(), at.UnixMilli()),
		fmt.Sprintf(`INSERT INTO sources(name, config, created, updated, last_poll) VALUES ('s', '{}', %[1]d, %[1]d, %[1]d)`, at.UnixMilli()),
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	if db, err = Open(ctx, url, schema); err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	check := func(q string, want time.Time) {
		t.Helper()
		var got *time.Time
		if err := db.QueryRow(q).Scan(&got); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if want.IsZero() != (got == nil) || got != nil && !got.Equal(want) {
			t.Errorf("%s = %v, want %v", q, got, want)
		}
	}
	check(`SELECT created FROM users`, at)
	check(`SELECT expires FROM sessions`, at)
	check(`SELECT created FROM api_tokens`, at)
	check(`SELECT last_used FROM api_tokens`, time.Time{})
	check(`SELECT created FROM jobs WHERE domain = 'a.pl'`, at)
	check(`SELECT finished FROM jobs WHERE domain = 'a.pl'`, at)
	check(`SELECT lease_until FROM jobs WHERE domain = 'a.pl'`, at)
	check(`SELECT not_before FROM jobs WHERE domain = 'a.pl'`, time.Time{})
	check(`SELECT not_before FROM jobs WHERE domain = 'b.pl'`, at)
	check(`SELECT last_poll FROM sources`, at)
	var types string
	db.QueryRow(`SELECT string_agg(DISTINCT data_type, ',') FROM information_schema.columns WHERE table_schema = current_schema()
		AND column_name IN ('created', 'updated', 'expires', 'last_used', 'lease_until', 'not_before', 'finished', 'last_poll')`).Scan(&types)
	if types != "timestamp with time zone" {
		t.Errorf("time columns: %s", types)
	}
}
