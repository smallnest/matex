package breaker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/smallnest/matex/pkg/core/errs"
)

func serverErr(context.Context) error { return errs.Internal(50001, "boom") }
func clientErr(context.Context) error { return errs.NotFound(40401, "gone") }

// run drives n calls that all fail, or all succeed when f is ok.
func run(b *Breaker, n int, f func(context.Context) error) {
	for i := 0; i < n; i++ {
		_ = b.Do(context.Background(), f)
	}
}

func TestRatioOpensOnFailuresInsideTheWindow(t *testing.T) {
	b, _ := newTestBreaker(Config{Ratio: 0.5, MinSamples: 10, Window: time.Minute, Cooldown: time.Minute})
	run(b, 4, ok)
	run(b, 6, serverErr) // 10 samples, 60% failure
	if b.State() != StateOpen {
		t.Fatalf("state = %v, want open at a 60%% failure ratio", b.State())
	}
	if err := b.Do(context.Background(), ok); !errors.Is(err, ErrOpen) {
		t.Fatalf("err = %v, want ErrOpen", err)
	}
}

func TestRatioNeedsEnoughSamples(t *testing.T) {
	b, _ := newTestBreaker(Config{Ratio: 0.5, MinSamples: 10})
	run(b, 9, serverErr) // 100% failure but only 9 samples
	if b.State() != StateClosed {
		t.Fatalf("state = %v; MinSamples must gate the ratio", b.State())
	}
	run(b, 1, serverErr)
	if b.State() != StateOpen {
		t.Fatalf("state = %v; the 10th sample must be evaluated", b.State())
	}
}

// TestRatioIgnoresStaleOutcomes: outcomes older than the window slide out,
// so a burst of failures long ago does not open the breaker today.
func TestRatioIgnoresStaleOutcomes(t *testing.T) {
	b, cur := newTestBreaker(Config{Ratio: 0.5, MinSamples: 4, Window: time.Minute})
	run(b, 3, serverErr)
	*cur = cur.Add(3 * time.Minute) // clearly beyond the window, clock grid aside

	run(b, 4, ok) // fresh samples only; the 3 old failures are outside the window
	if b.State() != StateClosed {
		t.Fatalf("state = %v; stale failures must not count", b.State())
	}
}

// TestRatioLowErrorRateStaysClosed: a few 5xx inside healthy traffic must
// not trip the breaker, which is the point of the ratio model.
func TestRatioLowErrorRateStaysClosed(t *testing.T) {
	b, _ := newTestBreaker(Config{Ratio: 0.5, MinSamples: 5})
	run(b, 100, ok)
	run(b, 2, serverErr)
	if b.State() != StateClosed {
		t.Fatalf("state = %v, want closed at a 2%% failure ratio", b.State())
	}
}

func TestClientErrorsDoNotCountAsFailures(t *testing.T) {
	b, _ := newTestBreaker(Config{Ratio: 0.5, MinSamples: 2})
	run(b, 10, clientErr) // 404s are the caller's fault
	if b.State() != StateClosed {
		t.Fatalf("state = %v; 4xx errors must not open the breaker", b.State())
	}

	// The same holds in the consecutive-failures model.
	c, _ := newTestBreaker(Config{Failures: 2})
	run(c, 10, clientErr)
	if c.State() != StateClosed {
		t.Fatalf("state = %v; 4xx errors must not open the breaker", c.State())
	}
}

func TestCustomIsFailure(t *testing.T) {
	sentinel := errors.New("business rejection")
	b, _ := newTestBreaker(Config{Ratio: 0.5, MinSamples: 2, IsFailure: func(err error) bool {
		return errors.Is(err, sentinel)
	}})
	run(b, 10, func(context.Context) error { return sentinel })
	if b.State() != StateOpen {
		t.Fatalf("state = %v; the custom classifier was ignored", b.State())
	}
}

func TestRatioHalfOpenRecovers(t *testing.T) {
	b, cur := newTestBreaker(Config{Ratio: 0.5, MinSamples: 2, Cooldown: time.Minute, Successes: 2})
	run(b, 2, serverErr)
	if b.State() != StateOpen {
		t.Fatalf("state = %v, want open", b.State())
	}
	*cur = cur.Add(time.Minute)
	run(b, 2, ok)
	if b.State() != StateClosed {
		t.Fatalf("state = %v, want closed after two probe successes", b.State())
	}
	// Recovery starts fresh only if the window was cleared: with clearing,
	// the first post-close failure leaves 1/3 and stays closed; without it,
	// the two failures from before the open give 3/5 and re-open at once.
	run(b, 1, serverErr)
	if b.State() != StateClosed {
		t.Fatalf("state = %v; the window must be cleared on close", b.State())
	}
}

func TestRatioConfigDefaults(t *testing.T) {
	cfg := New(Config{Ratio: 1.5}).cfg
	if cfg.Ratio != 1 || cfg.Window != time.Minute || cfg.MinSamples != 20 {
		t.Fatalf("ratio defaults: %+v", cfg)
	}
	// Without a ratio the consecutive model keeps its own defaults untouched.
	cfg = New(Config{}).cfg
	if cfg.Window != 0 || cfg.MinSamples != 0 || cfg.Failures != 5 {
		t.Fatalf("consecutive defaults: %+v", cfg)
	}
}
