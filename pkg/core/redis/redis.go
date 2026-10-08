// Package redis wraps go-redis with convenient initialization and JSON
// helpers. Business code uses this package instead of importing
// go-redis directly (depguard enforces the choke point).
package redis

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// defaultPoolSize mirrors the "pool_size" struct tag; keep in sync.
const defaultPoolSize = 20

// Config configures the client. Leave Addr empty to disable redis.
type Config struct {
	Addr         string        `json:"addr" env:"REDIS_ADDR"`
	Username     string        `json:"username" optional:""`
	Password     string        `json:"password" optional:""`
	DB           int           `json:"db" default:"0"`
	DialTimeout  time.Duration `json:"dial_timeout" default:"3s"`
	ReadTimeout  time.Duration `json:"read_timeout" default:"1s"`
	WriteTimeout time.Duration `json:"write_timeout" default:"1s"`
	PoolSize     int           `json:"pool_size" default:"20"` // keep in sync with defaultPoolSize
}

// Client is the matex redis client.
type Client struct {
	c *goredis.Client
}

// Open creates a client and verifies connectivity with a ping.
func Open(cfg Config) (*Client, error) {
	poolSize := cfg.PoolSize
	if poolSize <= 0 {
		poolSize = defaultPoolSize
	}
	c := goredis.NewClient(&goredis.Options{
		Addr:         cfg.Addr,
		Username:     cfg.Username,
		Password:     cfg.Password,
		DB:           cfg.DB,
		DialTimeout:  cfg.DialTimeout,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		PoolSize:     poolSize,
	})
	ctx, cancel := context.WithTimeout(context.Background(), cfg.DialTimeout)
	defer cancel()
	if err := c.Ping(ctx).Err(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("redis: ping %s: %w", cfg.Addr, err)
	}
	return &Client{c: c}, nil
}

// New wraps an existing go-redis client (used by tests).
func New(c *goredis.Client) *Client { return &Client{c: c} }

// Cmdable exposes the full redis API for anything the helpers don't cover.
func (c *Client) Cmdable() goredis.Cmdable { return c.c }

// Ping checks connectivity.
func (c *Client) Ping(ctx context.Context) error { return c.c.Ping(ctx).Err() }

// Close releases the pool.
func (c *Client) Close() error { return c.c.Close() }

// SetJSON marshals v and stores it with a TTL.
func (c *Client) SetJSON(ctx context.Context, key string, v any, ttl time.Duration) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("redis: marshal %s: %w", key, err)
	}
	if err := c.c.Set(ctx, key, b, ttl).Err(); err != nil {
		return fmt.Errorf("redis: set %s: %w", key, err)
	}
	return nil
}

// GetJSON fetches and unmarshals v. ok is false on a cache miss;
// corruption is returned as an error (decide per case whether to
// treat it as a miss and overwrite).
func GetJSON[T any](ctx context.Context, c *Client, key string) (v T, ok bool, err error) {
	b, err := c.c.Get(ctx, key).Bytes()
	if errors.Is(err, goredis.Nil) {
		return v, false, nil
	}
	if err != nil {
		return v, false, fmt.Errorf("redis: get %s: %w", key, err)
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return v, false, fmt.Errorf("redis: unmarshal %s: %w", key, err)
	}
	return v, true, nil
}

// IsNotFound reports whether err is a missing-key error, so callers do not
// have to reach for the driver's sentinel.
func IsNotFound(err error) bool { return errors.Is(err, goredis.Nil) }

// Del removes keys (missing keys are not an error).
func (c *Client) Del(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	if err := c.c.Del(ctx, keys...).Err(); err != nil {
		return fmt.Errorf("redis: del %v: %w", keys, err)
	}
	return nil
}

// Claim takes key for ttl, reporting false when someone already holds it.
//
// Unlike TryLock the caller never releases it: the key is meant to expire on
// its own. That is what makes a windowed claim work — "exactly one instance
// acts in each interval" — where a lock released the moment the work ends
// would let a second instance with an offset clock act in the same window.
func (c *Client) Claim(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	ok, err := c.c.SetNX(ctx, key, "1", ttl).Result()
	if err != nil {
		return false, fmt.Errorf("redis: claim %s: %w", key, err)
	}
	return ok, nil
}

// TryLock acquires a best-effort distributed lock. The returned release
// function deletes the key only if the caller still owns it; the lock
// self-expires after ttl, so a crashed holder cannot deadlock others.
//
//	ok is false when someone else holds the lock — retry or skip.
func (c *Client) TryLock(ctx context.Context, key string, ttl time.Duration) (release func(context.Context) error, ok bool, err error) {
	token := randomToken()
	ok, err = c.c.SetNX(ctx, key, token, ttl).Result()
	if err != nil || !ok {
		return nil, false, err
	}
	return func(ctx context.Context) error {
		if err := releaseScript.Run(ctx, c.c, []string{key}, token).Err(); err != nil {
			return fmt.Errorf("redis: release %s: %w", key, err)
		}
		return nil
	}, true, nil
}

// releaseScript deletes the lock only when the token still matches.
var releaseScript = goredis.NewScript(`if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("del", KEYS[1])
else
	return 0
end`)

// Script is a reusable Lua script. Wrap atomic read-modify-write sequences
// that a helper API cannot express in one round trip — counters, sliding
// windows, compare-and-set.
type Script struct{ s *goredis.Script }

// NewScript compiles src into a reusable script. go-redis sends EVALSHA and
// transparently falls back to EVAL until the server has it cached.
func NewScript(src string) *Script { return &Script{s: goredis.NewScript(src)} }

// RunInt runs the script and returns its integer reply.
func (s *Script) RunInt(ctx context.Context, c *Client, keys []string, args ...any) (int64, error) {
	return s.s.Run(ctx, c.c, keys, args...).Int64()
}

// Run runs the script and returns the raw command, for reply shapes RunInt
// does not cover.
func (s *Script) Run(ctx context.Context, c *Client, keys []string, args ...any) *goredis.Cmd {
	return s.s.Run(ctx, c.c, keys, args...)
}

// IncrBy increments key by n and returns the new value, arming ttl on the
// first increment. It is the counter primitive behind fixed-window rate
// limiting and idempotency bookkeeping.
func (c *Client) IncrBy(ctx context.Context, key string, n int64, ttl time.Duration) (int64, error) {
	v, err := incrWithTTL.RunInt(ctx, c, []string{key}, n, ttl.Milliseconds())
	if err != nil {
		return 0, fmt.Errorf("redis: incr %s: %w", key, err)
	}
	return v, nil
}

// incrWithTTL keeps the counter and its expiry in step: setting them in two
// calls would let a crash leave a counter that never expires.
var incrWithTTL = NewScript(`local n = redis.call("incrby", KEYS[1], ARGV[1])
if n == tonumber(ARGV[1]) then
	redis.call("pexpire", KEYS[1], ARGV[2])
end
return n`)

func randomToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
