// Package memcache wraps gomemcache with JSON helpers. gomemcache
// predates context; timeouts are set on the client instead.
package memcache

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	gomc "github.com/bradfitz/gomemcache/memcache"
)

// ErrMiss re-exports the cache-miss sentinel.
var ErrMiss = gomc.ErrCacheMiss

// Config configures the client. Leave Addrs empty to disable memcache.
type Config struct {
	Addrs   []string      `json:"addrs" optional:""`
	Timeout time.Duration `json:"timeout" default:"300ms"`
	MaxIdle int           `json:"max_idle" default:"16"`
}

// Client is the matex memcache client.
type Client struct {
	mc *gomc.Client
}

// Open creates a client (no connectivity check — memcache has no ping;
// the first command will fail if unreachable).
func Open(cfg Config) *Client {
	mc := gomc.New(cfg.Addrs...)
	if cfg.Timeout > 0 {
		mc.Timeout = cfg.Timeout
	}
	if cfg.MaxIdle > 0 {
		mc.MaxIdleConns = cfg.MaxIdle
	}
	return &Client{mc: mc}
}

// SetJSON marshals v and stores it with a TTL (seconds granularity).
func (c *Client) SetJSON(key string, v any, ttl time.Duration) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("memcache: marshal %s: %w", key, err)
	}
	if err := c.mc.Set(&gomc.Item{Key: key, Value: b, Expiration: int32(ttl.Seconds())}); err != nil {
		return fmt.Errorf("memcache: set %s: %w", key, err)
	}
	return nil
}

// GetJSON fetches and unmarshals v; ok is false on a miss.
func GetJSON[T any](c *Client, key string) (v T, ok bool, err error) {
	item, err := c.mc.Get(key)
	if errors.Is(err, gomc.ErrCacheMiss) {
		return v, false, nil
	}
	if err != nil {
		return v, false, fmt.Errorf("memcache: get %s: %w", key, err)
	}
	if err := json.Unmarshal(item.Value, &v); err != nil {
		return v, false, fmt.Errorf("memcache: unmarshal %s: %w", key, err)
	}
	return v, true, nil
}

// Delete removes a key (missing keys are not an error).
func (c *Client) Delete(key string) error {
	err := c.mc.Delete(key)
	if errors.Is(err, gomc.ErrCacheMiss) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("memcache: delete %s: %w", key, err)
	}
	return nil
}

// Touch extends the TTL of a key.
func (c *Client) Touch(key string, ttl time.Duration) error {
	if err := c.mc.Touch(key, int32(ttl.Seconds())); err != nil {
		return fmt.Errorf("memcache: touch %s: %w", key, err)
	}
	return nil
}
