// Package redistest starts a Redis for tests — a real one via
// REDIS_TEST_ADDR or an in-process miniredis otherwise, so tests need
// no Docker.
package redistest

import (
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"github.com/smallnest/matex/pkg/core/redis"
)

// Start returns a *redis.Client wired to t (miniredis is fast-forward
// capable via TTL manipulation when needed).
func Start(t testing.TB) *redis.Client {
	t.Helper()
	if addr := os.Getenv("REDIS_TEST_ADDR"); addr != "" {
		rc, err := redis.Open(redis.Config{Addr: addr, DialTimeout: 2 * time.Second})
		if err != nil {
			t.Fatalf("redistest: open %s: %v", addr, err)
		}
		t.Cleanup(func() { _ = rc.Close() })
		return rc
	}
	mr := miniredis.RunT(t)
	return redis.New(goredis.NewClient(&goredis.Options{Addr: mr.Addr()}))
}

// Addr returns a redis address, starting a miniredis if REDIS_TEST_ADDR
// is unset. Useful for code that builds its own client.
func Addr(t testing.TB) string {
	t.Helper()
	if addr := os.Getenv("REDIS_TEST_ADDR"); addr != "" {
		return addr
	}
	return miniredis.RunT(t).Addr()
}
