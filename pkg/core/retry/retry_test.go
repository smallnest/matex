package retry

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// noSleep replaces the sleep primitive so tests never actually wait.
func noSleep(cfg *Config) *[]time.Duration {
	var slept []time.Duration
	cfg.after = func(d time.Duration) <-chan time.Time {
		slept = append(slept, d)
		ch := make(chan time.Time, 1)
		ch <- time.Unix(0, 0)
		return ch
	}
	return &slept
}

func TestSucceedsAfterTransientFailures(t *testing.T) {
	var cfg Config
	slept := noSleep(&cfg)
	cfg.Attempts = 3

	var calls int32
	err := Do(context.Background(), cfg, func(context.Context) error {
		if atomic.AddInt32(&calls, 1) < 3 {
			return errors.New("transient")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("called %d times, want 3", got)
	}
	// One wait before the second attempt and one before the third.
	if len(*slept) != 2 {
		t.Fatalf("slept %d times, want 2", len(*slept))
	}
}

func TestGivesUpAfterAttempts(t *testing.T) {
	var cfg Config
	noSleep(&cfg)
	cfg.Attempts = 3

	sentinel := errors.New("always fails")
	var calls int32
	err := Do(context.Background(), cfg, func(context.Context) error {
		atomic.AddInt32(&calls, 1)
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the last error", err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("called %d times, want exactly Attempts", got)
	}
}

func TestNonRetryableStopsImmediately(t *testing.T) {
	var cfg Config
	noSleep(&cfg)
	cfg.Attempts = 5
	cfg.Retryable = func(err error) bool { return !errors.Is(err, errPermanent) }

	var calls int32
	err := Do(context.Background(), cfg, func(context.Context) error {
		atomic.AddInt32(&calls, 1)
		return errPermanent
	})
	if !errors.Is(err, errPermanent) {
		t.Fatalf("err = %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("called %d times; a permanent error must not be retried", got)
	}
}

var errPermanent = errors.New("permanent")

func TestContextCancellationIsNotRetried(t *testing.T) {
	var cfg Config
	noSleep(&cfg)
	cfg.Attempts = 5

	var calls int32
	err := Do(context.Background(), cfg, func(context.Context) error {
		atomic.AddInt32(&calls, 1)
		return context.DeadlineExceeded
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("called %d times; an expired deadline must not be retried", got)
	}
}

func TestContextAlreadyDoneReturnsWithoutCalling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var calls int32
	err := Do(ctx, Config{Attempts: 3}, func(context.Context) error {
		atomic.AddInt32(&calls, 1)
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("called %d times with a cancelled context", got)
	}
}

func TestDelayGrowsAndCaps(t *testing.T) {
	cfg := Config{
		BaseDelay:  100 * time.Millisecond,
		Multiplier: 2,
		MaxDelay:   time.Second,
		NoJitter:   true,
	}
	want := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
		time.Second, // capped
		time.Second,
	}
	for i, w := range want {
		if got := cfg.delay(i + 1); got != w {
			t.Errorf("delay(attempt=%d) = %v, want %v", i+1, got, w)
		}
	}
}

// TestJitterSpreadsTheDelay: without jitter every client that failed at the
// same instant retries at the same instant.
func TestJitterSpreadsTheDelay(t *testing.T) {
	cfg := Config{BaseDelay: time.Second, Multiplier: 2, MaxDelay: time.Minute}

	seen := map[time.Duration]bool{}
	for i := 0; i < 50; i++ {
		d := cfg.delay(1)
		if d < 0 || d > time.Second {
			t.Fatalf("jittered delay %v is outside [0, 1s)", d)
		}
		seen[d] = true
	}
	if len(seen) < 2 {
		t.Fatal("jitter produced identical delays; it is not spreading anything")
	}
}

func TestOnRetryReportsEachRetry(t *testing.T) {
	var cfg Config
	noSleep(&cfg)
	cfg.Attempts = 3

	type attempt struct {
		n     int
		delay time.Duration
	}
	var seen []attempt
	cfg.OnRetry = func(n int, _ error, d time.Duration) {
		seen = append(seen, attempt{n, d})
	}

	_ = Do(context.Background(), cfg, func(context.Context) error { return errors.New("boom") })
	if len(seen) != 2 {
		t.Fatalf("OnRetry called %d times, want 2", len(seen))
	}
	if seen[0].n != 1 || seen[1].n != 2 {
		t.Fatalf("attempt numbers = %v", seen)
	}
}

func TestDoValue(t *testing.T) {
	var cfg Config
	noSleep(&cfg)
	cfg.Attempts = 3

	var calls int32
	v, err := DoValue(context.Background(), cfg, func(context.Context) (string, error) {
		if atomic.AddInt32(&calls, 1) < 2 {
			return "", errors.New("transient")
		}
		return "ok", nil
	})
	if err != nil || v != "ok" {
		t.Fatalf("v=%q err=%v", v, err)
	}

	// On failure the zero value comes back, never a stale partial result.
	_, _ = DoValue(context.Background(), cfg, func(context.Context) (string, error) { return "ignored", errPermanent })
	if v, err := DoValue(context.Background(), Config{Attempts: 1}, func(context.Context) (string, error) {
		return "partial", errPermanent
	}); err == nil || v != "" {
		t.Fatalf("v=%q err=%v, want the zero value", v, err)
	}
}

func TestDefaults(t *testing.T) {
	cfg := Config{}.withDefaults()
	if cfg.Attempts != 3 || cfg.BaseDelay != 100*time.Millisecond ||
		cfg.Multiplier != 2 || cfg.MaxDelay != 5*time.Second {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	if cfg.Retryable == nil || cfg.after == nil {
		t.Fatal("default funcs not applied")
	}
	// A multiplier below 1 would shrink the delay into no backoff at all.
	if got := (Config{Attempts: 2, Multiplier: 0.5}).withDefaults().Multiplier; got != 2 {
		t.Fatalf("multiplier = %v, want 2", got)
	}
}
