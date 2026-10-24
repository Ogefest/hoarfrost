// Package pgdb opens PostgreSQL databases with the settings hoarfrost relies
// on and applies schema migrations. Every component keeps its tables in its
// own schema of one database, selected through the search path.
package pgdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

// Open connects to the database at url with schema as the search path,
// creating the schema if needed.
func Open(ctx context.Context, url, schema string) (*sql.DB, error) {
	if url == "" {
		return nil, fmt.Errorf("pgdb: no database URL (set HOARFROST_DATABASE)")
	}
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("pgdb: %w", err)
	}
	cfg.RuntimeParams["search_path"] = schema
	cfg.RuntimeParams["timezone"] = "UTC"
	db := stdlib.OpenDB(*cfg)
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(4)
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS `+Ident(schema)); err != nil {
		db.Close()
		return nil, fmt.Errorf("pgdb: schema %s: %w", schema, err)
	}
	return db, nil
}

// Ident quotes an identifier.
func Ident(name string) string { return pgx.Identifier{name}.Sanitize() }

// ResetSchema drops a schema with everything in it and creates it empty.
func ResetSchema(ctx context.Context, db *sql.DB, schema string) error {
	_, err := db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+Ident(schema)+` CASCADE; CREATE SCHEMA `+Ident(schema))
	return err
}

// Migrate applies migrations[i] for every i at or above the version recorded
// in schema_version of the current schema. Concurrent callers are serialized.
func Migrate(ctx context.Context, db *sql.DB, migrations []string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext(current_schema()))`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return err
	}
	var v int
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(version), 0) FROM schema_version`).Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
	}
	if len(migrations) > v {
		if _, err := tx.ExecContext(ctx, `DELETE FROM schema_version; INSERT INTO schema_version VALUES (`+fmt.Sprint(len(migrations))+`)`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Rebind numbers the '?' placeholders of a query assembled from parts as
// $1, $2, ...; question marks inside single-quoted literals are kept.
func Rebind(q string) string {
	var b strings.Builder
	n, quoted := 0, false
	for _, r := range q {
		switch {
		case r == '\'':
			quoted = !quoted
		case r == '?' && !quoted:
			n++
			b.WriteString("$" + strconv.Itoa(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// IsUniqueViolation reports whether err is a unique constraint violation.
func IsUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}
