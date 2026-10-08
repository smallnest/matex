// Package cache adds the cache-aside patterns that a redis client alone
// does not give you.
//
// A plain get-or-load has two failure modes that only show up under load:
//
//	stampede   a hot key expires and every request reloads it at once,
//	           so the database gets the burst the cache was meant to absorb
//	synchronized expiry
//	           a batch of keys written together expires together, producing
//	           that burst on a schedule
//
// GetOrLoad closes both: concurrent misses for one key are collapsed into a
// single load, and each stored TTL is spread across a window.
//
//	v, err := cache.GetOrLoad(ctx, env.Redis, key, cache.Config{TTL: time.Minute},
//		func(ctx context.Context) (*User, error) { return dao.ByID(ctx, env.DB, id) })
//
// A nil client is not an error: the load runs uncached, which keeps the call
// site free of `if env.Redis != nil` branches.
package cache

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/smallnest/matex/pkg/core/obs"
	"github.com/smallnest/matex/pkg/core/redis"
)

// DefaultTTL is used when Config.TTL is not set.
const DefaultTTL = time.Minute

// Config configures a GetOrLoad call.
type Config struct {
	// TTL is the base lifetime of a stored value.
	TTL time.Duration
	// Jitter shortens each TTL by a random amount up to this, so keys
	// written together do not expire together. A value around 10-20% of TTL
	// is usually right.
	Jitter time.Duration
}

func (c Config) ttl() time.Duration {
	ttl := c.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if c.Jitter <= 0 {
		return ttl
	}
	jitter := c.Jitter
	if jitter >= ttl {
		// Never jitter a TTL down to nothing: a zero expiry means "delete",
		// not "cache briefly".
		jitter = ttl / 2
	}
	return ttl - time.Duration(rand.Int63n(int64(jitter)))
}

// loads collapses concurrent misses for one key into a single load. Shared
// across the process by design: a stampede is a process-wide event.
var loads singleflight.Group

// GetOrLoad returns the cached value for key, or calls load once, stores the
// result and returns it.
//
// The loading call runs under the first caller's context, so that caller
// giving up also ends the shared load. That is deliberate — the alternative
// is a request whose caller has already gone away still holding a database
// connection — but it does mean a cancelled first caller fails the whole
// group, not just itself.
func GetOrLoad[T any](ctx context.Context, c *redis.Client, key string, cfg Config, load func(context.Context) (T, error)) (T, error) {
	var zero T
	if c == nil {
		return load(ctx)
	}
	if v, ok, err := redis.GetJSON[T](ctx, c, key); err == nil && ok {
		return v, nil
	}

	res, err, _ := loads.Do(key, func() (any, error) {
		// Whoever waited for the group deserves a second look: the value may
		// have been written while they were queued.
		if v, ok, err := redis.GetJSON[T](ctx, c, key); err == nil && ok {
			return v, nil
		}
		v, err := load(ctx)
		if err != nil {
			return zero, err
		}
		if err := c.SetJSON(ctx, key, v, cfg.ttl()); err != nil {
			// A cache write failure is a performance problem, not a
			// correctness one: the value is already in hand.
			obs.Warn(ctx, "cache write failed", "key", key, "err", err)
		}
		return v, nil
	})
	if err != nil {
		return zero, err
	}
	v, ok := res.(T)
	if !ok {
		return zero, errors.New("cache: stored value has an unexpected type")
	}
	return v, nil
}

// Invalidate drops keys so the next read reloads them. Call it after a write
// that changes what those keys hold.
//
// Deleting rather than updating is the point: a concurrent writer would
// otherwise be able to leave a stale value behind, and a delete followed by
// a read is always safe.
func Invalidate(ctx context.Context, c *redis.Client, keys ...string) error {
	if c == nil || len(keys) == 0 {
		return nil
	}
	return c.Del(ctx, keys...)
}

// Key builds a namespaced cache key: Key("user", 42) → "user:42".
//
// Namespace every key. Cache keys share one flat redis database with
// everything else the service stores, and GetOrLoad coalesces by the literal
// key string, so two features that both use "1" would otherwise collide.
func Key(parts ...any) string {
	var b strings.Builder
	for i, p := range parts {
		if i > 0 {
			b.WriteByte(':')
		}
		fmt.Fprint(&b, p)
	}
	return b.String()
}
