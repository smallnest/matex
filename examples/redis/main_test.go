package main

import (
	"errors"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/smallnest/matex/pkg/core/redis"
	"github.com/smallnest/matex/pkg/core/redistest"
)

// redistest.Start gives a real redis.Client backed by an in-process
// miniredis (or a real server via REDIS_TEST_ADDR), so these tests need no
// Docker.
func newTestService(t *testing.T) *redisService {
	t.Helper()
	return &redisService{
		cfg: redisConfig{Prefix: "t:", TTL: time.Minute, LockTTL: time.Minute, WaitTimeout: 2 * time.Second, Watchdog: time.Second, Hold: time.Millisecond},
		rc:  redistest.Start(t),
	}
}

// twoClients is for contention tests: redistest.Start would hand out two
// different miniredis servers, so both clients must share one Addr.
func twoClients(t *testing.T) (first, second *redis.Client) {
	t.Helper()
	addr := redistest.Addr(t)
	open := func() *redis.Client {
		g := goredis.NewClient(&goredis.Options{Addr: addr})
		t.Cleanup(func() { _ = g.Close() })
		return redis.New(g)
	}
	return open(), open()
}

func TestCacheAside(t *testing.T) {
	svc := newTestService(t)
	ctx := t.Context()

	v1, cached1, err := svc.cachedValue(ctx, "k")
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if cached1 {
		t.Error("first read must miss the cache")
	}
	if v1 != "computed:k" {
		t.Errorf("value = %q", v1)
	}

	v2, cached2, err := svc.cachedValue(ctx, "k")
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if !cached2 {
		t.Error("second read must hit the cache")
	}
	if v2 != v1 {
		t.Errorf("cached value = %q, want %q", v2, v1)
	}
}

func TestCacheDelete(t *testing.T) {
	svc := newTestService(t)
	ctx := t.Context()

	if _, _, err := svc.cachedValue(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if err := svc.rc.Del(ctx, svc.cfg.Prefix+"k"); err != nil {
		t.Fatal(err)
	}
	if _, cached, err := svc.cachedValue(ctx, "k"); err != nil || cached {
		t.Fatalf("after delete: cached=%v err=%v", cached, err)
	}
}

func TestTryLockIsExclusive(t *testing.T) {
	svc := newTestService(t)
	ctx := t.Context()
	key := svc.cfg.Prefix + "lock:x"

	release, ok, err := svc.rc.TryLock(ctx, key, time.Minute)
	if err != nil || !ok {
		t.Fatalf("first acquire: ok=%v err=%v", ok, err)
	}

	if _, ok2, err := svc.rc.TryLock(ctx, key, time.Minute); err != nil || ok2 {
		t.Fatalf("second acquire must fail while held: ok=%v err=%v", ok2, err)
	}

	if err := release(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
	release2, ok3, err := svc.rc.TryLock(ctx, key, time.Minute)
	if err != nil || !ok3 {
		t.Fatalf("after release: ok=%v err=%v", ok3, err)
	}
	if err := release2(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestLockReentrant(t *testing.T) {
	svc := newTestService(t)
	ctx := t.Context()
	key := svc.cfg.Prefix + "lock:r"

	a, err := svc.rc.Lock(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.rc.Lock(ctx, key) // same client nests instead of waiting
	if err != nil {
		t.Fatalf("reentrant acquire: %v", err)
	}
	if n := a.Held(); n != 2 {
		t.Fatalf("Held() = %d, want 2", n)
	}
	if err := a.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if n := svc.rc.Cmdable().Exists(ctx, "lock:"+key).Val(); n != 1 {
		t.Fatal("first release deleted a nested lock")
	}
	if err := b.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if n := svc.rc.Cmdable().Exists(ctx, "lock:"+key).Val(); n != 0 {
		t.Fatal("last release left the key behind")
	}
}

func TestLockBlocksUntilReleased(t *testing.T) {
	a, b := twoClients(t)
	ctx := t.Context()
	key := "t:lock:b"

	h, err := a.Lock(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release(ctx)

	acquired := make(chan struct{})
	go func() {
		l, err := b.Lock(ctx, key, redis.WithWaitTimeout(2*time.Second))
		if err != nil {
			t.Errorf("waiter acquire: %v", err)
			return
		}
		close(acquired)
		_ = l.Release(ctx)
	}()

	select {
	case <-acquired:
		t.Fatal("waiter acquired a held lock")
	case <-time.After(150 * time.Millisecond):
	}

	if err := h.Release(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never acquired after release")
	}
}

func TestLockRenewsLease(t *testing.T) {
	a, b := twoClients(t)
	ctx := t.Context()
	key := "t:lock:w"

	l, err := a.Lock(ctx, key, redis.WithLockTTL(300*time.Millisecond), redis.WithWatchdog(200*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release(ctx)

	// Hold well past the TTL: the watchdog re-arms the lease, so the key
	// outlives it and no outsider may slip in.
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if n := a.Cmdable().Exists(ctx, "lock:"+key).Val(); n != 1 {
			t.Fatal("lock key vanished while the watchdog was renewing")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := b.Lock(ctx, key, redis.WithWaitTimeout(50*time.Millisecond)); !errors.Is(err, redis.ErrLockBusy) {
		t.Fatalf("outsider acquired a renewed lock: %v", err)
	}
}
