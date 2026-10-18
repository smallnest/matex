package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/smallnest/matex/pkg/core/dbtest"
	"github.com/smallnest/matex/pkg/core/errs"
)

// A unique-constraint violation must surface as KindConflict (409), not a
// generic 500: business code cannot inspect driver errors (depguard).

func TestUniqueViolationIsConflict(t *testing.T) {
	d := dbtest.Start(t)
	ctx := t.Context()
	if _, err := d.Exec(ctx, `CREATE TABLE users (id INTEGER PRIMARY KEY, name VARCHAR(64) NOT NULL UNIQUE)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO users (id, name) VALUES (?, ?)`, 1, "ann"); err != nil {
		t.Fatal(err)
	}
	_, err := d.Exec(ctx, `INSERT INTO users (id, name) VALUES (?, ?)`, 2, "ann")
	if err == nil {
		t.Fatal("duplicate name was accepted")
	}
	if got := errs.KindOf(err); got != errs.KindConflict {
		t.Fatalf("KindOf = %v, want KindConflict (%v)", got, err)
	}
	if status, _, _ := errs.Status(err); status != 409 {
		t.Fatalf("status = %d, want 409", status)
	}

	// A non-unique constraint (NOT NULL) stays an internal error.
	if _, err := d.Exec(ctx, `INSERT INTO users (id, name) VALUES (?, NULL)`, 3); err == nil {
		t.Fatal("NULL name was accepted")
	} else if got := errs.KindOf(err); got != errs.KindInternal {
		t.Fatalf("NOT NULL violation KindOf = %v, want KindInternal", got)
	}
}

func TestCancelledQueryIsUnavailable(t *testing.T) {
	d := dbtest.Start(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := d.Exec(ctx, `SELECT 1`)
	if err == nil {
		t.Skip("driver ignored the cancelled context")
	}
	if got := errs.KindOf(err); got != errs.KindUnavailable && got != errs.KindInternal {
		t.Fatalf("KindOf = %v", got)
	}
	if errors.Is(err, context.Canceled) && errs.KindOf(err) != errs.KindUnavailable {
		t.Fatalf("cancellation flattened to %v, want KindUnavailable", errs.KindOf(err))
	}
}
