// Package pgtest gives tests an isolated place in the test database named by
// HOARFROST_TEST_DATABASE: a fresh schema, or a whole database for tests that
// run the binary. Tests are skipped when the variable is unset, unless
// HOARFROST_TEST_DATABASE_REQUIRED is set (CI).
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/ogefest/hoarfrost/internal/pgdb"
)

// URL returns the test database URL or skips the test.
func URL(t testing.TB) string {
	t.Helper()
	u := os.Getenv("HOARFROST_TEST_DATABASE")
	if u == "" {
		if os.Getenv("HOARFROST_TEST_DATABASE_REQUIRED") != "" {
			t.Fatal("HOARFROST_TEST_DATABASE is required")
		}
		t.Skip("HOARFROST_TEST_DATABASE not set (e.g. postgres://user:pass@host/hoarfrost_test)")
	}
	return u
}

func name(prefix string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b) // never fails
	return prefix + "_" + hex.EncodeToString(b)
}

// Schema returns the test database URL and a new schema name; the schema
// is dropped when the test ends.
func Schema(t testing.TB, prefix string) (string, string) {
	t.Helper()
	u := URL(t)
	s := NewSchemaName(prefix)
	t.Cleanup(func() {
		if err := DropSchema(u, s); err != nil {
			t.Logf("drop schema %s: %v", s, err)
		}
	})
	return u, s
}

// NewSchemaName returns a unique schema name, for fixtures shared by tests.
func NewSchemaName(prefix string) string { return name("t_" + prefix) }

// DropSchema removes a schema created for tests.
func DropSchema(url, schema string) error {
	db, err := pgdb.Open(context.Background(), url, "public")
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(`DROP SCHEMA IF EXISTS ` + pgdb.Ident(schema) + ` CASCADE`)
	return err
}

// Database creates an empty database next to the test database and returns
// its URL; it is dropped when the test ends.
func Database(t testing.TB) string {
	t.Helper()
	u := URL(t)
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	db, err := pgdb.Open(context.Background(), u, "public")
	if err != nil {
		t.Fatal(err)
	}
	dbName := name("hoarfrost_e2e")
	if _, err := db.Exec(`CREATE DATABASE ` + pgdb.Ident(dbName)); err != nil {
		db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		defer db.Close()
		if _, err := db.Exec(`DROP DATABASE IF EXISTS ` + pgdb.Ident(dbName) + ` WITH (FORCE)`); err != nil {
			t.Logf("drop database %s: %v", dbName, err)
		}
	})
	parsed.Path = "/" + strings.TrimPrefix(dbName, "/")
	return parsed.String()
}
