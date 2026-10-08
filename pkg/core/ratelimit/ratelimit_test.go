package ratelimit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/smallnest/matex/pkg/core/httpx"
	"github.com/smallnest/matex/pkg/core/redistest"
)

// newClock returns a manually advanced clock plus its current time.
func newClock() (*time.Time, func() time.Time) {
	cur := time.Unix(0, 0)
	return &cur, func() time.Time { return cur }
}

func TestLocalBurstThenRefill(t *testing.T) {
	cur, clock := newClock()
	l := NewLocal(10, 2) // 10 tokens/s, burst 2
	l.now = clock
	l.lastGC = *cur

	for i := 1; i <= 2; i++ {
		if ok, err := l.Allow(context.Background(), "k"); err != nil || !ok {
			t.Fatalf("burst request %d: ok=%v err=%v", i, ok, err)
		}
	}
	if ok, _ := l.Allow(context.Background(), "k"); ok {
		t.Fatal("third request allowed; burst is 2")
	}

	// 10 tokens/s means one token per 100ms.
	*cur = cur.Add(100 * time.Millisecond)
	if ok, _ := l.Allow(context.Background(), "k"); !ok {
		t.Fatal("token did not refill after the refill interval")
	}
	if ok, _ := l.Allow(context.Background(), "k"); ok {
		t.Fatal("more than one token was refilled")
	}
}

func TestLocalRefillIsCappedAtBurst(t *testing.T) {
	cur, clock := newClock()
	l := NewLocal(10, 2)
	l.now = clock
	l.lastGC = *cur

	localAllow(t, l, "k")
	localAllow(t, l, "k")

	// A long idle period must not bank more than `burst` tokens.
	*cur = cur.Add(time.Hour)
	allowed := 0
	for i := 0; i < 5; i++ {
		if ok, _ := l.Allow(context.Background(), "k"); ok {
			allowed++
		}
	}
	if allowed != 2 {
		t.Fatalf("allowed %d requests after a long idle, want the burst of 2", allowed)
	}
}

func TestLocalKeysAreIndependent(t *testing.T) {
	cur, clock := newClock()
	l := NewLocal(1, 1)
	l.now = clock
	l.lastGC = *cur

	localAllow(t, l, "alice")
	if ok, _ := l.Allow(context.Background(), "alice"); ok {
		t.Fatal("alice got a second request")
	}
	// bob must be unaffected by alice's exhausted bucket.
	if ok, _ := l.Allow(context.Background(), "bob"); !ok {
		t.Fatal("bob is limited by alice's usage")
	}
}

func TestLocalForgetsIdleKeys(t *testing.T) {
	cur, clock := newClock()
	l := NewLocal(10, 2)
	l.now = clock
	l.lastGC = *cur

	localAllow(t, l, "early")
	if got := l.Keys(); got != 1 {
		t.Fatalf("tracked %d keys, want 1", got)
	}

	*cur = cur.Add(2 * time.Minute) // past the idle cutoff
	localAllow(t, l, "late")

	if got := l.Keys(); got != 1 {
		t.Fatalf("tracked %d keys after GC, want only the fresh one", got)
	}
}

func TestLocalEmptyKeyIsNotLimited(t *testing.T) {
	l := NewLocal(1, 1)
	for i := 0; i < 10; i++ {
		if ok, _ := l.Allow(context.Background(), ""); !ok {
			t.Fatal("an empty key must not be rate limited")
		}
	}
}

func TestNewLocalRejectsBadConfig(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rate  float64
		burst int
	}{
		{"zero rate", 0, 1},
		{"negative rate", -1, 1},
		{"zero burst", 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			NewLocal(tc.rate, tc.burst)
		})
	}
}

func TestRedisFixedWindow(t *testing.T) {
	rc := redistest.Start(t)
	l := NewRedis(rc, 3, time.Second)
	ctx := context.Background()

	for i := 1; i <= 3; i++ {
		ok, err := l.Allow(ctx, "user-1")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if !ok {
			t.Fatalf("request %d denied before the limit of 3", i)
		}
	}
	if ok, err := l.Allow(ctx, "user-1"); err != nil || ok {
		t.Fatalf("fourth request: ok=%v err=%v (limit is 3)", ok, err)
	}

	// A separate key has its own window.
	if ok, err := l.Allow(ctx, "user-2"); err != nil || !ok {
		t.Fatalf("other key: ok=%v err=%v", ok, err)
	}

	// The window counter must carry a TTL, or it would never reset.
	ttl, err := rc.Cmdable().TTL(ctx, "ratelimit:user-1").Result()
	if err != nil {
		t.Fatalf("ttl: %v", err)
	}
	if ttl <= 0 {
		t.Fatalf("window counter ttl = %v, want a positive expiry", ttl)
	}
}

func TestNewRedisRejectsBadConfig(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for a nil client")
		}
	}()
	NewRedis(nil, 1, time.Second)
}

func TestMiddlewareRejectsOverLimit(t *testing.T) {
	cur, clock := newClock()
	l := NewLocal(1, 1)
	l.now = clock
	l.lastGC = *cur

	calls := 0
	srv := httpx.New(httpx.Config{Timeout: time.Second})
	srv.Use(Middleware(l, ByHeader("X-API-Key")))
	srv.Handle("GET", "/x", func(context.Context, *http.Request) (any, error) {
		calls++
		return "ok", nil
	})

	do := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("X-API-Key", "k1")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	if rec := do(); rec.Code != http.StatusOK {
		t.Fatalf("first request: code %d", rec.Code)
	}
	rec := do()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: code %d body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"code":42901`) {
		t.Fatalf("body %s missing the business code", rec.Body.String())
	}
	if calls != 1 {
		t.Fatalf("handler ran %d times; a limited request must not reach it", calls)
	}
}

// failLimiter stands in for a broken backing store.
type failLimiter struct{}

func (failLimiter) Allow(context.Context, string) (bool, error) {
	return false, errors.New("store is down")
}

// TestMiddlewareFailsOpen: a broken limiter must not reject traffic. Losing
// the rate limit is bad; losing the service is worse.
func TestMiddlewareFailsOpen(t *testing.T) {
	srv := httpx.New(httpx.Config{Timeout: time.Second})
	srv.Use(Middleware(failLimiter{}, ByIP))
	srv.Handle("GET", "/x", func(context.Context, *http.Request) (any, error) { return "ok", nil })

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d, want the request to pass through", rec.Code)
	}
}

func TestKeyFuncs(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/orders/1", nil)
	req.RemoteAddr = "10.1.2.3:5555"
	req.Pattern = "GET /api/v1/orders/{id}"
	req.Header.Set("X-API-Key", "k1")

	if got := ByIP(req); got != "10.1.2.3" {
		t.Errorf("ByIP = %q", got)
	}
	if got := ByHeader("X-API-Key")(req); got != "k1" {
		t.Errorf("ByHeader = %q", got)
	}
	if got := ByRoute(req); got != "GET /api/v1/orders/{id}" {
		t.Errorf("ByRoute = %q", got)
	}
	if got := ByIPAndRoute(req); got != "10.1.2.3|GET /api/v1/orders/{id}" {
		t.Errorf("ByIPAndRoute = %q", got)
	}
	// A header the client did not send yields "" and therefore no limiting.
	if got := ByHeader("X-Missing")(req); got != "" {
		t.Errorf("ByHeader = %q, want empty", got)
	}
}

func localAllow(t *testing.T, l *Local, key string) {
	t.Helper()
	ok, err := l.Allow(context.Background(), key)
	if err != nil || !ok {
		t.Fatalf("Allow(%q) = %v, %v", key, ok, err)
	}
}
