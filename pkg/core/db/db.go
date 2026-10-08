// Package db provides SQL access over database/sql with three drivers:
//
//   - PostgreSQL via github.com/jackc/pgx/v5/stdlib (driver name "pgx")
//   - MySQL via github.com/go-sql-driver/mysql (driver name "mysql")
//   - SQLite via modernc.org/sqlite (driver name "sqlite", pure Go, no cgo)
//
// The zero magic: SQL stays SQL. On top of database/sql it adds:
//
//   - a DB handle whose query helpers carry the driver (so `?` placeholders
//     are rewritten to $1..$n for PostgreSQL — write portable SQL once)
//
//   - generic scan helpers QueryAll[T]/QueryOne[T]/QueryScalar[T] (db tags)
//
//   - WithTx for transaction scoping with automatic rollback
//
//   - slow query logging and error wrapping into errs kinds
//
//     type UserDAO struct{ db *db.DB }
//     func (d *UserDAO) GetByID(ctx context.Context, id int64) (*User, error) {
//     return d.db.QueryOne[User](ctx, `SELECT * FROM users WHERE id = ?`, id)
//     }
//
// Business code never imports a driver (enforced by depguard): it deals in
// *db.DB / *db.Tx / db.Result only.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	// Drivers register themselves with database/sql. Blank-imported here so
	// pkg/core/db is the single place that decides which drivers ship.
	_ "github.com/go-sql-driver/mysql" // "mysql"
	_ "github.com/jackc/pgx/v5/stdlib" // "pgx"
	_ "modernc.org/sqlite"             // "sqlite" (pure Go, CGO_ENABLED=0)

	"github.com/smallnest/matex/pkg/core/errs"
)

// Driver selects the SQL dialect and its database/sql driver.
type Driver string

const (
	// Postgres is the default driver (pgx stdlib).
	Postgres Driver = "postgres"
	// MySQL uses go-sql-driver/mysql.
	MySQL Driver = "mysql"
	// SQLite uses modernc.org/sqlite — pure Go, no cgo.
	SQLite Driver = "sqlite"
)

// driverName maps a Driver to the database/sql driver name. The empty
// Driver defaults to Postgres (zero-value tolerance for Config).
func (d Driver) driverName() (string, error) {
	switch d {
	case Postgres, "":
		return "pgx", nil
	case MySQL:
		return "mysql", nil
	case SQLite:
		return "sqlite", nil
	default:
		return "", fmt.Errorf("db: unknown driver %q (want postgres|mysql|sqlite)", string(d))
	}
}

// ErrNoRow is returned by QueryOne/QueryScalar when nothing matched.
var ErrNoRow = errors.New("db: no row")

// Result is the outcome of Exec, re-exported so business code needs no
// database/sql import.
type Result = sql.Result

// Defaults are applied both via struct tags (when the config file omits a
// key) and by Open's zero-value tolerance (when Config is built directly,
// e.g. by dbtest). Keep each tag value in sync with its constant.
const (
	defaultMaxOpenConns   int32 = 16
	defaultMaxIdleConns   int32 = 2
	defaultConnectTimeout       = 5 * time.Second
	defaultSlowThreshold        = 250 * time.Millisecond
)

// Config configures a pool. DSN is required; other fields default to sane
// production values.
//
// Placeholders: business SQL uses "?" for every driver. For Postgres the
// helpers rewrite them to $1..$n. DSN examples:
//
//	postgres  postgres://user:pass@host:5432/db?sslmode=disable
//	mysql     user:pass@tcp(host:3306)/db?parseTime=true   (parseTime → time.Time)
//	sqlite    file:matex.db   or   :memory:
type Config struct {
	Driver          Driver        `json:"driver" default:"postgres"`
	DSN             string        `json:"dsn" env:"DB_DSN"`
	MaxOpenConns    int32         `json:"max_open_conns" default:"16"` // keep in sync with defaultMaxOpenConns
	MaxIdleConns    int32         `json:"max_idle_conns" default:"2"`  // keep in sync with defaultMaxIdleConns
	ConnMaxLifetime time.Duration `json:"conn_max_lifetime" default:"30m"`
	ConnMaxIdleTime time.Duration `json:"conn_max_idle_time" default:"5m"`
	ConnectTimeout  time.Duration `json:"connect_timeout" default:"5s"`   // keep in sync with defaultConnectTimeout
	SlowThreshold   time.Duration `json:"slow_threshold" default:"250ms"` // keep in sync with defaultSlowThreshold
}

// DB is a database/sql pool bound to one driver. It is safe for concurrent
// use. A nil *DB is a valid "not configured" value for optional deps.
type DB struct {
	sql    *sql.DB
	driver Driver
	slow   time.Duration
}

