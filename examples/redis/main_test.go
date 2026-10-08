package main

import (
	"testing"
	"time"

	"github.com/smallnest/matex/pkg/core/redistest"
)

// redistest.Start gives a real redis.Client backed by an in-process
// miniredis (or a real server via REDIS_TEST_ADDR), so these tests need no
// Docker.
func newTestService(t *testing.T) *redisService {
	t.Helper()
	return &redisService{
		cfg: redisConfig{Prefix: "t:", TTL: time.Minute, LockTTL: time.Minute, Hold: time.Millisecond},
		rc:  redistest.Start(t),
	}
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
