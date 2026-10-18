package redis_test

import (
	"errors"
	"testing"
	"time"

	"github.com/smallnest/matex/pkg/core/redis"
	"github.com/smallnest/matex/pkg/core/redistest"
)

// The redis helpers are the primitives cron, cache, idempotency and
// ratelimit build on, so their contract is pinned here against miniredis.

type user struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

func TestSetJSONGetJSONRoundTrip(t *testing.T) {
	rc := redistest.Start(t)
	ctx := t.Context()

	if err := rc.SetJSON(ctx, "user:1", user{ID: 1, Name: "ann"}, time.Minute); err != nil {
		t.Fatalf("SetJSON: %v", err)
	}
	got, ok, err := redis.GetJSON[user](ctx, rc, "user:1")
	if err != nil || !ok {
		t.Fatalf("GetJSON: ok=%v err=%v", ok, err)
	}
	if got != (user{ID: 1, Name: "ann"}) {
		t.Fatalf("got %+v", got)
	}
}

func TestGetJSONMissingKeyIsNotAnError(t *testing.T) {
	rc := redistest.Start(t)

	_, ok, err := redis.GetJSON[user](t.Context(), rc, "absent")
	if err != nil {
		t.Fatalf("missing key must not be an error, got %v", err)
	}
	if ok {
		t.Fatal("missing key reported as found")
	}
}

func TestGetJSONCorruptValueReportsError(t *testing.T) {
	rc := redistest.Start(t)
	ctx := t.Context()

	if _, err := rc.Cmdable().Set(ctx, "bad", "not json", 0).Result(); err != nil {
		t.Fatal(err)
	}
	_, ok, err := redis.GetJSON[user](ctx, rc, "bad")
	if err == nil || ok {
		t.Fatalf("corrupt value: ok=%v err=%v, want an error", ok, err)
	}
}

func TestDelIgnoresMissingKeysAndNoKeys(t *testing.T) {
	rc := redistest.Start(t)
	ctx := t.Context()

	if err := rc.SetJSON(ctx, "a", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := rc.Del(ctx, "a", "never-existed"); err != nil {
		t.Fatalf("Del: %v", err)
	}
	if err := rc.Del(ctx); err != nil {
		t.Fatalf("Del with no keys: %v", err)
	}
	if n := rc.Cmdable().Exists(ctx, "a").Val(); n != 0 {
		t.Fatal("key survived Del")
	}
}

func TestClaimHeldForItsTTL(t *testing.T) {
	rc := redistest.Start(t)
	ctx := t.Context()

	ok, err := rc.Claim(ctx, "tick", time.Minute)
	if err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}
	ok, err = rc.Claim(ctx, "tick", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("a claim must not be re-taken before its TTL expires")
	}
}

func TestClaimZeroTTLNeverExpires(t *testing.T) {
	rc := redistest.Start(t)
	ctx := t.Context()

	if ok, err := rc.Claim(ctx, "once", 0); err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}
	if ttl := rc.Cmdable().TTL(ctx, "once").Val(); ttl != -1 {
		t.Fatalf("ttl = %v, want -1 (no expiration) so one-shot jobs stay claimed", ttl)
	}
	if ok, _ := rc.Claim(ctx, "once", 0); ok {
		t.Fatal("a permanent claim must never be taken twice")
	}
}

func TestTryLockMutualExclusion(t *testing.T) {
	rc := redistest.Start(t)
	ctx := t.Context()

	release, ok, err := rc.TryLock(ctx, "lock", time.Minute)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	if _, ok, err := rc.TryLock(ctx, "lock", time.Minute); err != nil || ok {
		t.Fatalf("second acquire: ok=%v err=%v, want busy", ok, err)
	}

	if err := release(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
	if ok, _ := rc.Cmdable().Exists(ctx, "lock").Result(); ok != 0 {
		t.Fatal("release left the key behind")
	}
	if _, ok, _ := rc.TryLock(ctx, "lock", time.Minute); !ok {
		t.Fatal("lock must be acquirable again after release")
	}
}

func TestTryLockReleaseKeepsAnotherOwnersLock(t *testing.T) {
	rc := redistest.Start(t)
	ctx := t.Context()

	release, ok, err := rc.TryLock(ctx, "lock", time.Minute)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	// Simulate the lock expiring and being re-taken by someone else.
	if err := rc.Del(ctx, "lock"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := rc.TryLock(ctx, "lock", time.Minute); !ok {
		t.Fatal("re-acquire failed")
	}
	if err := release(ctx); err != nil {
		t.Fatal(err)
	}
	if n := rc.Cmdable().Exists(ctx, "lock").Val(); n != 1 {
		t.Fatal("a stale holder's release deleted the new owner's lock")
	}
}

func TestIncrByArmsTTLOnFirstIncrementOnly(t *testing.T) {
	rc := redistest.Start(t)
	ctx := t.Context()

	for want := int64(1); want <= 3; want++ {
		got, err := rc.IncrBy(ctx, "hits", 1, time.Minute)
		if err != nil || got != want {
			t.Fatalf("IncrBy #%d: got %d err=%v", want, got, err)
		}
	}
	// A TTL of exactly one minute proves the timer was not restarted.
	if ttl := rc.Cmdable().TTL(ctx, "hits").Val(); ttl != time.Minute {
		t.Fatalf("ttl = %v, want %v", ttl, time.Minute)
	}
}

func TestScriptRunInt(t *testing.T) {
	rc := redistest.Start(t)
	ctx := t.Context()
	s := redis.NewScript(`return tonumber(ARGV[1]) + tonumber(ARGV[2])`)

	n, err := s.RunInt(ctx, rc, nil, 2, 40)
	if err != nil || n != 42 {
		t.Fatalf("RunInt = %d, %v", n, err)
	}
}

func TestIsNotFound(t *testing.T) {
	rc := redistest.Start(t)

	if !redis.IsNotFound(rc.Cmdable().Get(t.Context(), "absent").Err()) {
		t.Fatal("a missing key must be reported by IsNotFound")
	}
	if redis.IsNotFound(errors.New("boom")) {
		t.Fatal("an unrelated error must not be a miss")
	}
}
