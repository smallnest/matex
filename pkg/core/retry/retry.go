// Package retry re-runs a call that failed for a transient reason.
//
//	err := retry.Do(ctx, retry.Config{Attempts: 3, BaseDelay: 50 * time.Millisecond},
//		func(ctx context.Context) error { return downstream.Call(ctx) })
//
// The delay before retry N is BaseDelay × Multiplier^(N-1), capped at
// MaxDelay, then spread across [0, delay) — full jitter, so a fleet that
// failed together does not retry together and turn a blip into a spike.
//
// Retrying is only safe for idempotent work. For writes, combine it with
// the idempotency middleware rather than hoping the second attempt is
// harmless.
package retry

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"time"
)

// Config configures a retry loop. Zero values take the documented defaults.
type Config struct {
	// Attempts is the total number of tries, including the first.
	Attempts int
	// BaseDelay is the wait before the second attempt.
	BaseDelay time.Duration
	// Multiplier grows the delay after each attempt.
	Multiplier float64
	// MaxDelay caps the delay; jitter is applied after the cap.
	MaxDelay time.Duration
	// NoJitter disables the random spread. Leave it off unless you have a
	// reason — synchronized retries are how a small failure becomes an
	// outage.
	NoJitter bool
	// Retryable decides whether an error is worth another attempt. By
	// default everything is retried except a cancelled or expired context,
	// which retrying cannot fix.
	Retryable func(error) bool
	// OnRetry is called before each retry, for logs and metrics.
	OnRetry func(attempt int, err error, delay time.Duration)

	// after is the sleep primitive, overridable in tests.
	after func(time.Duration) <-chan time.Time
}

func (c Config) withDefaults() Config {
	if c.Attempts <= 0 {
		c.Attempts = 3
	}
	if c.BaseDelay <= 0 {
		c.BaseDelay = 100 * time.Millisecond
	}
	if c.Multiplier < 1 {
		c.Multiplier = 2
	}
	if c.MaxDelay <= 0 {
		c.MaxDelay = 5 * time.Second
	}
	if c.Retryable == nil {
		c.Retryable = defaultRetryable
	}
	if c.after == nil {
		c.after = time.After
	}
	return c
}

// defaultRetryable retries everything except a context that is already
// done: sleeping and trying again cannot help once the deadline has passed.
func defaultRetryable(err error) bool {
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// Do runs fn until it succeeds, the attempts run out, the error stops being
// retryable, or ctx is done. It returns the last error.
func Do(ctx context.Context, cfg Config, fn func(context.Context) error) error {
	_, err := DoValue(ctx, cfg, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, fn(ctx)
	})
	return err
}

// DoValue is Do for a function that returns a value.
func DoValue[T any](ctx context.Context, cfg Config, fn func(context.Context) (T, error)) (T, error) {
	cfg = cfg.withDefaults()

	var (
		zero    T
		lastErr error
	)
	for attempt := 1; attempt <= cfg.Attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return zero, lastErr
			}
			return zero, err
		}

		v, err := fn(ctx)
		if err == nil {
			return v, nil
		}
		lastErr = err

		if attempt == cfg.Attempts || !cfg.Retryable(err) {
			return zero, lastErr
		}

		delay := cfg.delay(attempt)
		if cfg.OnRetry != nil {
			cfg.OnRetry(attempt, lastErr, delay)
		}
		select {
		case <-ctx.Done():
			return zero, lastErr
		case <-cfg.after(delay):
		}
	}
	return zero, lastErr
}

// delay is the backoff before the retry that follows the given attempt.
func (c Config) delay(attempt int) time.Duration {
	d := float64(c.BaseDelay) * math.Pow(c.Multiplier, float64(attempt-1))
	if d > float64(c.MaxDelay) {
		d = float64(c.MaxDelay)
	}
	if !c.NoJitter && d > 0 {
		// Full jitter: spread across the whole window rather than adding a
		// small ± on top of a common base.
		d = rand.Float64() * d
	}
	return time.Duration(d)
}
