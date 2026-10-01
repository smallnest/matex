// Package helloworld is the example domain. It demonstrates the full
// matex stack: a handler (route) → service (business) → dao (db) plus
// optional redis/memcache caching and a kafka event.
//
// Copy this package to add a new domain (see the add-domain skill).
package helloworld

import (
	"context"

	"github.com/smallnest/matex/pkg/core/db"
)

// GreetStat is the DAO row (db tags match column names).
type GreetStat struct {
	Name  string `json:"name" db:"name"`
	Count int64  `json:"count" db:"count"`
}

// DAO groups data access for the domain.
type DAO struct {
	db *db.DB
}

// NewDAO creates a DAO. db may be nil when no database is configured;
// methods short-circuit in that case.
func NewDAO(d *db.DB) *DAO { return &DAO{db: d} }

// IncrementGreet bumps a per-name counter and returns its new value.
//
// The SQL is portable across postgres/mysql/sqlite: it uses "?" placeholders
// (rewritten to $1..$n for postgres) and an UPDATE-then-INSERT instead of a
// driver-specific upsert.
func (d *DAO) IncrementGreet(ctx context.Context, name string) (int64, error) {
	res, err := d.db.Exec(ctx, `UPDATE greet_stats SET count = count + 1 WHERE name = ?`, name)
	if err != nil {
		return 0, err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		if _, err := d.db.Exec(ctx, `INSERT INTO greet_stats (name, count) VALUES (?, 1)`, name); err != nil {
			return 0, err
		}
	}
	return d.db.QueryScalar[int64](ctx, `SELECT count FROM greet_stats WHERE name = ?`, name)
}

// Stat reads a counter (db.ErrNoRow when unknown).
func (d *DAO) Stat(ctx context.Context, name string) (*GreetStat, error) {
	return d.db.QueryOne[GreetStat](ctx,
		`SELECT name, count FROM greet_stats WHERE name = ?`, name)
}
