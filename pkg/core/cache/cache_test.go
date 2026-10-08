package cache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/smallnest/matex/pkg/core/redistest"
)

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition never became true")
}

func TestMissThenHit(t *testing.T) {
	rc := redistest.Start(t)
	ctx := context.Background()
	var loads atomic.Int32

	load := func(context.Context) (string, error) {
		loads.Add(1)
		return "alice", nil
	}

	for i := 1; i <= 2; i++ {
		v, err := GetOrLoad(ctx, rc, "user:1", Config{TTL: time.Minute}, load)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if v != "alice" {
			t.Fatalf("call %d: value %q", i, v)
		}
	}
	if got := loads.Load(); got != 1 {
		t.Fatalf("loaded %d times; the second call should have hit the cache", got)
	}
}

func TestLoadFailureIsNotCached(t *testing.T) {
	rc := redistest.Start(t)
	ctx := context.Background()
	boom := errors.New("source is down")
	var loads atomic.Int32

	load := func(context.Context) (string, error) {
		loads.Add(1)
		return "", boom
	}

	for i := 1; i <= 2; i++ {
		if _, err := GetOrLoad(ctx, rc, "user:1", Config{TTL: time.Minute}, load); !errors.Is(err, boom) {
			t.Fatalf("call %d: err = %v, want the loader's error", i, err)
		}
	}
	if got := loads.Load(); got != 2 {
		t.Fatalf("loaded %d times; a failed load must not be cached", got)
	}
}

// TestConcurrentMissesLoadOnce is the stampede case: a hot key expires and
// every in-flight request tries to reload it.
func TestConcurrentMissesLoadOnce(t *testing.T) {
	rc := redistest.Start(t)
	const n = 8

	var (
		loads       atomic.Int32
		attempted   atomic.Int32
		startedOnce sync.Once
	)
	started := make(chan struct{})
	release := make(chan struct{})

	load := func(context.Context) (int, error) {
		startedOnce.Do(func() { close(started) })
		loads.Add(1)
		<-release
		return 42, nil
	}

	var wg sync.WaitGroup
	values := make([]int, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			attempted.Add(1)
			values[i], errs[i] = GetOrLoad(t.Context(), rc, "hot", Config{TTL: time.Minute}, load)
		}(i)
	}

	<-started
	waitFor(t, func() bool { return attempted.Load() == n })
	close(release)
	wg.Wait()

	for i := range n {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if values[i] != 42 {
			t.Fatalf("goroutine %d: value %d", i, values[i])
		}
	}
	if got := loads.Load(); got != 1 {
		t.Fatalf("loaded %d times for %d concurrent misses", got, n)
	}
}

func TestDifferentKeysLoadConcurrently(t *testing.T) {
	rc := redistest.Start(t)
	ctx := context.Background()
	release := make(chan struct{})
	var loads atomic.Int32

	load := func(context.Context) (string, error) {
		loads.Add(1)
		<-release
		return "v", nil
	}

	done := make(chan error, 2)
	for _, key := range []string{"a", "b"} {
		go func(key string) {
			_, err := GetOrLoad(ctx, rc, key, Config{TTL: time.Minute}, load)
			done <- err
		}(key)
	}

	waitFor(t, func() bool { return loads.Load() == 2 })
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatalf("key %d: %v", i, err)
		}
	}
	if got := loads.Load(); got != 2 {
		t.Fatalf("loaded %d times, want one per key", got)
	}
}

func TestNilClientLoadsUncached(t *testing.T) {
	ctx := context.Background()
	var loads atomic.Int32
	load := func(context.Context) (int, error) {
		loads.Add(1)
		return 7, nil
	}

	for i := 0; i < 3; i++ {
		v, err := GetOrLoad(ctx, nil, "k", Config{TTL: time.Minute}, load)
		if err != nil || v != 7 {
			t.Fatalf("call %d: v=%d err=%v", i, v, err)
		}
	}
	if got := loads.Load(); got != 3 {
		t.Fatalf("loaded %d times; without a client every call must load", got)
	}
}

func TestInvalidateForcesReload(t *testing.T) {
	rc := redistest.Start(t)
	ctx := context.Background()
	var loads atomic.Int32
	load := func(context.Context) (string, error) {
		loads.Add(1)
		return "v", nil
	}
	const key = "thing:1"

	for i := 0; i < 2; i++ {
		if _, err := GetOrLoad(ctx, rc, key, Config{TTL: time.Minute}, load); err != nil {
			t.Fatal(err)
		}
	}
	if got := loads.Load(); got != 1 {
		t.Fatalf("loaded %d times before invalidation", got)
	}

	if err := Invalidate(ctx, rc, key); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	if _, err := GetOrLoad(ctx, rc, key, Config{TTL: time.Minute}, load); err != nil {
		t.Fatal(err)
	}
	if got := loads.Load(); got != 2 {
		t.Fatalf("loaded %d times; invalidation did not force a reload", got)
	}
}

func TestInvalidateIsSafeWithoutClient(t *testing.T) {
	if err := Invalidate(context.Background(), nil, "a", "b"); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestJitterShortensTheTTLWithinTheWindow(t *testing.T) {
	const (
		ttl    = time.Minute
		jitter = 10 * time.Second
	)
	cfg := Config{TTL: ttl, Jitter: jitter}

	seen := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		got := cfg.ttl()
		if got > ttl || got < ttl-jitter {
			t.Fatalf("ttl = %v, want within [%v, %v]", got, ttl-jitter, ttl)
		}
		seen[got] = true
	}
	if len(seen) < 2 {
		t.Fatal("jitter produced one value; it is not spreading expiries")
	}
}

// TestJitterNeverZeroesTheTTL: a jitter larger than the TTL must not produce
// a zero expiry, which redis reads as "delete immediately".
func TestJitterNeverZeroesTheTTL(t *testing.T) {
	cfg := Config{TTL: time.Second, Jitter: time.Hour}
	for i := 0; i < 200; i++ {
		got := cfg.ttl()
		if got < 500*time.Millisecond || got > time.Second {
			t.Fatalf("ttl = %v, want within [500ms, 1s]", got)
		}
	}
}

func TestZeroConfigUsesTheDefaultTTL(t *testing.T) {
	if got := (Config{}).ttl(); got != DefaultTTL {
		t.Fatalf("ttl = %v, want %v", got, DefaultTTL)
	}
}

func TestKey(t *testing.T) {
	cases := []struct {
		parts []any
		want  string
	}{
		{[]any{"user", 42}, "user:42"},
		{[]any{"a", "b", "c"}, "a:b:c"},
		{[]any{"solo"}, "solo"},
		{[]any{"order", int64(9), "items"}, "order:9:items"},
	}
	for _, c := range cases {
		if got := Key(c.parts...); got != c.want {
			t.Errorf("Key(%v) = %q, want %q", c.parts, got, c.want)
		}
	}
}
