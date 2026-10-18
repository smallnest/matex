//go:build pgtest

package db_test

import (
	"os"
	"sync"
	"testing"

	"github.com/smallnest/matex/pkg/core/db"
)

// Run with:
//
//	DB_TEST_DSN='postgres://...' go test -tags pgtest ./pkg/core/db -run Migrations
//
// Two replicas booting together must not race the same migration file: the
// advisory lock serializes them, so the second sees the file as applied.
func TestConcurrentMigrationsOnPostgres(t *testing.T) {
	dsn := os.Getenv("DB_TEST_DSN")
	if dsn == "" {
		t.Skip("set DB_TEST_DSN to run against postgres")
	}
	dir := t.TempDir()
	// A slow, non-idempotent DDL statement: without the lock the second
	// replica would fail here.
	write(t, dir, "0001_lock_probe.sql", `
		CREATE TABLE lock_probe AS SELECT pg_sleep(1) AS slept;
		INSERT INTO schema_migrations (version, applied_at) VALUES ('dup', 'x');
	`)

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		d, err := db.Open(t.Context(), db.Config{Driver: db.Postgres, DSN: dsn})
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		wg.Go(func() { errs[i] = d.ApplyMigrations(t.Context(), dir) })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("replica %d: %v", i, err)
		}
	}
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(dir+"/"+name, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
