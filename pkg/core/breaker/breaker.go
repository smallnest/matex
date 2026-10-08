// Package breaker stops a service from hammering a dependency that is
// already failing.
//
// The classic three-state machine:
//
//	closed     traffic flows; consecutive failures trip it open
//	open       every call fails fast for the cooldown, no load on the target
//	half-open  one probe at a time is let through; enough successes close it
//
//	err := b.Do(ctx, func(ctx context.Context) error {
//		return payments.Charge(ctx, order)
//	})
//	if errors.Is(err, breaker.ErrOpen) {
//		return nil, errs.Unavailable(50302, "payment service is unavailable")
//	}
//
// Failing fast is the point: without it, every request in the fleet waits
// out its own timeout against a service that cannot answer, and the
// timeout queue becomes the outage.
package breaker

import (
	"context"
	"errors"
	"sync"
	"time"
)

// State is the breaker's state.
type State int

const (
	// StateClosed lets traffic through.
	StateClosed State = iota
	// StateOpen rejects everything until the cooldown expires.
	StateOpen
	// StateHalfOpen lets a single probe through at a time.
	StateHalfOpen
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// ErrOpen is returned instead of calling the function while the breaker is
// open. Callers check it with errors.Is to tell "we did not even try" from
// "we tried and it failed".
var ErrOpen = errors.New("breaker: circuit is open")

// Config configures a Breaker. Zero values take the documented defaults.
type Config struct {
	// Failures is how many consecutive failures trip the breaker open.
	Failures int
	// Successes is how many consecutive probe successes close it again.
	Successes int
	// Cooldown is how long the breaker stays open before probing.
	Cooldown time.Duration
	// OnStateChange is called on every transition, for logs and metrics.
	// It runs while the breaker's lock is held: keep it short and do not
	// call back into the Breaker.
	OnStateChange func(from, to State)
}

func (c Config) withDefaults() Config {
	if c.Failures <= 0 {
		c.Failures = 5
	}
	if c.Successes <= 0 {
		c.Successes = 2
	}
	if c.Cooldown <= 0 {
		c.Cooldown = 10 * time.Second
	}
	return c
}

// Breaker is a circuit breaker. It is safe for concurrent use.
type Breaker struct {
	cfg Config
	now func() time.Time // overridable in tests

	mu        sync.Mutex
	state     State
	failures  int
	successes int
	openedAt  time.Time
	probing   bool // a half-open probe is in flight
}

// New creates a breaker.
func New(cfg Config) *Breaker {
	return &Breaker{cfg: cfg.withDefaults(), now: time.Now}
}

// Do runs fn under the breaker's protection. It returns ErrOpen without
// calling fn when the circuit is open; otherwise it returns fn's error and
// records the outcome.
func (b *Breaker) Do(ctx context.Context, fn func(context.Context) error) error {
	if !b.allow() {
		return ErrOpen
	}
	err := fn(ctx)
	b.record(err != nil)
	return err
}

// DoValue is Do for a function that returns a value.
func DoValue[T any](ctx context.Context, b *Breaker, fn func(context.Context) (T, error)) (T, error) {
	var zero T
	if !b.allow() {
		return zero, ErrOpen
	}
	v, err := fn(ctx)
	b.record(err != nil)
	return v, err
}

// State reports the current state.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// allow decides whether a call may proceed, moving the breaker out of the
// open state when the cooldown has elapsed.
func (b *Breaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case StateClosed:
		return true
	case StateOpen:
		if b.now().Sub(b.openedAt) < b.cfg.Cooldown {
			return false
		}
		b.transition(StateHalfOpen)
		b.probing = true
		return true
	default: // half-open
		// One probe at a time: a burst of probes against a service that is
		// still down is just the original outage again.
		if b.probing {
			return false
		}
		b.probing = true
		return true
	}
}

// record folds one call's outcome into the state machine.
func (b *Breaker) record(failed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probing = false

	switch b.state {
	case StateClosed:
		if !failed {
			b.failures = 0
			return
		}
		b.failures++
		if b.failures >= b.cfg.Failures {
			b.openedAt = b.now()
			b.transition(StateOpen)
		}
	case StateHalfOpen:
		if failed {
			b.failures = 0
			b.openedAt = b.now()
			b.transition(StateOpen)
			return
		}
		b.successes++
		if b.successes >= b.cfg.Successes {
			b.failures, b.successes = 0, 0
			b.transition(StateClosed)
		}
	case StateOpen:
		// A probe that was in flight when the breaker opened; its result is
		// stale, so it changes nothing.
	}
}

// transition moves to next and notifies. Callers hold the lock.
func (b *Breaker) transition(next State) {
	if b.state == next {
		return
	}
	from := b.state
	b.state = next
	if next == StateHalfOpen {
		b.successes = 0
	}
	if b.cfg.OnStateChange != nil {
		b.cfg.OnStateChange(from, next)
	}
}
