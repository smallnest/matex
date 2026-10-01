package obs

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"net/http"
)

// Metrics groups the framework-level Prometheus metrics.
type Metrics struct {
	registry     *prometheus.Registry
	HTTPDuration *prometheus.HistogramVec
	HTTPRequests *prometheus.CounterVec
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

// Counter creates and registers a custom counter.
func (m *Metrics) Counter(name, help string, labels ...string) *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, labels)
	m.registry.MustRegister(c)
	return c
}

// Gauge creates and registers a custom gauge.
func (m *Metrics) Gauge(name, help string, labels ...string) *prometheus.GaugeVec {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels)
	m.registry.MustRegister(g)
	return g
}

// Histogram creates and registers a custom histogram.
func (m *Metrics) Histogram(name, help string, buckets []float64, labels ...string) *prometheus.HistogramVec {
	if len(buckets) == 0 {
		buckets = prometheus.DefBuckets
	}
	h := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: name, Help: help, Buckets: buckets}, labels)
	m.registry.MustRegister(h)
	return h
}
