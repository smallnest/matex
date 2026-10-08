package main

import (
	"bytes"
	"testing"

	"github.com/smallnest/matex/pkg/core/dbtest"
)

// dbtest.Start gives a migrated database with no external dependency
// (pure-Go SQLite by default); set DB_TEST_DRIVER/DB_TEST_DSN to run the
// same test against a real server.
func TestRun(t *testing.T) {
	d := dbtest.Start(t)

	var out bytes.Buffer
	res, err := run(t.Context(), d, &out)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}

	if res.InitialCount != 1 {
		t.Errorf("initial count = %d, want 1", res.InitialCount)
	}
	if res.AfterCommit != 3 {
		t.Errorf("after commit = %d, want 3", res.AfterCommit)
	}
	if res.AfterRollback != 3 {
		t.Errorf("after rollback = %d, want 3 (tx-c must not persist)", res.AfterRollback)
	}
	if res.AfterDAO != 4 {
		t.Errorf("after DAO tx = %d, want 4 (tx-d committed through the DAO)", res.AfterDAO)
	}
	if res.First.Slug != "hello-matex" || res.First.Views != 1 || res.First.Published {
		t.Errorf("first row = %+v", res.First)
	}
	if res.First.Note != nil {
		t.Errorf("NULL note should scan to nil, got %q", *res.First.Note)
	}
	if res.First.UpdatedAt.IsZero() {
		t.Error("updated_at should scan into time.Time")
	}
	if len(res.All) != 3 || res.All[0].Slug != "hello-matex" {
		t.Errorf("all = %+v", res.All)
	}
}
