package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/smallnest/matex/pkg/core/httpx"
	"github.com/smallnest/matex/pkg/core/obs"
	"github.com/smallnest/matex/pkg/core/verticle"
)

// newTestService runs Setup (which applies the config defaults) and then
// lets the test override the config, so the rate limiter can be pushed out
// of the way for tests that are not about rate limiting.
func newTestService(t *testing.T, cfg *resilienceConfig, downstreamFailures int) *resilienceService {
	t.Helper()
	svc := &resilienceService{}
	if err := svc.Setup(t.Context(), &verticle.Env{Metrics: obs.NewMetrics()}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if cfg != nil {
		svc.cfg = *cfg
	}
	svc.downstream = &fakeDownstream{failures: downstreamFailures}
	return svc
}

// unlimited keeps the rate limiter from interfering with other tests.
func unlimited(failures int) *resilienceConfig {
	return &resilienceConfig{LoginRate: 1e6, LoginBurst: 1e6, DownstreamFailures: failures}
}

func newTestHandler(t *testing.T, svc *resilienceService) http.Handler {
	t.Helper()
	srv := httpx.New(httpx.Config{Timeout: 2 * time.Second, MaxBody: 1 << 20})
	if err := svc.BuildRouter(srv); err != nil {
		t.Fatalf("build router: %v", err)
	}
	return srv.Handler()
}

func do(h http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func dataOf(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return env.Data
}

// TestRateLimitRejectsBurst: one client, burst 2 — the third rapid request
// is turned away with 429 before it reaches the handler.
func TestRateLimitRejectsBurst(t *testing.T) {
	svc := newTestService(t, nil, 0) // config default: 2/s, burst 2
	h := newTestHandler(t, svc)

	statuses := make([]int, 0, 3)
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/login", strings.NewReader(`{"user":"alice"}`))
		req.RemoteAddr = "10.0.0.1:1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		statuses = append(statuses, rec.Code)
	}

	want := []int{http.StatusOK, http.StatusOK, http.StatusTooManyRequests}
	for i, w := range want {
		if statuses[i] != w {
			t.Fatalf("request %d: code %d, want %d (all: %v)", i+1, statuses[i], w, statuses)
		}
	}
}

func TestRateLimitIsPerClient(t *testing.T) {
	svc := newTestService(t, nil, 0)
	h := newTestHandler(t, svc)

	call := func(addr string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/login", strings.NewReader(`{"user":"alice"}`))
		req.RemoteAddr = addr
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	if got := call("10.0.0.1:1"); got != http.StatusOK {
		t.Fatalf("first call: code %d", got)
	}
	if got := call("10.0.0.1:2"); got != http.StatusOK {
		t.Fatalf("second call: code %d; the burst is 2", got)
	}
	if got := call("10.0.0.1:3"); got != http.StatusTooManyRequests {
		t.Fatalf("third call from the same IP: code %d, want 429", got)
	}
	// A different client still has its own budget.
	if got := call("10.0.0.2:1"); got != http.StatusOK {
		t.Fatalf("a different client got %d; limits must be per key", got)
	}
}

func TestIdempotentPayments(t *testing.T) {
	svc := newTestService(t, unlimited(0), 0)
	h := newTestHandler(t, svc)

	first := do(h, http.MethodPost, "/api/v1/payments", `{"amount":10}`,
		map[string]string{"Idempotency-Key": "k-1"})
	if first.Code != http.StatusOK {
		t.Fatalf("first: code %d body %s", first.Code, first.Body.String())
	}

	// The client retries with the same key: same answer, no second payment.
	second := do(h, http.MethodPost, "/api/v1/payments", `{"amount":10}`,
		map[string]string{"Idempotency-Key": "k-1"})
	if second.Code != http.StatusOK {
		t.Fatalf("replay: code %d body %s", second.Code, second.Body.String())
	}
	if got, want := dataOf(t, second)["id"], dataOf(t, first)["id"]; got != want {
		t.Fatalf("replayed id = %v, want the original %v", got, want)
	}
	if svc.payments.next != 1 {
		t.Fatalf("created %d payments; the duplicate reached the store", svc.payments.next)
	}

	// A new key is a new payment.
	third := do(h, http.MethodPost, "/api/v1/payments", `{"amount":20}`,
		map[string]string{"Idempotency-Key": "k-2"})
	if got := dataOf(t, third)["id"]; got == dataOf(t, first)["id"] {
		t.Fatalf("a different key reused id %v", got)
	}
	if svc.payments.next != 2 {
		t.Fatalf("created %d payments, want 2", svc.payments.next)
	}
}

func TestPaymentWithoutKeyIsNotDeduplicated(t *testing.T) {
	svc := newTestService(t, unlimited(0), 0)
	h := newTestHandler(t, svc)

	do(h, http.MethodPost, "/api/v1/payments", `{"amount":10}`, nil)
	do(h, http.MethodPost, "/api/v1/payments", `{"amount":10}`, nil)
	if svc.payments.next != 2 {
		t.Fatalf("created %d payments, want 2 — no key means no deduplication", svc.payments.next)
	}
}

// TestRetryAbsorbsATransientFailure: the downstream fails twice and recovers
// on the third attempt, which the retry budget covers.
func TestRetryAbsorbsATransientFailure(t *testing.T) {
	svc := newTestService(t, unlimited(2), 2)
	h := newTestHandler(t, svc)

	rec := do(h, http.MethodGet, "/api/v1/flaky/7", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
	}
	data := dataOf(t, rec)
	if data["downstream_calls"] != float64(3) {
		t.Fatalf("downstream_calls = %v, want 3 (two failures, one success)", data["downstream_calls"])
	}
	if data["circuit"] != "closed" {
		t.Fatalf("circuit = %v, want closed after a success", data["circuit"])
	}
}

// TestBreakerOpensAfterRetriesAreExhausted: once failures outlast the retry
// budget the circuit opens, and the next request fails fast instead of
// queueing three more doomed attempts.
func TestBreakerOpensAfterRetriesAreExhausted(t *testing.T) {
	svc := newTestService(t, unlimited(100), 100)
	h := newTestHandler(t, svc)

	first := do(h, http.MethodGet, "/api/v1/flaky/1", "", nil)
	if first.Code != http.StatusInternalServerError {
		t.Fatalf("first: code %d body %s", first.Code, first.Body.String())
	}
	callsAfterFirst := svc.downstream.calls()
	if callsAfterFirst != 3 {
		t.Fatalf("downstream called %d times, want 3 (the whole retry budget)", callsAfterFirst)
	}

	second := do(h, http.MethodGet, "/api/v1/flaky/2", "", nil)
	if second.Code != http.StatusServiceUnavailable {
		t.Fatalf("second: code %d body %s; the circuit should be open", second.Code, second.Body.String())
	}
	if !strings.Contains(second.Body.String(), `"code":50302`) {
		t.Fatalf("body %s missing the open-circuit code", second.Body.String())
	}
	if got := svc.downstream.calls(); got != callsAfterFirst {
		t.Fatalf("downstream called %d times while the circuit was open, want %d", got, callsAfterFirst)
	}
}

func TestValidationStillApplies(t *testing.T) {
	svc := newTestService(t, unlimited(0), 0)
	h := newTestHandler(t, svc)

	rec := do(h, http.MethodPost, "/api/v1/payments", `{"amount":0}`,
		map[string]string{"Idempotency-Key": "k-1"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
	}
	// A deterministic rejection is safe to replay.
	again := do(h, http.MethodPost, "/api/v1/payments", `{"amount":0}`,
		map[string]string{"Idempotency-Key": "k-1"})
	if again.Code != http.StatusBadRequest {
		t.Fatalf("replay: code %d", again.Code)
	}
}

// TestConfigDefaults proves the service's config tags are wired up.
func TestConfigDefaults(t *testing.T) {
	svc := newTestService(t, nil, 0)
	if svc.cfg.LoginRate != 2 || svc.cfg.LoginBurst != 2 {
		t.Fatalf("rate defaults not applied: %+v", svc.cfg)
	}
	if svc.cfg.DownstreamFailures != 10 {
		t.Fatalf("downstream_failures default not applied: %+v", svc.cfg)
	}
}
