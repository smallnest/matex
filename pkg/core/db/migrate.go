package db

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// ApplyMigrations applies every unapplied *.sql file in dir, in filename
// order, tracking state in a schema_migrations table. Statements are split
// on ";" — avoid multi-statement constructs (e.g. CREATE FUNCTION bodies)
// in a single file.
//
// Migration SQL should stay portable if you target more than one driver:
// prefer VARCHAR(n) over TEXT for keys (MySQL), and set timestamps from Go
// (or a driver-neutral type) rather than now()/CURRENT_TIMESTAMP.
func (d *DB) ApplyMigrations(ctx context.Context, dir string) error {
	const create = `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    VARCHAR(255) NOT NULL PRIMARY KEY,
		applied_at VARCHAR(64)  NOT NULL
	)`
	if _, err := d.Exec(ctx, create); err != nil {
		return fmt.Errorf("migrate: ensure table: %w", err)
	}
	if release, ok, err := d.lockMigrations(ctx); err != nil {
		return err
	} else if ok {
		defer release()
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("migrate: read dir %s: %w", dir, err)
	}
	files := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	slices.Sort(files)

	for _, name := range files {
		applied, err := d.QueryScalar[int64](ctx,
			`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, name)
		if err != nil {
			return fmt.Errorf("migrate: check %s: %w", name, err)
		}
		if applied > 0 {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("migrate: read %s: %w", name, err)
		}
		if err := d.applyFile(ctx, name, string(body)); err != nil {
			return err
		}
	}
	return nil
}

// lockMigrations serializes migration runs across replicas that boot at
// the same moment: without it both read "not applied" and both execute the
// DDL, and the loser fails to start. Postgres uses an advisory lock on the
// pool connection; pgx's stdlib is per-query, but migrations run on a
// single *sql.Conn, so the session that took the lock is the one released.
// MySQL and SQLite get no lock (SQLite is a test driver; MySQL needs a
// different mechanism) — the tracked-state check and DDL that tolerates
// IF NOT EXISTS still make the common case safe.
func (d *DB) lockMigrations(ctx context.Context) (release func(), ok bool, err error) {
	if d.driver != Postgres {
		return nil, false, nil
	}
	conn, err := d.sql.Conn(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("migrate: lock connection: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock(?)`, migrationLockKey); err != nil {
		_ = conn.Close()
		return nil, false, fmt.Errorf("migrate: advisory lock: %w", err)
	}
	return func() {
		// Unlock explicitly so a pooled connection is never reused holding
		// the lock; closing would also drop the session, but costs the conn.
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(uctx, `SELECT pg_advisory_unlock(?)`, migrationLockKey)
		_ = conn.Close()
	}, true, nil
}

// migrationLockKey is an arbitrary stable key for pg_advisory_lock.
const migrationLockKey = 0x6d61746578 // "matex"

func (d *DB) applyFile(ctx context.Context, name, body string) error {
	return d.WithTx(ctx, func(ctx context.Context, tx *Tx) error {
		for _, stmt := range splitSQL(body) {
			if _, err := tx.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("migrate: %s: apply statement: %w", name, err)
			}
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			name, time.Now().UTC().Format(time.RFC3339),
		); err != nil {
			return fmt.Errorf("migrate: %s: record: %w", name, err)
		}
		return nil
	})
}

// splitSQL splits a migration body into statements, dropping whitespace-only
// and comment-only fragments.
func splitSQL(body string) []string {
	var stmts []string
	for stmt := range strings.SplitSeq(body, ";") {
		var lines []string
		for line := range strings.SplitSeq(stmt, "\n") {
			t := strings.TrimSpace(line)
			if t == "" || strings.HasPrefix(t, "--") {
				continue
			}
			lines = append(lines, line)
		}
		if len(lines) == 0 {
			continue
		}
		stmts = append(stmts, strings.TrimSpace(strings.Join(lines, "\n")))
	}
	return stmts
}
