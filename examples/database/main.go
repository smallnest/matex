// Command database demonstrates pkg/core/db against all three supported
// drivers (postgres / mysql / sqlite) with one portable SQL dialect:
//
//   - "?" placeholders everywhere (rewritten to $1..$n for postgres)
//   - generic scanning: QueryAll[T] / QueryOne[T] / QueryScalar[T]
//     via `db` tags (unknown columns ignored, NULL → nil pointer,
//     RFC3339 string → time.Time)
//   - WithTx: commit on nil, automatic rollback on error
//   - a DAO written against db.Querier, so the same methods run against the
//     pool and inside a transaction — no transaction-only twin methods
//
// It runs with zero external dependencies by default (a throwaway pure-Go
// SQLite database):
//
//	go run ./examples/database
//
// Against a real server:
//
//	go run ./examples/database -driver postgres \
//	  -dsn 'postgres://matex:matex@localhost:5432/matex?sslmode=disable'
//	go run ./examples/database -driver mysql \
//	  -dsn 'matex:matex@tcp(localhost:3306)/matex?parseTime=true'
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/smallnest/matex/pkg/core/db"
)

// schema is deliberately portable: VARCHAR(n) instead of TEXT for the
// primary key (MySQL cannot index a bare TEXT key), no AUTO_INCREMENT /
// SERIAL / IDENTITY, and timestamps as VARCHAR filled from Go.
const schema = `CREATE TABLE IF NOT EXISTS articles (
	slug       VARCHAR(64)  NOT NULL PRIMARY KEY,
	title      VARCHAR(255) NOT NULL,
	views      INTEGER      NOT NULL DEFAULT 0,
	published  BOOLEAN      NOT NULL DEFAULT FALSE,
	note       VARCHAR(255) NULL,
	updated_at VARCHAR(64)  NOT NULL
)`

// Article is a scanned row. `db` tags match column names; a column with
// no matching field is ignored, so `SELECT *` keeps working as the table
// grows.
type Article struct {
	Slug      string    `db:"slug"       json:"slug"`
	Title     string    `db:"title"      json:"title"`
	Views     int64     `db:"views"      json:"views"`
	Published bool      `db:"published"  json:"published"`
	Note      *string   `db:"note"       json:"note"` // NULL → nil
	UpdatedAt time.Time `db:"updated_at" json:"updated_at"`
}

// articleDAO is written against db.Querier rather than *db.DB, which is what
// lets the same methods serve the pool and a transaction. An interface method
// cannot carry type parameters, so a QueryOne[T] *method* cannot live on an
// interface — the package-level helpers plus Querier are how a DAO stays
// generic *and* transaction-aware.
type articleDAO struct{}

func (articleDAO) bySlug(ctx context.Context, q db.Querier, slug string) (*Article, error) {
	return db.QueryOne[Article](ctx, q, `SELECT * FROM articles WHERE slug = ?`, slug)
}

func (articleDAO) insert(ctx context.Context, q db.Querier, a Article, now string) error {
	_, err := db.Exec(ctx, q, `INSERT INTO articles (slug, title, views, published, note, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`, a.Slug, a.Title, a.Views, a.Published, a.Note, now)
	return err
}

func (articleDAO) count(ctx context.Context, q db.Querier) (int64, error) {
	return db.QueryScalar[int64](ctx, q, `SELECT COUNT(*) FROM articles`)
}

// result is what run observed, so tests can assert on values instead of
// parsing the printed output.
type result struct {
	InitialCount  int64
	AfterCommit   int64
	AfterRollback int64
	AfterDAO      int64
	First         Article
	All           []Article
}

func main() {
	driver := flag.String("driver", "sqlite", "postgres | mysql | sqlite")
	dsn := flag.String("dsn", "", "DSN (default: a throwaway SQLite file)")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dsnVal, cleanup := resolveDSN(*driver, *dsn)
	defer cleanup()

	d, err := db.Open(ctx, db.Config{Driver: db.Driver(*driver), DSN: dsnVal})
	if err != nil {
		fatal(err)
	}
	defer func() { _ = d.Close() }()

	fmt.Printf("driver = %s\n\n", d.Driver())
	if _, err := run(ctx, d, os.Stdout); err != nil {
		fatal(err)
	}
}

