// Package ratelimit bounds how many requests a caller may make.
//
// Two strategies behind one interface:
//
//	NewLocal   per-key token bucket in this process — protects one instance
//	NewRedis   fixed window shared through redis — protects the fleet
//
// Wire it into the HTTP layer with the middleware:
//
//	srv.Use(ratelimit.Middleware(limiter, ratelimit.ByIP))
//	srv.Handle("POST", "/api/v1/login", login)
//
// A rejected request gets the usual envelope — 429 {"code":42901,…} — and
// the handler never runs.
package ratelimit

import (
	"context"
	"math"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/smallnest/matex/pkg/core/errs"
	"github.com/smallnest/matex/pkg/core/httpx"
	"github.com/smallnest/matex/pkg/core/obs"
	"github.com/smallnest/matex/pkg/core/redis"
)

// CodeRateLimited is the business code of a rejected request.
const CodeRateLimited = 42901

// Limiter decides whether one more request for key is allowed.
type Limiter interface {
	Allow(ctx context.Context, key string) (bool, error)
}

// KeyFunc derives the limiting key of a request. Returning "" means "do not
// limit this request".
type KeyFunc func(r *http.Request) string

// ---------------------------------------------------------------- local

// Local is a per-key token bucket: each key may burst up to `burst`
// requests at once and then refills at `rate` per second. A key that has
// been quiet long enough to refill completely is dropped from memory, so
// an unbounded key space (client IPs, say) cannot grow the map forever.
type Local struct {
	rate  float64
	burst float64
	idle  time.Duration
	now   func() time.Time // overridable in tests

	mu      sync.Mutex
	buckets map[string]*bucket
	lastGC  time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewLocal creates a token-bucket limiter. rate is tokens per second; burst
// is how many may accumulate. Both must be positive.
func NewLocal(rate float64, burst int) *Local {
	if rate <= 0 {
		panic("ratelimit: rate must be positive")
	}
	if burst <= 0 {
		panic("ratelimit: burst must be positive")
	}
	// A key sitting idle for a full refill (plus slack) is indistinguishable
	// from a fresh one, so forgetting it changes no decision.
	full := time.Duration(float64(time.Second) * float64(burst) / rate)
	idle := 2 * full
	if idle < time.Minute {
		idle = time.Minute
	}
	return &Local{
		rate:    rate,
		burst:   float64(burst),
		idle:    idle,
		now:     time.Now,
		buckets: make(map[string]*bucket),
		lastGC:  time.Now(),
	}
}

// Allow implements Limiter.
func (l *Local) Allow(_ context.Context, key string) (bool, error) {
	if key == "" {
		return true, nil
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	if now.Sub(l.lastGC) >= l.idle {
		for k, b := range l.buckets {
			if now.Sub(b.last) >= l.idle {
				delete(l.buckets, k)
			}
		}
		l.lastGC = now
	}

	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens < 1 {
		return false, nil
	}
	b.tokens--
	return true, nil
}

// Keys reports how many keys are currently tracked (for tests and metrics).
func (l *Local) Keys() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// ---------------------------------------------------------------- redis

// Redis counts requests in fixed windows shared by every instance, so the
// fleet enforces one limit instead of N × limit. The window starts with the
// first request and is not a sliding one: a caller can send up to 2×limit
// across a window boundary. That trade buys atomicity and one round trip.
type Redis struct {
	client *redis.Client
	limit  int
	window time.Duration
}

// NewRedis creates a shared fixed-window limiter: at most limit requests
// per window, per key.
func NewRedis(client *redis.Client, limit int, window time.Duration) *Redis {
	if client == nil {
		panic("ratelimit: redis client is nil")
	}
	if limit <= 0 {
		panic("ratelimit: limit must be positive")
	}
	if window <= 0 {
		panic("ratelimit: window must be positive")
	}
	return &Redis{client: client, limit: limit, window: window}
}

// Allow implements Limiter. A redis failure is returned as an error; the
// middleware decides what to do about it (it lets the request through).
func (r *Redis) Allow(ctx context.Context, key string) (bool, error) {
	if key == "" {
		return true, nil
	}
	n, err := r.incrWindow(ctx, key)
	if err != nil {
		return false, err
	}
	return n <= int64(r.limit), nil
}

func (r *Redis) incrWindow(ctx context.Context, key string) (int64, error) {
	return windowScript.RunInt(ctx, r.client, []string{"ratelimit:" + key},
		r.window.Milliseconds())
}

// windowScript increments the window counter and arms its TTL on the first
// request. Doing it in one script keeps the counter and the expiry from
// drifting apart under concurrency.
var windowScript = redis.NewScript(`local n = redis.call("incr", KEYS[1])
if n == 1 then
	redis.call("pexpire", KEYS[1], ARGV[1])
end
return n`)

// ---------------------------------------------------------------- middleware

// Middleware rejects requests over the limit with 429. When the limiter
// itself fails (redis down, say) the request is allowed through: losing the
// rate limit must not take the service down with it.
func Middleware(l Limiter, key KeyFunc) httpx.Middleware {
	return func(next httpx.HandlerFunc) httpx.HandlerFunc {
		return func(ctx context.Context, r *http.Request) (any, error) {
			k := key(r)
			if k == "" {
				return next(ctx, r)
			}
			ok, err := l.Allow(ctx, k)
			if err != nil {
				obs.Warn(ctx, "rate limiter unavailable; letting the request through", "err", err)
				return next(ctx, r)
			}
			if !ok {
				obs.Warn(ctx, "rate limited", "key", k, "path", r.URL.Path)
				return nil, errs.RateLimited(CodeRateLimited, "too many requests")
			}
			return next(ctx, r)
		}
	}
}

// ByIP limits per client address.
//
// It uses RemoteAddr, which is the proxy's address behind a load balancer.
// If your edge sets X-Forwarded-For and you trust it, combine explicitly:
//
//	ratelimit.ByHeader("X-Forwarded-For")
func ByIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ByHeader limits per header value — an API key, a tenant id.
func ByHeader(name string) KeyFunc {
	return func(r *http.Request) string { return r.Header.Get(name) }
}

// ByRoute limits per matched route pattern, ignoring who the caller is.
func ByRoute(r *http.Request) string { return r.Pattern }

// ByIPAndRoute combines the two into one key.
func ByIPAndRoute(r *http.Request) string { return ByIP(r) + "|" + r.Pattern }
