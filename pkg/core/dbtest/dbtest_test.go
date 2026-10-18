package dbtest_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/smallnest/matex/pkg/core/db"
	"github.com/smallnest/matex/pkg/core/dbtest"
)

// dbtest is the offline database every DAO test relies on: it must return
// a live, migrated SQLite DB and leave migrations applied exactly once.

func TestStartReturnsMigratedDatabase(t *testing.T) {
	d := dbtest.Start(t)

	n, err := db.QueryScalar[int64](t.Context(), d, `SELECT COUNT(*) FROM schema_migrations`)
	if err != nil {
		t.Fatalf("schema_migrations: %v", err)
	}
	if n == 0 {
		t.Fatal("no migrations were recorded")
	}

	files, _ := filepath.Glob(filepath.Join(migrationsDir(t), "*.sql"))
	if int(n) != len(files) {
		t.Fatalf("recorded %d migrations, %d files on disk", n, len(files))
	}
}

func TestStartIsIdempotent(t *testing.T) {
	d := dbtest.Start(t)

	if err := d.ApplyMigrations(t.Context(), migrationsDir(t)); err != nil {
		t.Fatalf("second ApplyMigrations: %v", err)
	}
	n, err := db.QueryScalar[int64](t.Context(), d, `SELECT COUNT(*) FROM schema_migrations`)
	if err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(migrationsDir(t), "*.sql"))
	if int(n) != len(files) {
		t.Fatalf("re-running migrations duplicated rows: %d rows for %d files", n, len(files))
	}
}

func TestStartIsolatesTests(t *testing.T) {
	// Each call gets its own file, so a table created here is absent in a
	// second call.
	a := dbtest.Start(t)
	if _, err := a.Exec(t.Context(), `CREATE TABLE scratch (id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	b := dbtest.Start(t)
	if _, err := db.QueryScalar[int64](t.Context(), b, `SELECT COUNT(*) FROM scratch`); err == nil {
		t.Fatal("a second dbtest.Start saw the first test's table")
	}
}

func migrationsDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "..", "..", "..", "migrations")
}
