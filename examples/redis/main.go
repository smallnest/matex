// Command redis demonstrates pkg/core/redis: cache-aside with JSON values
// and a best-effort distributed lock.
//
// Redis must be running (make dev). A configured `redis:` section is
// created by verticle.Run and handed over as env.Redis; when the section
// is absent env.Redis is nil.
//
// Run:
//
//	make dev
//	go run ./examples/redis
//
//	curl -i localhost:8080/api/v1/cache/world          # 第一次 miss，第二次 hit
//	curl -i localhost:8080/api/v1/cache/world
//	curl -i -X DELETE localhost:8080/api/v1/cache/world
//	curl -i -X POST localhost:8080/api/v1/lock/job1     # 并发两个，第二个 409
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
	Prefix  string        `json:"prefix" default:"example:"`
	TTL     time.Duration `json:"ttl" default:"30s"`
	LockTTL time.Duration `json:"lock_ttl" default:"5s"`
	Hold    time.Duration `json:"hold" default:"100ms"`
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
	if env.Redis == nil {
		return errors.New("rediscache: the `redis:` section is required (start Redis first: make dev)")
	}
	s.rc = env.Redis
	return nil
}

func (s *redisService) BuildRouter(srv *httpx.Server) error {
	srv.Handle("GET", "/api/v1/cache/{key}", s.get)
	srv.Handle("DELETE", "/api/v1/cache/{key}", s.delete)
	srv.Handle("POST", "/api/v1/lock/{name}", s.lock)
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

// lock runs a short critical section under TryLock. The lock carries a
// TTL, so a holder that crashes cannot deadlock everyone else.
func (s *redisService) lock(ctx context.Context, r *http.Request) (any, error) {
	name := r.PathValue("name")
	if name == "" {
		return nil, errs.Invalid(40001, "name is required")
	}

	release, ok, err := s.rc.TryLock(ctx, s.cfg.Prefix+"lock:"+name, s.cfg.LockTTL)
	if err != nil {
		return nil, err
	}
	if !ok {
		// Let the caller decide: retry, queue, or give up.
		return nil, errs.Conflict(40901, "lock %q is held by someone else", name)
	}
	defer func() { _ = release(ctx) }()

	start := time.Now()
	time.Sleep(s.cfg.Hold) // stand-in for the real critical section
	obs.Info(ctx, "critical section done", "lock", name, "hold_ms", time.Since(start).Milliseconds())

	return map[string]any{"acquired": true, "hold_ms": time.Since(start).Milliseconds()}, nil
}

func main() {
	conf := flag.String("conf", "examples/redis/config.yaml", "config file")
	flag.Parse()

	if err := verticle.Run(context.Background(), &redisService{}, verticle.WithConf(*conf)); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}
