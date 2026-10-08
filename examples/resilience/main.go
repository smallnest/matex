// Command resilience demonstrates the four protection tools in pkg/core,
// each wired into the httpx middleware slot or wrapped around an outgoing
// call:
//
//	ratelimit    bound how fast one caller may ask        (protects us)
//	idempotency  make a duplicate write harmless          (protects the data)
//	breaker      stop calling a dependency that is down   (protects it)
//	retry        absorb a transient failure               (hides the blip)
//
// Run:
//
//	go run ./examples/resilience
//
//	# rate limit: the third rapid POST is rejected with 429
//	for i in 1 2 3; do curl -s -o /dev/null -w '%{http_code} ' \
//	  -X POST localhost:8082/api/v1/login -d '{"user":"alice"}'; done; echo
//
//	# idempotency: both calls return the same payment id, only one is created
//	curl -s -X POST localhost:8082/api/v1/payments -H 'Idempotency-Key: k-1' -d '{"amount":10}'
//	curl -s -X POST localhost:8082/api/v1/payments -H 'Idempotency-Key: k-1' -d '{"amount":10}'
//
//	# retry + breaker: the flaky downstream recovers within the retry budget
//	curl -s localhost:8082/api/v1/flaky/1
//
//	go test ./examples/resilience
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/smallnest/matex/pkg/core/breaker"
	"github.com/smallnest/matex/pkg/core/errs"
	"github.com/smallnest/matex/pkg/core/httpx"
	"github.com/smallnest/matex/pkg/core/idempotency"
	"github.com/smallnest/matex/pkg/core/obs"
	"github.com/smallnest/matex/pkg/core/ratelimit"
	"github.com/smallnest/matex/pkg/core/retry"
	"github.com/smallnest/matex/pkg/core/verticle"
)

type resilienceConfig struct {
	LoginRate          float64 `json:"login_rate" default:"2"`
	LoginBurst         int     `json:"login_burst" default:"2"`
	DownstreamFailures int     `json:"downstream_failures" default:"10"`
}

type resilienceService struct {
	cfg        resilienceConfig
	downstream *fakeDownstream
	circuit    *breaker.Breaker
	payments   *paymentStore
}

func (s *resilienceService) Name() string { return "resilience" }

func (s *resilienceService) Setup(_ context.Context, env *verticle.Env) error {
	if err := env.DecodeService(&s.cfg); err != nil {
		return err
	}
	s.downstream = &fakeDownstream{failures: s.cfg.DownstreamFailures}
	s.payments = newPaymentStore()
	s.circuit = breaker.New(breaker.Config{
		// The trip threshold has to be at least the retry budget, or the
		// breaker opens in the middle of a retry loop and the last attempt
		// never gets a chance to succeed.
		Failures:  3,
		Successes: 1,
		Cooldown:  5 * time.Second,
		OnStateChange: func(from, to breaker.State) {
			obs.Info(context.Background(), "circuit state changed", "from", from.String(), "to", to.String())
		},
	})
	return nil
}

func (s *resilienceService) BuildRouter(srv *httpx.Server) error {
	// Use applies to every route registered after it, so the protections
	// stack in the order they are declared.
	srv.Use(ratelimit.Middleware(
		ratelimit.NewLocal(s.cfg.LoginRate, s.cfg.LoginBurst),
		ratelimit.ByIP,
	))
	srv.Handle("POST", "/api/v1/login", s.login)

	// Payments get the rate limit above plus idempotency. The memory store
	// suits a single instance; a fleet needs idempotency.NewRedis(env.Redis).
	srv.Use(idempotency.Middleware(idempotency.NewMemory(), idempotency.Config{}))
	srv.Handle("POST", "/api/v1/payments", s.pay)
	srv.Handle("GET", "/api/v1/flaky/{id}", s.flaky)

	return nil
}

func (s *resilienceService) login(_ context.Context, r *http.Request) (any, error) {
	var req struct {
		User string `json:"user"`
	}
	if err := httpx.ReadJSON(r, &req); err != nil {
		return nil, errs.Invalid(40001, "bad request body: %v", err)
	}
	if req.User == "" {
		return nil, errs.Invalid(40002, "user is required")
	}
	return map[string]any{"token": "token-for-" + req.User}, nil
}

func (s *resilienceService) pay(ctx context.Context, r *http.Request) (any, error) {
	var req struct {
		Amount int `json:"amount"`
	}
	if err := httpx.ReadJSON(r, &req); err != nil {
		return nil, errs.Invalid(40001, "bad request body: %v", err)
	}
	if req.Amount <= 0 {
		return nil, errs.Invalid(40003, "amount must be positive")
	}
	id, err := s.payments.create(ctx, req.Amount)
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "amount": req.Amount}, nil
}

// flaky calls a dependency that is having a bad time. Retry sits outside
// the breaker: each attempt passes through the circuit, and once the
// circuit is open the retry loop stops instead of hammering.
func (s *resilienceService) flaky(ctx context.Context, r *http.Request) (any, error) {
	payload, err := retry.DoValue(ctx, retry.Config{
		Attempts:  3,
		BaseDelay: 5 * time.Millisecond,
		Retryable: func(err error) bool {
			// An open circuit will not close inside a retry loop.
			return !errors.Is(err, breaker.ErrOpen)
		},
	}, func(ctx context.Context) (string, error) {
		return breaker.DoValue(ctx, s.circuit, func(context.Context) (string, error) {
			return s.downstream.call()
		})
	})

	switch {
	case errors.Is(err, breaker.ErrOpen):
		return nil, errs.Unavailable(50302, "downstream circuit is open")
	case err != nil:
		return nil, errs.Internal(50010, "downstream failed: %v", err)
	}
	return map[string]any{
		"id":               r.PathValue("id"),
		"payload":          payload,
		"downstream_calls": s.downstream.calls(),
		"circuit":          s.circuit.State().String(),
	}, nil
}

// fakeDownstream stands in for a remote service: it fails the first N calls
// and then recovers, which is enough to show retry succeeding and the
// breaker opening when the failures outlast the retry budget.
type fakeDownstream struct {
	mu       sync.Mutex
	failures int
	n        int
}

func (d *fakeDownstream) call() (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.n++
	if d.failures > 0 {
		d.failures--
		return "", errors.New("connection reset by peer")
	}
	return "downstream payload", nil
}

func (d *fakeDownstream) calls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.n
}

// paymentStore allocates payment ids. Every call creates a real payment, so
// the count doubling is exactly the damage a failed idempotency key would
// do — the tests assert on it.
type paymentStore struct {
	mu   sync.Mutex
	next int
}

func newPaymentStore() *paymentStore { return &paymentStore{} }

func (p *paymentStore) create(_ context.Context, _ int) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.next++
	return fmt.Sprintf("pay-%d", p.next), nil
}

func main() {
	conf := flag.String("conf", "examples/resilience/config.yaml", "config file")
	flag.Parse()

	if err := verticle.Run(context.Background(), &resilienceService{}, verticle.WithConf(*conf)); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}
