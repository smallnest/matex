// Package idempotency makes a write happen once even if the request arrives
// twice — a client retry, a double click, a redelivered webhook.
//
// The caller sends a unique Idempotency-Key; the first request runs and its
// outcome is stored, later ones with the same key get that stored outcome
// back instead of running again:
//
//	srv.Use(v.Middleware())
//	srv.Handle("POST", "/api/v1/payments", pay)
//
//	// then, with a redis-backed store:
//	srv.Use(idempotency.Middleware(idempotency.NewRedis(env.Redis), idempotency.Config{}))
//
// A second request that arrives while the first is still running is
// rejected with 409 rather than being run in parallel — that is the case
// the key exists to prevent.
//
// Server-side failures (5xx) release the key instead of being stored, so a
// retry can still succeed; client errors and successes are replayed.
package idempotency

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/smallnest/matex/pkg/core/errs"
	"github.com/smallnest/matex/pkg/core/httpx"
	"github.com/smallnest/matex/pkg/core/obs"
	"github.com/smallnest/matex/pkg/core/redis"
)

// Codes for the two ways a duplicate is answered.
const (
	CodeInProgress = 40902 // 409: the first request is still running
	CodeCorrupt    = 50002 // 500: a stored outcome could not be read back
)

// Config configures the middleware.
type Config struct {
	// Header carries the caller's key.
	Header string
	// TTL is how long an outcome is replayable.
	TTL time.Duration
	// Prefix namespaces the keys inside the store.
	Prefix string
	// Methods are the HTTP methods that are deduplicated. GET and friends
	// are already idempotent and carrying a key for them is noise.
	Methods []string
}

func (c Config) withDefaults() Config {
	if c.Header == "" {
		c.Header = "Idempotency-Key"
	}
	if c.TTL <= 0 {
		c.TTL = 24 * time.Hour
	}
	if c.Prefix == "" {
		c.Prefix = "idem:"
	}
	if c.Methods == nil {
		c.Methods = []string{http.MethodPost, http.MethodPatch, http.MethodDelete}
	}
	return c
}

func (c Config) covers(method string) bool {
	for _, m := range c.Methods {
		if m == method {
			return true
		}
	}
	return false
}

