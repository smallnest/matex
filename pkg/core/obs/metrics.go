package obs

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics groups the framework-level Prometheus metrics.
type Metrics struct {
	registry     *prometheus.Registry
	HTTPDuration *prometheus.HistogramVec
	HTTPRequests *prometheus.CounterVec

	custom map[string]custom
}

// custom is a collector registered through Counter/Gauge/Histogram,
// remembered so a re-declaration returns the same handle instead of failing.
type custom struct {
	metric prometheus.Collector
}

// sameKind reports whether other is the same *Vec type as c, so a
// re-declaration is a duplicate and not a type clash.
func (c custom) sameKind(other prometheus.Collector) bool {
	switch c.metric.(type) {
	case *prometheus.CounterVec:
		_, ok := other.(*prometheus.CounterVec)
		return ok
	case *prometheus.GaugeVec:
		_, ok := other.(*prometheus.GaugeVec)
		return ok
	case *prometheus.HistogramVec:
		_, ok := other.(*prometheus.HistogramVec)
		return ok
	}
	return false
}

// NewMetrics creates a registry with Go/process collectors plus the
// framework HTTP metrics. The same handler is reused for /metrics.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	m := &Metrics{
		registry: reg,
		HTTPDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_server_request_duration_seconds",
			Help:    "HTTP request duration by route/method/code.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"route", "method", "code"}),
		HTTPRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_server_requests_total",
			Help: "HTTP requests by route/method/code.",
		}, []string{"route", "method", "code"}),
		custom: make(map[string]custom),
	}
	reg.MustRegister(m.HTTPDuration, m.HTTPRequests)
	return m
}

// Registry exposes the underlying registry (for custom metrics).
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// Handler returns the /metrics HTTP handler.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// Counter creates and registers a custom counter. Declaring the same name
// twice returns the first collector instead of panicking, so a library and
// the service can both declare it; declaring it again as another type is a
// real bug and panics.
func (m *Metrics) Counter(name, help string, labels ...string) *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, labels)
	if cv, ok := m.declare(c, name).(*prometheus.CounterVec); ok {
		return cv
	}
	panic(fmt.Sprintf("obs: metric %q already registered as a different type", name))
}

// Gauge creates and registers a custom gauge (re-declaring the same name is safe).
func (m *Metrics) Gauge(name, help string, labels ...string) *prometheus.GaugeVec {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels)
	if gv, ok := m.declare(g, name).(*prometheus.GaugeVec); ok {
		return gv
	}
	panic(fmt.Sprintf("obs: metric %q already registered as a different type", name))
}

// Histogram creates and registers a custom histogram (re-declaring the same
// name is safe). An empty buckets slice means prometheus.DefBuckets.
func (m *Metrics) Histogram(name, help string, buckets []float64, labels ...string) *prometheus.HistogramVec {
	if len(buckets) == 0 {
		buckets = prometheus.DefBuckets
	}
	h := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: name, Help: help, Buckets: buckets}, labels)
	if hv, ok := m.declare(h, name).(*prometheus.HistogramVec); ok {
		return hv
	}
	panic(fmt.Sprintf("obs: metric %q already registered as a different type", name))
}

// declare registers c under name. A second declaration of the same name and
// kind returns the first collector: prometheus would reject a re-declaration
// whose help or labels differ, and a service must not crash because two
// packages happened to declare one metric.
func (m *Metrics) declare(c prometheus.Collector, name string) prometheus.Collector {
	if prev, ok := m.custom[name]; ok {
		if prev.sameKind(c) {
			return prev.metric
		}
		panic(fmt.Sprintf("obs: metric %q already registered as a different type", name))
	}
	if err := m.registry.Register(c); err != nil {
		var are prometheus.AlreadyRegisteredError
		if errors.As(err, &are) {
			return are.ExistingCollector // registered by other means, identical collector
		}
		panic(err)
	}
	m.custom[name] = custom{metric: c}
	return c
}
