package redistest_test

import (
	"testing"
	"time"

	"github.com/smallnest/matex/pkg/core/redis"
	"github.com/smallnest/matex/pkg/core/redistest"
)

// redistest must hand back a working client, so every package that builds
// on redis can be tested offline.

func TestStartReturnsWorkingClient(t *testing.T) {
	rc := redistest.Start(t)
	ctx := t.Context()

	if err := rc.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if err := rc.SetJSON(ctx, "k", 1, time.Minute); err != nil {
		t.Fatalf("SetJSON: %v", err)
	}
	v, ok, err := redis.GetJSON[int](ctx, rc, "k")
	if err != nil || !ok || v != 1 {
		t.Fatalf("GetJSON = %d, %v, %v", v, ok, err)
	}
}

func TestAddrIsDialable(t *testing.T) {
	addr := redistest.Addr(t)
	if addr == "" {
		t.Fatal("Addr returned an empty address")
	}
	rc, err := redis.Open(redis.Config{Addr: addr, DialTimeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("open Addr(): %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	if err := rc.Ping(t.Context()); err != nil {
		t.Fatalf("ping Addr(): %v", err)
	}
}

func TestStartIsolatesTests(t *testing.T) {
	a := redistest.Start(t)
	if err := a.SetJSON(t.Context(), "shared", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	b := redistest.Start(t)
	if _, ok, _ := redis.GetJSON[int](t.Context(), b, "shared"); ok {
		t.Fatal("a second redistest.Start saw the first test's keys")
	}
}
