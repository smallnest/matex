package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/smallnest/matex/pkg/core/httpx"
	"github.com/smallnest/matex/pkg/core/obs"
	"github.com/smallnest/matex/pkg/core/verticle"
)

// Setup only needs env.Metrics; a zero Env is enough (its service section
// is empty, and every config field has a default).
func newTestService(t *testing.T) (*obsService, *verticle.Env) {
	t.Helper()
	env := &verticle.Env{Metrics: obs.NewMetrics()}
	svc := &obsService{}
	if err := svc.Setup(t.Context(), env); err != nil {
		t.Fatalf("setup: %v", err)
	}
	return svc, env
}

func TestWorkHandlerCarriesTraceID(t *testing.T) {
	svc, _ := newTestService(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/work/alice", nil)
	req.SetPathValue("name", "alice")
	ctx := obs.WithTraceID(req.Context(), "trace-1")

	data, err := svc.workHandler(ctx, req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	got, ok := data.(map[string]any)
	if !ok {
		t.Fatalf("data = %T", data)
	}
	if got["trace_id"] != "trace-1" {
		t.Errorf("trace_id = %v, want trace-1", got["trace_id"])
	}
	if got["value"] != 10 { // len("alice") * 2
		t.Errorf("value = %v, want 10", got["value"])
	}
}

func TestCustomMetricsAreExported(t *testing.T) {
	svc, env := newTestService(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/work/bob", nil)
	req.SetPathValue("name", "bob")
	if _, err := svc.workHandler(req.Context(), req); err != nil {
		t.Fatal(err)
	}

	families, err := env.Metrics.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var found bool
	for _, mf := range families {
		if mf.GetName() != "observe_work_total" {
			continue
		}
		found = true
		if n := len(mf.GetMetric()); n != 1 {
			t.Fatalf("observe_work_total has %d series, want 1", n)
		}
		if v := mf.GetMetric()[0].GetCounter().GetValue(); v != 1 {
			t.Errorf("counter = %v, want 1", v)
		}
	}
	if !found {
		t.Error("observe_work_total is not exported on the registry")
	}
}

func TestFrameworkMetricsExist(t *testing.T) {
	_, env := newTestService(t)
	families, err := env.Metrics.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, mf := range families {
		names[mf.GetName()] = true
	}
	if !names["go_goroutines"] {
		t.Error("expected the Go collector to be registered")
	}
}

// TestTracingJoinsTheCallersTrace: with tracing on, an inbound traceparent
// decides the trace id the service reports, and the handler's own spans
// nest under it. This is what makes logs from two services line up.
func TestTracingJoinsTheCallersTrace(t *testing.T) {
	prevPropagator := otel.GetTextMapPropagator()
	shutdown, err := obs.InitTracing(context.Background(), "observe", obs.TraceConfig{Enabled: true})
	if err != nil {
		t.Fatalf("init tracing: %v", err)
	}
	t.Cleanup(func() {
		_ = shutdown(context.Background())
		if _, err := obs.InitTracing(context.Background(), "observe", obs.TraceConfig{}); err != nil {
			t.Errorf("reset tracing: %v", err)
		}
		otel.SetTextMapPropagator(prevPropagator)
	})

	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	svc, _ := newTestService(t)
	srv := httpx.New(httpx.Config{Timeout: time.Second})
	if err := svc.BuildRouter(srv); err != nil {
		t.Fatalf("build router: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/work/alice", nil)
	req.Header.Set("traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Request-ID"); got != traceID {
		t.Fatalf("X-Request-ID = %q, want %q", got, traceID)
	}
	if !strings.Contains(rec.Body.String(), traceID) {
		t.Fatalf("handler reported a different trace id: %s", rec.Body.String())
	}
}

// TestTracingOffKeepsTheOldContract: the default (no `trace` section) must
// behave exactly as before tracing existed.
func TestTracingOffKeepsTheOldContract(t *testing.T) {
	if _, err := obs.InitTracing(context.Background(), "observe", obs.TraceConfig{}); err != nil {
		t.Fatal(err)
	}
	svc, _ := newTestService(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/work/alice", nil)
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	req.SetPathValue("name", "alice")

	data, err := svc.workHandler(req.Context(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	got := data.(map[string]any)
	if got["trace_id"] == "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatal("tracing is off; the inbound traceparent must not become the trace id")
	}
}
