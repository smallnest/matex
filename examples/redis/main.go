// Command redis demonstrates pkg/core/redis: cache-aside with JSON values,
// a blocking reentrant Lock, and the one-shot TryLock.
//
// Redis must be running (make dev). A configured `redis:` section is
// created by verticle.Run and read in Setup with the comma-ok accessor
// env.Redis().
//
// Run:
//
//	make dev
//	go run ./examples/redis
//
//	curl -i localhost:8080/api/v1/cache/world          # 第一次 miss，第二次 hit
//	curl -i localhost:8080/api/v1/cache/world
//	curl -i -X DELETE localhost:8080/api/v1/cache/world
//	curl -i -X POST localhost:8080/api/v1/lock/job1     # 阻塞等待 + 重入 + 续租
//	curl -i -X POST localhost:8080/api/v1/trylock/job1  # 一次尝试，忙则 409
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/smallnest/matex/pkg/core/errs"
	"github.com/smallnest/matex/pkg/core/httpx"
	"github.com/smallnest/matex/pkg/core/obs"
	"github.com/smallnest/matex/pkg/core/redis"
	"github.com/smallnest/matex/pkg/core/verticle"
)

type redisConfig struct {
	Prefix      string        `json:"prefix" default:"example:"`
	TTL         time.Duration `json:"ttl" default:"30s"`
	LockTTL     time.Duration `json:"lock_ttl" default:"5s"`
	WaitTimeout time.Duration `json:"wait_timeout" default:"2s"`
	Watchdog    time.Duration `json:"watchdog" default:"1s"`
	Hold        time.Duration `json:"hold" default:"100ms"`
}

type redisService struct {
	cfg redisConfig
	rc  *redis.Client
}

func (s *redisService) Name() string { return "rediscache" }

func (s *redisService) Setup(_ context.Context, env *verticle.Env) error {
	if err := env.DecodeService(&s.cfg); err != nil {
		return err
	}
	rc, ok := env.Redis()
	if !ok {
		return errors.New("rediscache: the `redis:` section is required (start Redis first: make dev)")
	}
	s.rc = rc
	return nil
}

func (s *redisService) BuildRouter(srv *httpx.Server) error {
	srv.Handle("GET", "/api/v1/cache/{key}", s.get)
	srv.Handle("DELETE", "/api/v1/cache/{key}", s.delete)
	srv.Handle("POST", "/api/v1/lock/{name}", s.lock)
	srv.Handle("POST", "/api/v1/trylock/{name}", s.tryLock)
	return nil
}

// entry is what we cache. JSON keeps values inspectable (redis-cli GET).
type entry struct {
	Value      string    `json:"value"`
	ComputedAt time.Time `json:"computed_at"`
}

// cachedValue is the classic cache-aside read-through: try the cache,
// compute on a miss, write it back with a TTL.
func (s *redisService) cachedValue(ctx context.Context, key string) (value string, cached bool, err error) {
	full := s.cfg.Prefix + key
	e, ok, err := redis.GetJSON[entry](ctx, s.rc, full)
	if err != nil {
		return "", false, err
	}
	if ok {
		return e.Value, true, nil
	}

	e = entry{Value: "computed:" + key, ComputedAt: time.Now().UTC()}
	if err := s.rc.SetJSON(ctx, full, e, s.cfg.TTL); err != nil {
		return "", false, err
	}
	return e.Value, false, nil
}

func (s *redisService) get(ctx context.Context, r *http.Request) (any, error) {
	key := r.PathValue("key")
	if key == "" {
		return nil, errs.Invalid(40001, "key is required")
	}
	v, cached, err := s.cachedValue(ctx, key)
	if err != nil {
		return nil, err
	}
	obs.Info(ctx, "cache read", "key", key, "cached", cached)
	return map[string]any{"key": key, "value": v, "cached": cached}, nil
}

func (s *redisService) delete(ctx context.Context, r *http.Request) (any, error) {
	key := r.PathValue("key")
	if err := s.rc.Del(ctx, s.cfg.Prefix+key); err != nil {
		return nil, err
	}
	return map[string]any{"key": key, "deleted": true}, nil
}

// lock runs a short critical section under a blocking Lock: it waits up to
// WaitTimeout for the key, re-arms its lease with a watchdog, and counts a
// nested acquire to show reentrancy in the response body.
func (s *redisService) lock(ctx context.Context, r *http.Request) (any, error) {
	name := r.PathValue("name")
	if name == "" {
		return nil, errs.Invalid(40001, "name is required")
	}
	key := s.cfg.Prefix + "lock:" + name

	l, err := s.rc.Lock(ctx, key,
		redis.WithLockTTL(s.cfg.LockTTL),
		redis.WithWaitTimeout(s.cfg.WaitTimeout),
		redis.WithWatchdog(s.cfg.Watchdog),
	)
	if errors.Is(err, redis.ErrLockBusy) {
		// The wait budget ran out: the caller may retry, queue, or give up.
		return nil, errs.Conflict(40901, "lock %q is held by someone else", name)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = l.Release(ctx) }()

	// Reentrancy: this nested Lock returns the same lease at once instead
	// of waiting behind itself; its Release drops one of the two levels.
	inner, err := s.rc.Lock(ctx, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = inner.Release(ctx) }()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.Lost():
		// The watchdog noticed the lease lapsed (failover or a long pause).
		return nil, errs.Internal(50001, "lease on %q was lost", name)
	case <-time.After(s.cfg.Hold):
	}

	obs.Info(ctx, "critical section done", "lock", name, "held", l.Held())
	return map[string]any{"acquired": true, "held": l.Held(), "hold_ms": s.cfg.Hold.Milliseconds()}, nil
}

// tryLock is the one-shot contrast to lock: a single SETNX attempt, no
// wait, no reentrancy. It suits a caller that would rather skip than queue.
func (s *redisService) tryLock(ctx context.Context, r *http.Request) (any, error) {
	name := r.PathValue("name")
	if name == "" {
		return nil, errs.Invalid(40001, "name is required")
	}

	release, ok, err := s.rc.TryLock(ctx, s.cfg.Prefix+"lock:"+name, s.cfg.LockTTL)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errs.Conflict(40901, "lock %q is held by someone else", name)
	}
	defer func() { _ = release(ctx) }()

	time.Sleep(s.cfg.Hold) // stand-in for the real critical section
	return map[string]any{"acquired": true, "hold_ms": s.cfg.Hold.Milliseconds()}, nil
}

func main() {
	conf := flag.String("conf", "examples/redis/config.yaml", "config file")
	flag.Parse()

	if err := verticle.Run(context.Background(), &redisService{}, verticle.WithConf(*conf)); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}
