// Package dbtest provides a migrated database for tests with no external
// dependency: by default a per-test SQLite file via modernc.org/sqlite
// (pure Go, no cgo, nothing to download), so `go test ./...` needs no Docker
// and works offline. Migrations from the repo's migrations/ directory are
// applied automatically.
//
// Point DB_TEST_DRIVER (postgres|mysql|sqlite) and DB_TEST_DSN at a real
// server to run the same tests against it, e.g.
//
//	DB_TEST_DRIVER=postgres DB_TEST_DSN='postgres://...' go test ./...
package dbtest

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/smallnest/matex/pkg/core/db"
)

// Start returns a migrated *db.DB with cleanup wired to t. It uses SQLite
// unless DB_TEST_DSN is set.
func Start(t testing.TB) *db.DB {
	t.Helper()
	cfg := sqliteConfig(t)
	if dsn := os.Getenv("DB_TEST_DSN"); dsn != "" {
		driver := db.Driver(os.Getenv("DB_TEST_DRIVER"))
		if driver == "" {
			driver = db.Postgres
		}
		cfg = db.Config{Driver: driver, DSN: dsn}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d, err := db.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("dbtest: open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.ApplyMigrations(ctx, migrationsDir(t)); err != nil {
		t.Fatalf("dbtest: migrate: %v", err)
	}
	return d
}

// sqliteConfig returns a fresh file-backed SQLite DB. A temp file (rather
// than :memory:) lets the pool open more than one connection safely.
func sqliteConfig(t testing.TB) db.Config {
	t.Helper()
	return db.Config{
		Driver: db.SQLite,
		DSN:    "file:" + filepath.Join(t.TempDir(), "matex.db"),
	}
}

// migrationsDir locates <repo>/migrations relative to this file
// (pkg/core/dbtest → up three levels → repo root).
func migrationsDir(t testing.TB) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("dbtest: cannot locate source file")
	}
	dir := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "migrations"))
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dbtest: migrations dir: %v", err)
	}
	return dir
}
