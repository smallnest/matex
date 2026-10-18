package obs

import (
	"testing"
)

// TestMetricsDuplicateRegistrationDoesNotPanic pins the idempotent-registration
// contract: two pieces of code declaring the same metric name get the same
// collector back instead of a MustRegister panic.
func TestMetricsDuplicateRegistrationDoesNotPanic(t *testing.T) {
	m := NewMetrics()
	a := m.Counter("api_requests_total", "requests", "method")
	b := m.Counter("api_requests_total", "requests (again)", "method")
	if a != b {
		t.Fatal("duplicate Counter did not return the already-registered collector")
	}
	// The value actually records, so callers never operate on a dead vector.
	a.WithLabelValues("get").Inc()

	g1 := m.Gauge("api_active", "active")
	g2 := m.Gauge("api_active", "active again")
	if g1 != g2 {
		t.Fatal("duplicate Gauge did not return the already-registered collector")
	}

	h1 := m.Histogram("api_duration_seconds", "duration", nil)
	h2 := m.Histogram("api_duration_seconds", "duration again", nil)
	if h1 != h2 {
		t.Fatal("duplicate Histogram did not return the already-registered collector")
	}
}
