// Command cache demonstrates pkg/core/cache: cache-aside without the two
// failure modes that only show up under load.
//
//	GetOrLoad   collapses concurrent misses for one key into a single load,
//	            so a hot key expiring does not stampede the source
//	Jitter      spreads each stored TTL, so keys written together do not
//	            expire together and produce that stampede on a schedule
//
// The store is a fake "database" with a deliberately slow read, so the
// difference between a hit and a miss is easy to see.
//
// Run (needs redis: `make dev`):
//
//	go run ./examples/cache
//
//	curl -s localhost:8083/api/v1/users/1        # miss: ~50ms, source read
//	curl -s localhost:8083/api/v1/users/1        # hit: fast, no source read
//	curl -s localhost:8083/api/v1/stats          # how many reads reached the source
//
//	# concurrent misses collapse into one load
//	for i in $(seq 1 20); do curl -s -o /dev/null localhost:8083/api/v1/users/2 & done; wait
//	curl -s localhost:8083/api/v1/stats
//
//	# a write invalidates, so the next read reloads
//	curl -s -X PUT localhost:8083/api/v1/users/1 -d '{"name":"alice v2"}'
//
//	go test ./examples/cache
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/smallnest/matex/pkg/core/cache"
	"github.com/smallnest/matex/pkg/core/errs"
	"github.com/smallnest/matex/pkg/core/httpx"
	"github.com/smallnest/matex/pkg/core/obs"
	"github.com/smallnest/matex/pkg/core/redis"
	"github.com/smallnest/matex/pkg/core/verticle"
)

// errNotFound is what the store returns for a missing row; the handler maps
// it to a 404 instead of a 500.
var errNotFound = errors.New("store: not found")

type cacheConfig struct {
	TTL    time.Duration `json:"ttl" default:"30s"`
	Jitter time.Duration `json:"jitter" default:"5s"`
}

type cacheService struct {
	cfg   cacheConfig
	redis *redis.Client // may be nil: GetOrLoad then goes straight to the store
	users *userStore
}

func (s *cacheService) Name() string { return "cache" }

func (s *cacheService) Setup(_ context.Context, env *verticle.Env) error {
	if err := env.DecodeService(&s.cfg); err != nil {
		return err
	}
	s.redis = env.Redis
	s.users = newUserStore()
	return nil
}

func (s *cacheService) BuildRouter(srv *httpx.Server) error {
	srv.Handle("GET", "/api/v1/users/{id}", s.getUser)
	srv.Handle("PUT", "/api/v1/users/{id}", s.putUser)
	srv.Handle("GET", "/api/v1/stats", s.stats)
	return nil
}

func (s *cacheService) getUser(ctx context.Context, r *http.Request) (any, error) {
	id := r.PathValue("id")
	if id == "" {
		return nil, errs.Invalid(40001, "id is required")
	}

	u, err := cache.GetOrLoad(ctx, s.redis, cache.Key("user", id),
		cache.Config{TTL: s.cfg.TTL, Jitter: s.cfg.Jitter},
		func(ctx context.Context) (User, error) {
			return s.users.load(ctx, id)
		})
	switch {
	case errors.Is(err, errNotFound):
		return nil, errs.NotFound(40401, "user %s not found", id)
	case err != nil:
		return nil, errs.Internal(50001, "load user: %v", err)
	}
	return map[string]any{"user": u, "source_reads": s.users.reads()}, nil
}

func (s *cacheService) putUser(ctx context.Context, r *http.Request) (any, error) {
	id := r.PathValue("id")
	var req struct {
		Name string `json:"name"`
	}
	if err := httpx.ReadJSON(r, &req); err != nil {
		return nil, errs.Invalid(40002, "bad request body: %v", err)
	}
	if req.Name == "" {
		return nil, errs.Invalid(40003, "name is required")
	}

	u, err := s.users.save(id, req.Name)
	if err != nil {
		return nil, err
	}
	// Invalidate rather than update: the next read reloads, and a concurrent
	// writer cannot leave a stale value behind.
	if err := cache.Invalidate(ctx, s.redis, cache.Key("user", id)); err != nil {
		obs.Warn(ctx, "invalidating the cache failed", "id", id, "err", err)
	}
	return map[string]any{"user": u, "invalidated": true}, nil
}

func (s *cacheService) stats(_ context.Context, _ *http.Request) (any, error) {
	return map[string]any{"source_reads": s.users.reads()}, nil
}

// User is both the stored value and the response body.
type User struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// userStore stands in for the database. Reads are slow on purpose: without
// the cache, a burst of requests for one key would be a burst of queries.
type userStore struct {
	readCount atomic.Int32

	mu    sync.Mutex
	users map[string]User
}

func newUserStore() *userStore {
	return &userStore{users: map[string]User{
		"1": {ID: "1", Name: "alice"},
		"2": {ID: "2", Name: "bob"},
		"3": {ID: "3", Name: "carol"},
	}}
}

func (s *userStore) load(ctx context.Context, id string) (User, error) {
	s.readCount.Add(1)
	select {
	case <-ctx.Done():
		return User{}, ctx.Err()
	case <-time.After(50 * time.Millisecond): // pretend this is a query
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	if !ok {
		return User{}, errNotFound
	}
	return u, nil
}

func (s *userStore) save(id, name string) (User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := User{ID: id, Name: name}
	s.users[id] = u
	return u, nil
}

func (s *userStore) reads() int { return int(s.readCount.Load()) }

func main() {
	conf := flag.String("conf", "examples/cache/config.yaml", "config file")
	flag.Parse()

	if err := verticle.Run(context.Background(), &cacheService{}, verticle.WithConf(*conf)); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}
