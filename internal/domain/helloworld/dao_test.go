package helloworld

import (
	"errors"
	"testing"

	"github.com/smallnest/matex/pkg/core/db"
	"github.com/smallnest/matex/pkg/core/dbtest"
)

func TestDAOCounter(t *testing.T) {
	pool := dbtest.Start(t)
	dao := NewDAO(pool)
	ctx := t.Context()

	c1, err := dao.IncrementGreet(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if c1 != 1 {
		t.Fatalf("first increment: %d", c1)
	}
	c2, err := dao.IncrementGreet(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if c2 != 2 {
		t.Fatalf("second increment: %d", c2)
	}

	st, err := dao.Stat(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if st.Count != 2 || st.Name != "alice" {
		t.Fatalf("stat: %+v", st)
	}

	if _, err := dao.Stat(ctx, "nobody"); !errors.Is(err, db.ErrNoRow) {
		t.Fatalf("expected ErrNoRow, got %v", err)
	}
}