// Open opens a pool for cfg.Driver, applies pool limits and verifies
// connectivity. cfg.Driver defaults to Postgres.
func Open(ctx context.Context, cfg Config) (*DB, error) {
	if cfg.DSN == "" {
		return nil, errors.New("db: dsn is required")
	}
	driver := cfg.Driver
	if driver == "" {
		driver = Postgres
	}
	name, err := driver.driverName()
	if err != nil {
		return nil, err
	}
	connectTimeout := cfg.ConnectTimeout
	if connectTimeout <= 0 {
		connectTimeout = defaultConnectTimeout
	}
	slow := cfg.SlowThreshold
	if slow <= 0 {
		slow = defaultSlowThreshold
	}

	sqlDB, err := sql.Open(name, cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("db: open %s: %w", driver, err)
	}
	maxOpen := cfg.MaxOpenConns
	if maxOpen <= 0 {
		maxOpen = defaultMaxOpenConns
		if driver == SQLite {
			// SQLite has a single writer; a one-connection pool avoids
			// SQLITE_BUSY without changing the caller's SQL.
			maxOpen = 1
		}
	}
	sqlDB.SetMaxOpenConns(int(maxOpen))
	maxIdle := cfg.MaxIdleConns
	if maxIdle <= 0 {
		maxIdle = defaultMaxIdleConns
	}
	sqlDB.SetMaxIdleConns(int(maxIdle))
	if cfg.ConnMaxLifetime > 0 {
		sqlDB.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	}
	if cfg.ConnMaxIdleTime > 0 {
		sqlDB.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)
	}

	pingCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if err := sqlDB.PingContext(pingCtx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("db: ping %s: %w", driver, err)
	}
	return &DB{sql: sqlDB, driver: driver, slow: slow}, nil
}

// Driver reports the configured driver.
func (d *DB) Driver() Driver { return d.driver }

// SQL exposes the underlying *sql.DB for advanced use (e.g. a driver-specific
// bulk API). Prefer the helpers.
func (d *DB) SQL() *sql.DB { return d.sql }

// Ping verifies connectivity.
func (d *DB) Ping(ctx context.Context) error { return d.sql.PingContext(ctx) }

// Close closes the pool.
func (d *DB) Close() error { return d.sql.Close() }

// Exec runs a statement and returns its result. Use "?" placeholders.
func (d *DB) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return Exec(ctx, d, query, args...)
}

// QueryAll runs the query and scans every row into []T via db tags.
func (d *DB) QueryAll[T any](ctx context.Context, query string, args ...any) ([]T, error) {
	return QueryAll[T](ctx, d, query, args...)
}

// QueryOne scans the first row into *T; ErrNoRow when nothing matched.
func (d *DB) QueryOne[T any](ctx context.Context, query string, args ...any) (*T, error) {
	return QueryOne[T](ctx, d, query, args...)
}

// QueryScalar scans a single column of the first row (e.g. COUNT(*)).
func (d *DB) QueryScalar[T any](ctx context.Context, query string, args ...any) (T, error) {
	return QueryScalar[T](ctx, d, query, args...)
}

// WithTx runs fn inside a transaction: commit on a nil error, rollback
// otherwise (including on panic).
func (d *DB) WithTx(ctx context.Context, fn func(ctx context.Context, tx *Tx) error) error {
	return withTx(ctx, d.sql, d.driver, d.slow, fn)
}

// Tx is the transaction handle passed to DB.WithTx. It offers the same
// helpers as DB under the Querier interface, so a DAO written against
// db.Querier works here without a second code path.
type Tx struct {
	tx     *sql.Tx
	driver Driver
	slow   time.Duration
}

// Exec runs a statement inside the transaction.
func (t *Tx) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return Exec(ctx, t, query, args...)
}

// QueryAll scans every row into []T.
func (t *Tx) QueryAll[T any](ctx context.Context, query string, args ...any) ([]T, error) {
	return QueryAll[T](ctx, t, query, args...)
}

// QueryOne scans the first row into *T; ErrNoRow when nothing matched.
func (t *Tx) QueryOne[T any](ctx context.Context, query string, args ...any) (*T, error) {
	return QueryOne[T](ctx, t, query, args...)
}

// QueryScalar scans a single column of the first row.
func (t *Tx) QueryScalar[T any](ctx context.Context, query string, args ...any) (T, error) {
	return QueryScalar[T](ctx, t, query, args...)
}

// executor is the subset of *sql.DB / *sql.Tx the helpers need.
type executor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// conn is a query surface: where statements go, how placeholders are
// rewritten, and when to complain about slowness.
type conn struct {
	ex     executor
	driver Driver
	slow   time.Duration
}

