package db

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestRebind(t *testing.T) {
	cases := []struct{ in, want string }{
		{"SELECT * FROM t WHERE a = ? AND b = ?", "SELECT * FROM t WHERE a = $1 AND b = $2"},
		{"SELECT '?' AS q, ? AS v", "SELECT '?' AS q, $1 AS v"},
		{"SELECT 'a''b' AS s, ?", "SELECT 'a''b' AS s, $1"},
		{"-- comment ?\nSELECT ?", "-- comment ?\nSELECT $1"},
		{"/* ? */ SELECT ?", "/* ? */ SELECT $1"},
		{`SELECT "col?" FROM t WHERE x = ?`, `SELECT "col?" FROM t WHERE x = $1`},
		{"SELECT data ?? 'k', ?", "SELECT data ? 'k', $1"},
	}
	for _, c := range cases {
		if got := rebind(Postgres, c.in); got != c.want {
			t.Errorf("rebind(postgres, %q) = %q, want %q", c.in, got, c.want)
		}
	}
	// MySQL and SQLite keep "?" as-is.
	for _, d := range []Driver{MySQL, SQLite} {
		const q = "SELECT ? , '?'"
		if got := rebind(d, q); got != q {
			t.Errorf("rebind(%s) changed query: %q", d, got)
		}
	}
}

func TestOpenValidation(t *testing.T) {
	if _, err := Open(t.Context(), Config{DSN: "x"}); err == nil {
		t.Fatal("expected error for missing/blank dsn")
	}
	if _, err := Open(t.Context(), Config{}); err == nil {
		t.Fatal("expected error for empty dsn")
	}
	if _, err := Open(t.Context(), Config{Driver: "oracle", DSN: "x"}); err == nil {
		t.Fatal("expected error for unknown driver")
	}
}

type userRow struct {
	ID        int64     `db:"id"`
	Name      string    `db:"name"`
	Score     int64     `db:"score"`
	Active    bool      `db:"active"`
	CreatedAt time.Time `db:"created_at"`
}

func sqliteTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(t.Context(), Config{
		Driver: SQLite,
		DSN:    "file:" + filepath.Join(t.TempDir(), "test.db"),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestQueryHelpersSQLite(t *testing.T) {
	ctx := t.Context()
	d := sqliteTestDB(t)

	if _, err := d.Exec(ctx, `CREATE TABLE users (
		id         INTEGER PRIMARY KEY,
		name       VARCHAR(255) NOT NULL,
		score      INTEGER NOT NULL DEFAULT 0,
		active     BOOLEAN NOT NULL DEFAULT 0,
		created_at VARCHAR(64) NOT NULL
	)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	const ts = "2026-01-02T03:04:05Z"
	if _, err := d.Exec(ctx,
		`INSERT INTO users (id, name, score, active, created_at) VALUES (?, ?, ?, ?, ?)`,
		1, "alice", 10, true, ts); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// QueryAll + struct scan (types: int64, string, bool, time.Time).
	all, err := d.QueryAll[userRow](ctx, `SELECT * FROM users ORDER BY id`)
	if err != nil {
		t.Fatalf("query all: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("rows: %d", len(all))
	}
	got := all[0]
	if got.ID != 1 || got.Name != "alice" || got.Score != 10 || !got.Active {
		t.Fatalf("row: %+v", got)
	}
	if want := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC); !got.CreatedAt.Equal(want) {
		t.Fatalf("created_at: %s", got.CreatedAt)
	}

	// QueryOne + ErrNoRow.
	if _, err := d.QueryOne[userRow](ctx, `SELECT * FROM users WHERE id = ?`, 1); err != nil {
		t.Fatalf("query one: %v", err)
	}
	if _, err := d.QueryOne[userRow](ctx, `SELECT * FROM users WHERE id = ?`, 404); !errors.Is(err, ErrNoRow) {
		t.Fatalf("want ErrNoRow, got %v", err)
	}

	// QueryScalar.
	n, err := d.QueryScalar[int64](ctx, `SELECT COUNT(*) FROM users`)
	if err != nil || n != 1 {
		t.Fatalf("count: %d, %v", n, err)
	}
	name, err := d.QueryScalar[string](ctx, `SELECT name FROM users WHERE id = ?`, 1)
	if err != nil || name != "alice" {
		t.Fatalf("name: %q, %v", name, err)
	}

	// Unknown columns are ignored (SELECT * stays usable).
	type lax struct {
		Name string `db:"name"`
	}
	if v, err := d.QueryOne[lax](ctx, `SELECT * FROM users WHERE id = ?`, 1); err != nil || v.Name != "alice" {
		t.Fatalf("lax: %+v, %v", v, err)
	}
}

func TestNullableAndScannerFields(t *testing.T) {
	ctx := t.Context()
	d := sqliteTestDB(t)
	if _, err := d.Exec(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY, note VARCHAR(255))`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO t (id, note) VALUES (?, ?)`, 1, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO t (id, note) VALUES (?, ?)`, 2, "hi"); err != nil {
		t.Fatal(err)
	}

	type ptrRow struct {
		ID   int64   `db:"id"`
		Note *string `db:"note"` // nullable via pointer
	}
	type nullRow struct {
		ID   int64          `db:"id"`
		Note sql.NullString `db:"note"` // nullable via sql.Null*
	}

	ptrs, err := d.QueryAll[ptrRow](ctx, `SELECT * FROM t ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	if len(ptrs) != 2 {
		t.Fatalf("rows: %d", len(ptrs))
	}
	if ptrs[0].Note != nil {
		t.Fatalf("NULL not preserved: %+v", ptrs[0])
	}
	if ptrs[1].Note == nil || *ptrs[1].Note != "hi" {
		t.Fatalf("value not scanned: %+v", ptrs[1])
	}

	nulls, err := d.QueryAll[nullRow](ctx, `SELECT * FROM t ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	if nulls[0].Note.Valid {
		t.Fatalf("NULL not preserved: %+v", nulls[0])
	}
	if !nulls[1].Note.Valid || nulls[1].Note.String != "hi" {
		t.Fatalf("value not scanned: %+v", nulls[1])
	}
}

func TestWithTx(t *testing.T) {
	ctx := t.Context()
	d := sqliteTestDB(t)
	if _, err := d.Exec(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}

	// commit
	if err := d.WithTx(ctx, func(ctx context.Context, tx *Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO t (id) VALUES (?)`, 1)
		return err
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if n, _ := d.QueryScalar[int64](ctx, `SELECT COUNT(*) FROM t`); n != 1 {
		t.Fatalf("after commit: %d", n)
	}

	// rollback on error
	boom := errors.New("boom")
	err := d.WithTx(ctx, func(ctx context.Context, tx *Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO t (id) VALUES (?)`, 2); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("want boom, got %v", err)
	}
	if n, _ := d.QueryScalar[int64](ctx, `SELECT COUNT(*) FROM t`); n != 1 {
		t.Fatalf("rollback failed: %d", n)
	}
}
