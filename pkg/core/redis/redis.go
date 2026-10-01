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

func randomToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