// run executes the demo and writes a human-readable trace to out.
func run(ctx context.Context, d *db.DB, out io.Writer) (result, error) {
	var res result
	line := func(label, format string, a ...any) {
		_, _ = fmt.Fprintf(out, "%-12s "+format+"\n", append([]any{label}, a...)...)
	}

	// 1. DDL
	if _, err := d.Exec(ctx, schema); err != nil {
		return res, fmt.Errorf("create table: %w", err)
	}
	line("schema", "OK (driver=%s)", d.Driver())

	// 2. Write. Timestamps are formatted in Go, never now()/CURRENT_TIMESTAMP.
	now := time.Now().UTC().Format(time.RFC3339)
	const insert = `INSERT INTO articles (slug, title, views, published, note, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`
	if _, err := d.Exec(ctx, insert, "hello-matex", "Hello matex", 0, false, nil, now); err != nil {
		return res, fmt.Errorf("insert: %w", err)
	}
	if _, err := d.Exec(ctx, `UPDATE articles SET views = views + 1 WHERE slug = ?`, "hello-matex"); err != nil {
		return res, fmt.Errorf("update: %w", err)
	}

	// 3. QueryScalar: one column, one value.
	n, err := d.QueryScalar[int64](ctx, `SELECT COUNT(*) FROM articles`)
	if err != nil {
		return res, fmt.Errorf("count: %w", err)
	}
	res.InitialCount = n
	line("scalar", "count=%d", n)

	// 4. QueryOne: generic scan into a struct.
	a, err := d.QueryOne[Article](ctx, `SELECT * FROM articles WHERE slug = ?`, "hello-matex")
	if err != nil {
		return res, fmt.Errorf("query one: %w", err)
	}
	res.First = *a
	line("query one", "slug=%s views=%d published=%t note=%v updated_at=%s",
		a.Slug, a.Views, a.Published, a.Note, a.UpdatedAt.Format(time.RFC3339))

	// 5. Miss → db.ErrNoRow.
	if _, err := d.QueryOne[Article](ctx, `SELECT * FROM articles WHERE slug = ?`, "nope"); !errors.Is(err, db.ErrNoRow) {
		return res, fmt.Errorf("want db.ErrNoRow, got %w", err)
	}
	line("no row", "errors.Is(err, db.ErrNoRow) = true")

	// 6. WithTx: commits when fn returns nil.
	if err := d.WithTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		for _, slug := range []string{"tx-a", "tx-b"} {
			if _, err := tx.Exec(ctx, insert, slug, "in tx: "+slug, 0, true, nil, now); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return res, fmt.Errorf("tx commit: %w", err)
	}
	if res.AfterCommit, err = count(ctx, d); err != nil {
		return res, err
	}
	line("tx commit", "count=%d", res.AfterCommit)

	// 7. WithTx: an error means rollback — the row must not survive.
	sentinel := errors.New("business check failed")
	err = d.WithTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if _, err := tx.Exec(ctx, insert, "tx-c", "should not persist", 0, false, nil, now); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		return res, fmt.Errorf("want sentinel error, got %w", err)
	}
	if res.AfterRollback, err = count(ctx, d); err != nil {
		return res, err
	}
	line("tx rollback", "count=%d (unchanged)", res.AfterRollback)

	// 8. QueryAll.
	all, err := d.QueryAll[Article](ctx, `SELECT * FROM articles ORDER BY slug`)
	if err != nil {
		return res, fmt.Errorf("query all: %w", err)
	}
	res.All = all
	line("query all", "%d row(s), first=%s", len(all), all[0].Slug)

	// 9. One DAO, two call sites: the same methods run inside a transaction
	// and against the pool. This is what db.Querier buys — without it every
	// DAO method would need a second, transaction-only twin.
	dao := articleDAO{}
	if err := d.WithTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := dao.insert(ctx, tx, Article{Slug: "tx-d", Title: "written via the DAO"}, now); err != nil {
			return err
		}
		// A read inside the transaction sees the transaction's own write.
		_, err := dao.bySlug(ctx, tx, "tx-d")
		return err
	}); err != nil {
		return res, fmt.Errorf("dao in tx: %w", err)
	}
	if res.AfterDAO, err = dao.count(ctx, d); err != nil {
		return res, err
	}
	line("dao + tx", "count=%d (same DAO methods, tx and pool)", res.AfterDAO)

	return res, nil
}

func count(ctx context.Context, d *db.DB) (int64, error) {
	n, err := d.QueryScalar[int64](ctx, `SELECT COUNT(*) FROM articles`)
	if err != nil {
		return 0, fmt.Errorf("count: %w", err)
	}
	return n, nil
}

// resolveDSN returns the DSN plus a cleanup func. Without -dsn it makes a
// throwaway SQLite database in a temp dir (so the example needs nothing).
func resolveDSN(driver, dsn string) (string, func()) {
	if dsn != "" {
		return dsn, func() {}
	}
	if driver != string(db.SQLite) {
		fmt.Fprintf(os.Stderr, "fatal: -dsn is required for driver %q\n", driver)
		os.Exit(2)
	}
	dir, err := os.MkdirTemp("", "matex-example-database-")
	if err != nil {
		fatal(err)
	}
	return "file:" + filepath.Join(dir, "example.db"), func() { _ = os.RemoveAll(dir) }
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "fatal:", err)
	os.Exit(1)
}