// Querier is the read/write surface shared by *DB and *Tx. A DAO method that
// takes a Querier runs unchanged inside or outside a transaction:
//
//	func (d *UserDAO) ByID(ctx context.Context, q db.Querier, id int64) (*User, error) {
//		return db.QueryOne[User](ctx, q, `SELECT * FROM users WHERE id = ?`, id)
//	}
//
//	u, err := dao.ByID(ctx, env.DB, 1)                      // pool
//	err = db.WithTx(ctx, func(ctx context.Context, tx *db.Tx) error {
//		_, err := dao.ByID(ctx, tx, 1)                      // same code, in a tx
//		return err
//	})
//
// The interface is sealed — its one method is unexported — so *DB and *Tx
// are the only implementations and business code cannot invent a third way
// to reach a driver.
//
// It exists because an interface method cannot carry type parameters. A
// *type* may have generic methods (Go 1.27), an interface may not — the
// compiler rejects it with "interface method must have no type parameters" —
// so QueryAll[T] can only ever live as a package-level function.
type Querier interface {
	conn() conn
}

// conn implements Querier.
func (d *DB) conn() conn { return conn{ex: d.sql, driver: d.driver, slow: d.slow} }

// conn implements Querier.
func (t *Tx) conn() conn { return conn{ex: t.tx, driver: t.driver, slow: t.slow} }

// Exec runs a statement against q — a *DB or a *Tx.
func Exec(ctx context.Context, q Querier, query string, args ...any) (sql.Result, error) {
	c := q.conn()
	return exec(ctx, c.ex, c.driver, c.slow, query, args...)
}

// QueryAll runs the query against q and scans every row into []T via db tags.
func QueryAll[T any](ctx context.Context, q Querier, query string, args ...any) ([]T, error) {
	c := q.conn()
	return queryAll[T](ctx, c.ex, c.driver, c.slow, query, args...)
}

// QueryOne scans the first row from q into *T; ErrNoRow when nothing matched.
func QueryOne[T any](ctx context.Context, q Querier, query string, args ...any) (*T, error) {
	c := q.conn()
	return queryOne[T](ctx, c.ex, c.driver, c.slow, query, args...)
}

// QueryScalar scans a single column of the first row from q.
func QueryScalar[T any](ctx context.Context, q Querier, query string, args ...any) (T, error) {
	c := q.conn()
	return queryScalar[T](ctx, c.ex, c.driver, c.slow, query, args...)
}

func exec(ctx context.Context, ex executor, driver Driver, slow time.Duration, query string, args ...any) (sql.Result, error) {
	q := rebind(driver, query)
	start := time.Now()
	res, err := ex.ExecContext(ctx, q, args...)
	logSlow(ctx, "exec", start, slow, q, err)
	return res, wrapErr(err)
}

func queryAll[T any](ctx context.Context, ex executor, driver Driver, slow time.Duration, query string, args ...any) ([]T, error) {
	q := rebind(driver, query)
	start := time.Now()
	rows, err := ex.QueryContext(ctx, q, args...)
	logSlow(ctx, "query", start, slow, q, err)
	if err != nil {
		return nil, wrapErr(err)
	}
	defer func() { _ = rows.Close() }()
	return scanAll[T](rows)
}

func queryOne[T any](ctx context.Context, ex executor, driver Driver, slow time.Duration, query string, args ...any) (*T, error) {
	q := rebind(driver, query)
	start := time.Now()
	rows, err := ex.QueryContext(ctx, q, args...)
	logSlow(ctx, "query", start, slow, q, err)
	if err != nil {
		return nil, wrapErr(err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, wrapErr(err)
		}
		return nil, ErrNoRow
	}
	cols, err := rows.Columns()
	if err != nil {
		return nil, wrapErr(err)
	}
	v, err := scanRow[T](rows, cols)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func queryScalar[T any](ctx context.Context, ex executor, driver Driver, slow time.Duration, query string, args ...any) (T, error) {
	var zero T
	q := rebind(driver, query)
	start := time.Now()
	rows, err := ex.QueryContext(ctx, q, args...)
	logSlow(ctx, "query", start, slow, q, err)
	if err != nil {
		return zero, wrapErr(err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return zero, wrapErr(err)
		}
		return zero, ErrNoRow
	}
	var v T
	if err := rows.Scan(&v); err != nil {
		return zero, wrapErr(err)
	}
	return v, nil
}

func withTx(ctx context.Context, sqlDB *sql.DB, driver Driver, slow time.Duration, fn func(ctx context.Context, tx *Tx) error) error {
	raw, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return wrapErr(err)
	}
	tx := &Tx{tx: raw, driver: driver, slow: slow}
	committed := false
	defer func() {
		if !committed {
			_ = raw.Rollback()
		}
	}()
	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := raw.Commit(); err != nil {
		return wrapErr(err)
	}
	committed = true
	return nil
}

func logSlow(ctx context.Context, op string, start time.Time, slow time.Duration, query string, err error) {
	if slow <= 0 {
		return
	}
	if d := time.Since(start); d > slow {
		slog.WarnContext(ctx, "db slow query",
			"op", op, "dur_ms", d.Milliseconds(), "sql", truncate(query, 200), "err", err)
	}
}

// wrapErr turns driver errors into errs kinds (timeout / internal).
func wrapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errs.Wrap(errs.KindTimeout, 50400, err, "db timeout")
	}
	return errs.InternalWrap(50000, err, "db error")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
