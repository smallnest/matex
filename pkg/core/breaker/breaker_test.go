package breaker

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestBreaker(cfg Config) (*Breaker, *time.Time) {
	cur := time.Unix(0, 0)
	b := New(cfg)
	b.now = func() time.Time { return cur }
	return b, &cur
}

func fail(context.Context) error { return errors.New("boom") }
func ok(context.Context) error   { return nil }

// trip drives the breaker from closed to open.
func trip(t *testing.T, b *Breaker, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := b.Do(context.Background(), fail); err == nil || errors.Is(err, ErrOpen) {
			t.Fatalf("call %d: err=%v, want a plain failure", i+1, err)
		}
	}
	if b.State() != StateOpen {
		t.Fatalf("state = %v after %d failures, want open", b.State(), n)
	}
}

func TestOpensAfterConsecutiveFailures(t *testing.T) {
	b, cur := newTestBreaker(Config{Failures: 3, Cooldown: time.Minute})
	trip(t, b, 3)

	called := false
	err := b.Do(context.Background(), func(context.Context) error { called = true; return nil })
	if !errors.Is(err, ErrOpen) {
		t.Fatalf("err = %v, want ErrOpen", err)
	}
	if called {
		t.Fatal("the function ran while the breaker was open")
	}

	// Before the cooldown, still open.
	*cur = cur.Add(30 * time.Second)
	if err := b.Do(context.Background(), ok); !errors.Is(err, ErrOpen) {
		t.Fatalf("err = %v during the cooldown", err)
	}

	// After the cooldown the breaker probes.
	*cur = cur.Add(30 * time.Second)
	if err := b.Do(context.Background(), ok); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if b.State() != StateHalfOpen {
		t.Fatalf("state = %v, want half-open after one success", b.State())
	}
}

func TestClosesAfterEnoughProbeSuccesses(t *testing.T) {
	b, cur := newTestBreaker(Config{Failures: 1, Successes: 3, Cooldown: time.Minute})
	trip(t, b, 1)

	*cur = cur.Add(time.Minute)
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		if err := b.Do(ctx, ok); err != nil {
			t.Fatalf("probe %d: %v", i, err)
		}
		if i < 3 && b.State() != StateHalfOpen {
			t.Fatalf("state = %v after %d successes, want half-open", b.State(), i)
		}
	}
	if b.State() != StateClosed {
		t.Fatalf("state = %v, want closed", b.State())
	}
	// A closed breaker lets everything through again.
	for i := 0; i < 10; i++ {
		if err := b.Do(ctx, ok); err != nil {
			t.Fatalf("call %d after closing: %v", i, err)
		}
	}
}

func TestHalfOpenFailureReopens(t *testing.T) {
	b, cur := newTestBreaker(Config{Failures: 2, Cooldown: time.Minute})
	trip(t, b, 2)

	*cur = cur.Add(time.Minute)
	if err := b.Do(context.Background(), fail); err == nil || errors.Is(err, ErrOpen) {
		t.Fatalf("probe err = %v", err)
	}
	if b.State() != StateOpen {
		t.Fatalf("state = %v, want open again", b.State())
	}

	// The cooldown restarts from the failed probe.
	*cur = cur.Add(59 * time.Second)
	if err := b.Do(context.Background(), ok); !errors.Is(err, ErrOpen) {
		t.Fatalf("err = %v; the cooldown did not restart", err)
	}
	*cur = cur.Add(time.Second)
	if err := b.Do(context.Background(), ok); err != nil {
		t.Fatalf("probe after the new cooldown: %v", err)
	}
}

func TestSuccessResetsTheFailureStreak(t *testing.T) {
	b, _ := newTestBreaker(Config{Failures: 3})
	ctx := context.Background()

	_ = b.Do(ctx, fail)
	_ = b.Do(ctx, fail) // two failures: one more would trip it
	if err := b.Do(ctx, ok); err != nil {
		t.Fatal(err)
	}
	// The streak restarted, so two more failures must not open it.
	_ = b.Do(ctx, fail)
	_ = b.Do(ctx, fail)
	if b.State() != StateClosed {
		t.Fatalf("state = %v; a success should have reset the failure streak", b.State())
	}
}

