package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ogefest/hoarfrost/internal/appdb"
	"github.com/ogefest/hoarfrost/internal/config"
)

// Conversions of SQLite values: booleans were 0/1 integers, times RFC 3339
// text, Unix seconds or Unix milliseconds (0 meaning none).
var (
	toBool      = func(v any) any { n, _ := v.(int64); return n != 0 }
	fromRFC3339 = func(v any) any {
		s, _ := v.(string)
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t
		}
		return nil
	}
	fromUnix = func(v any) any {
		if n, ok := v.(int64); ok && n > 0 {
			return time.Unix(n, 0)
		}
		return nil
	}
	fromUnixMilli = func(v any) any {
		if n, ok := v.(int64); ok && n > 0 {
			return time.UnixMilli(n)
		}
		return nil
	}
)

// sqliteTables lists the application tables copied from app.sqlite, parents
// first.
var sqliteTables = []struct {
	name     string
	cols     []string
	convert  map[string]func(any) any
	identity bool
}{
	{"users", []string{"id", "login", "pwhash", "role", "created"}, map[string]func(any) any{"created": fromRFC3339}, true},
	{"sessions", []string{"id", "user_id", "expires", "csrf"}, map[string]func(any) any{"expires": fromRFC3339}, false},
	{"api_tokens", []string{"id", "user_id", "token_hash", "name", "created", "last_used"},
		map[string]func(any) any{"created": fromRFC3339, "last_used": fromRFC3339}, true},
	{"jobs", []string{"id", "url", "domain", "source", "storage", "engine", "priority", "fallback", "state", "worker", "epoch",
		"lease_until", "not_before", "attempts", "last_error", "capture", "created", "finished"},
		map[string]func(any) any{"fallback": toBool, "lease_until": fromUnixMilli, "not_before": fromUnixMilli, "created": fromUnix, "finished": fromUnix}, true},
	{"sources", []string{"name", "config", "created", "updated", "last_poll", "found", "new", "last_error"},
		map[string]func(any) any{"created": fromUnixMilli, "updated": fromUnixMilli, "last_poll": fromUnixMilli}, false},
	{"settings", []string{"key", "value"}, nil, false},
}

// cmdImportSQLite copies the application data of an instance that used
// SQLite (app.sqlite) into the database. The index is not copied: it is
// rebuilt from storage when the coordinator starts.
func cmdImportSQLite(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("import-sqlite", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: hoarfrost import-sqlite <app.sqlite>")
	}
	if _, err := os.Stat(fs.Arg(0)); err != nil {
		return err
	}
	src, err := sql.Open("sqlite", "file:"+fs.Arg(0)+"?mode=ro")
	if err != nil {
		return err
	}
	defer src.Close()
	env, err := config.FromEnvironment()
	if err != nil {
		return err
	}
	dst, err := appdb.Open(ctx, env.Database, appdb.Schema)
	if err != nil {
		return err
	}
	defer dst.Close()

	tx, err := dst.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, t := range sqliteTables {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM `+t.name).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("import-sqlite: table %s.%s is not empty; import only into a new database", appdb.Schema, t.name)
		}
	}
	for _, t := range sqliteTables {
		n, err := copyTable(ctx, src, tx, t.name, t.cols, t.convert)
		if err != nil {
			return fmt.Errorf("import-sqlite: %s: %w", t.name, err)
		}
		if t.identity {
			if _, err := tx.ExecContext(ctx, `SELECT setval(pg_get_serial_sequence('`+t.name+`', 'id'), coalesce(max(id), 0) + 1, false) FROM `+t.name); err != nil {
				return err
			}
		}
		fmt.Fprintf(os.Stderr, "%s: %d rows\n", t.name, n)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "done; the index is rebuilt from storage when 'hoarfrost serve' starts (or run 'hoarfrost reindex')")
	return nil
}

func copyTable(ctx context.Context, src *sql.DB, dst *sql.Tx, table string, cols []string, convert map[string]func(any) any) (int, error) {
	rows, err := src.QueryContext(ctx, `SELECT `+strings.Join(cols, ", ")+` FROM `+table)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	marks := make([]string, len(cols))
	for i := range marks {
		marks[i] = fmt.Sprintf("$%d", i+1)
	}
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = `"` + c + `"`
	}
	stmt, err := dst.PrepareContext(ctx, `INSERT INTO `+table+` (`+strings.Join(quoted, ", ")+`) VALUES (`+strings.Join(marks, ", ")+`)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	n := 0
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return n, err
		}
		for i, c := range cols {
			if b, ok := vals[i].([]byte); ok {
				vals[i] = string(b)
			}
			if f := convert[c]; f != nil && vals[i] != nil {
				vals[i] = f(vals[i])
			}
		}
		if _, err := stmt.ExecContext(ctx, vals...); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}
