package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

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