func TestHalfOpenAllowsOneProbeAtATime(t *testing.T) {
	b, cur := newTestBreaker(Config{Failures: 1, Cooldown: time.Minute})
	trip(t, b, 1)
	*cur = cur.Add(time.Minute)

	var enteredOnce sync.Once
	entered := make(chan struct{})
	release := make(chan struct{})
	probe := func(context.Context) error {
		enteredOnce.Do(func() { close(entered) })
		<-release
		return nil
	}

	go func() { _ = b.Do(context.Background(), probe) }()
	<-entered

	err := b.Do(context.Background(), func(context.Context) error {
		t.Error("a second probe ran concurrently")
		return nil
	})
	if !errors.Is(err, ErrOpen) {
		t.Fatalf("concurrent probe err = %v, want ErrOpen", err)
	}
	close(release)
}

func TestDoValue(t *testing.T) {
	b, _ := newTestBreaker(Config{Failures: 1})
	ctx := context.Background()

	v, err := DoValue(ctx, b, func(context.Context) (int, error) { return 42, nil })
	if err != nil || v != 42 {
		t.Fatalf("v=%d err=%v", v, err)
	}

	_, _ = DoValue(ctx, b, func(context.Context) (int, error) { return 0, errors.New("boom") })
	if b.State() != StateOpen {
		t.Fatalf("state = %v, want open", b.State())
	}
	v, err = DoValue(ctx, b, func(context.Context) (int, error) { return 7, nil })
	if !errors.Is(err, ErrOpen) {
		t.Fatalf("err = %v, want ErrOpen", err)
	}
	if v != 0 {
		t.Fatalf("v = %d; the zero value must come back when the call is skipped", v)
	}
}

func TestOnStateChange(t *testing.T) {
	var (
		mu          sync.Mutex
		transitions []string
	)
	b, cur := newTestBreaker(Config{
		Failures: 1,
		Cooldown: time.Minute,
		OnStateChange: func(from, to State) {
			mu.Lock()
			defer mu.Unlock()
			transitions = append(transitions, from.String()+"→"+to.String())
		},
	})

	trip(t, b, 1)
	*cur = cur.Add(time.Minute)
	_ = b.Do(context.Background(), ok)

	mu.Lock()
	defer mu.Unlock()
	want := []string{"closed→open", "open→half-open"}
	if len(transitions) != len(want) {
		t.Fatalf("transitions = %v, want %v", transitions, want)
	}
	for i := range want {
		if transitions[i] != want[i] {
			t.Fatalf("transitions = %v, want %v", transitions, want)
		}
	}
}

func TestConfigDefaults(t *testing.T) {
	b := New(Config{})
	cfg := b.cfg
	if cfg.Failures != 5 || cfg.Successes != 2 || cfg.Cooldown != 10*time.Second {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
}

func TestStateString(t *testing.T) {
	for state, want := range map[State]string{
		StateClosed:   "closed",
		StateOpen:     "open",
		StateHalfOpen: "half-open",
		State(99):     "unknown",
	} {
		if got := state.String(); got != want {
			t.Errorf("State(%d).String() = %q, want %q", state, got, want)
		}
	}
}

// TestConcurrentUse exercises the lock under the race detector.
func TestConcurrentUse(t *testing.T) {
	b, _ := newTestBreaker(Config{Failures: 1000})
	var calls atomic.Int64

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = b.Do(context.Background(), func(context.Context) error {
					calls.Add(1)
					if (i+j)%2 == 0 {
						return errors.New("boom")
					}
					return nil
				})
			}
		}(i)
	}
	wg.Wait()

	if calls.Load() == 0 {
		t.Fatal("no calls went through")
	}
	if b.State() != StateClosed {
		t.Fatalf("state = %v; the threshold was never reached", b.State())
	}
}