// Outcome is what the first request produced. It is serialized as-is, so it
// must stay JSON-friendly.
type Outcome struct {
	// Failed marks a stored error (4xx class) rather than a success.
	Failed bool            `json:"failed,omitempty"`
	Kind   int             `json:"kind,omitempty"`
	Code   int             `json:"code,omitempty"`
	Msg    string          `json:"msg,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
}

// Store keeps track of keys claimed for processing and their outcomes.
// Implementations: Memory and Redis.
type Store interface {
	// Begin claims key. It returns false when the key is already claimed —
	// either in flight or finished.
	Begin(ctx context.Context, key string, ttl time.Duration) (bool, error)
	// Load returns a finished outcome. found is false while the first
	// request is still running.
	Load(ctx context.Context, key string) (Outcome, bool, error)
	// Finish stores outcome, or releases the key when outcome is nil (used
	// for server-side failures, so a retry may still succeed).
	Finish(ctx context.Context, key string, outcome *Outcome, ttl time.Duration) error
}

// ---------------------------------------------------------------- middleware

// Middleware answers duplicate requests from the stored outcome.
//
// A store failure lets the request through unguarded: losing deduplication
// is bad, but refusing every write because the store hiccuped is worse.
func Middleware(store Store, cfg Config) httpx.Middleware {
	cfg = cfg.withDefaults()
	return func(next httpx.HandlerFunc) httpx.HandlerFunc {
		return func(ctx context.Context, r *http.Request) (any, error) {
			raw := r.Header.Get(cfg.Header)
			if raw == "" || !cfg.covers(r.Method) {
				return next(ctx, r)
			}
			key := cfg.Prefix + raw

			claimed, err := store.Begin(ctx, key, cfg.TTL)
			if err != nil {
				obs.Warn(ctx, "idempotency store unavailable; running unguarded", "err", err)
				return next(ctx, r)
			}
			if !claimed {
				return replay(ctx, store, key, cfg.Header)
			}

			data, err := next(ctx, r)
			outcome := classify(data, err)
			if ferr := store.Finish(ctx, key, outcome, cfg.TTL); ferr != nil {
				obs.Warn(ctx, "storing the idempotent outcome failed", "err", ferr)
			}
			return data, err
		}
	}
}

// replay answers a duplicate request.
func replay(ctx context.Context, store Store, key, header string) (any, error) {
	outcome, found, err := store.Load(ctx, key)
	if err != nil {
		// The store let us claim the key but cannot tell us the outcome;
		// fail closed here, because running the work twice is exactly what
		// the caller asked us to prevent.
		obs.Error(ctx, "idempotency store failed on read; refusing the duplicate", "err", err)
		return nil, errs.Internal(CodeCorrupt, "idempotency state is unavailable")
	}
	if !found {
		return nil, errs.Conflict(CodeInProgress, "a request with this %s is still in progress", header)
	}
	return outcome.value()
}

// value turns a stored outcome back into what the envelope expects.
func (o Outcome) value() (any, error) {
	if o.Failed {
		return nil, errs.New(errs.Kind(o.Kind), o.Code, o.Msg)
	}
	if len(o.Data) == 0 {
		return nil, nil // the first request answered 204
	}
	var v any
	if err := json.Unmarshal(o.Data, &v); err != nil {
		return nil, errs.Internal(CodeCorrupt, "stored idempotent result is corrupt")
	}
	return v, nil
}

// classify decides what to remember. A 5xx is our own failure, not a
// property of the request, so the key is released instead of being stored:
// otherwise one bad moment would be replayed to the caller for the whole TTL.
func classify(data any, err error) *Outcome {
	if err != nil {
		status, code, msg := errs.Status(err)
		if status >= 500 {
			return nil
		}
		return &Outcome{Failed: true, Kind: int(errs.KindOf(err)), Code: code, Msg: msg}
	}
	b, mErr := json.Marshal(data)
	if mErr != nil {
		// Unserializable success: replaying it is impossible, so do not
		// pretend the key is protected.
		return nil
	}
	if string(b) == "null" {
		return &Outcome{}
	}
	return &Outcome{Data: b}
}

// ---------------------------------------------------------------- memory store

type entry struct {
	outcome Outcome
	done    bool
	expires time.Time
}

// Memory is a process-local store: correct for a single instance, useless
// across a fleet (each instance would deduplicate only its own traffic).
// Reach for it in tests and single-replica deployments.
type Memory struct {
	now func() time.Time

	mu    sync.Mutex
	items map[string]entry
}

// NewMemory creates an in-process store.
func NewMemory() *Memory {
	return &Memory{now: time.Now, items: make(map[string]entry)}
}

// Begin implements Store.
func (m *Memory) Begin(_ context.Context, key string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.evictLocked()
	if e, ok := m.items[key]; ok && m.now().Before(e.expires) {
		return false, nil
	}
	m.items[key] = entry{expires: m.now().Add(ttl)}
	return true, nil
}

// Load implements Store.
func (m *Memory) Load(_ context.Context, key string) (Outcome, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.items[key]
	if !ok || !e.done || !m.now().Before(e.expires) {
		return Outcome{}, false, nil
	}
	return e.outcome, true, nil
}

// Finish implements Store.
func (m *Memory) Finish(_ context.Context, key string, outcome *Outcome, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if outcome == nil {
		delete(m.items, key)
		return nil
	}
	m.items[key] = entry{outcome: *outcome, done: true, expires: m.now().Add(ttl)}
	return nil
}

// Len reports how many keys are held (for tests and metrics).
func (m *Memory) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.items)
}

func (m *Memory) evictLocked() {
	now := m.now()
	for k, e := range m.items {
		if !now.Before(e.expires) {
			delete(m.items, k)
		}
	}
}

// ---------------------------------------------------------------- redis store

// Redis stores outcomes in redis, so every instance of the service shares
// one view of which keys are taken.
type Redis struct {
	client *redis.Client
}

// NewRedis creates a redis-backed store.
func NewRedis(client *redis.Client) *Redis {
	if client == nil {
		panic("idempotency: redis client is nil")
	}
	return &Redis{client: client}
}

type record struct {
	Done    bool     `json:"done"`
	Outcome *Outcome `json:"outcome,omitempty"`
}

var inFlight = []byte(`{"done":false}`)

// Begin implements Store with SET NX: exactly one caller wins the key.
func (r *Redis) Begin(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	return r.client.Cmdable().SetNX(ctx, key, inFlight, ttl).Result()
}

// Load implements Store.
func (r *Redis) Load(ctx context.Context, key string) (Outcome, bool, error) {
	b, err := r.client.Cmdable().Get(ctx, key).Bytes()
	if err != nil {
		if redis.IsNotFound(err) {
			return Outcome{}, false, nil
		}
		return Outcome{}, false, err
	}
	var rec record
	if err := json.Unmarshal(b, &rec); err != nil {
		return Outcome{}, false, err
	}
	if !rec.Done || rec.Outcome == nil {
		return Outcome{}, false, nil
	}
	return *rec.Outcome, true, nil
}

// Finish implements Store.
func (r *Redis) Finish(ctx context.Context, key string, outcome *Outcome, ttl time.Duration) error {
	if outcome == nil {
		return r.client.Del(ctx, key)
	}
	b, err := json.Marshal(record{Done: true, Outcome: outcome})
	if err != nil {
		return err
	}
	return r.client.Cmdable().Set(ctx, key, b, ttl).Err()
}
