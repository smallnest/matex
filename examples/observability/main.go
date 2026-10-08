// Command observability demonstrates pkg/core/obs:
//
//   - structured JSON logs on stdout, every line carries "service"
//   - a correlation id attached to every log line via obs.Info(ctx, …) and
//     returned in the response body: the active span's trace id when
//     tracing is on, the X-Request-ID otherwise
//   - Prometheus metrics: the framework's http_server_* plus custom
//     counters/gauges registered in Setup
//   - OpenTelemetry tracing (opt-in via the `trace` config section): each
//     request becomes a server span that joins the caller's trace, and
//     obs.StartSpan adds child spans for the expensive parts
//
// Run:
//
//	go run ./examples/observability
//
//	curl -i -H 'X-Request-ID: demo-1' localhost:8080/api/v1/work/alice
//	curl -i -H 'traceparent: 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01' \
//	  localhost:8080/api/v1/work/alice      # needs trace.enabled: true
//	curl -s localhost:8080/metrics | grep -E '^(observe_|http_server_)'
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/smallnest/matex/pkg/core/httpx"
	"github.com/smallnest/matex/pkg/core/obs"
	"github.com/smallnest/matex/pkg/core/verticle"
)

type obsConfig struct {
	Prefix string `json:"prefix" default:"work"`
}

type obsService struct {
	cfg   obsConfig
	work  *prometheus.CounterVec
	value *prometheus.GaugeVec
}

func (s *obsService) Name() string { return "observe" }

// Setup registers the custom metrics. env.Metrics is created before Setup
// (and the same registry backs GET /metrics), so a service can declare its
// own collectors here.
func (s *obsService) Setup(ctx context.Context, env *verticle.Env) error {
	if err := env.DecodeService(&s.cfg); err != nil {
		return err
	}
	s.work = env.Metrics.Counter("observe_work_total", "processed calls", "name")
	s.value = env.Metrics.Gauge("observe_last_value", "last computed value", "name")
	obs.Info(ctx, "custom metrics registered", "prefix", s.cfg.Prefix)
	return nil
}

func (s *obsService) BuildRouter(srv *httpx.Server) error {
	srv.Handle("GET", "/api/v1/work/{name}", s.workHandler)
	return nil
}

func (s *obsService) workHandler(ctx context.Context, r *http.Request) (any, error) {
	name := r.PathValue("name")
	start := time.Now()

	// ctx carries the correlation id injected by httpx, so every line is
	// correlatable; no need to pass a logger around.
	obs.Info(ctx, s.cfg.Prefix+" started", "name", name)

	value := s.compute(ctx, name)

	s.work.WithLabelValues(name).Inc()
	s.value.WithLabelValues(name).Set(float64(value))

	obs.Info(ctx, s.cfg.Prefix+" done",
		"name", name, "value", value, "dur_ms", time.Since(start).Milliseconds())

	return map[string]any{
		"name":     name,
		"value":    value,
		"trace_id": obs.TraceID(ctx),
	}, nil
}

// compute wraps the expensive step in its own span, nested under the
// request's server span. obs.StartSpan is a no-op when tracing is off, so
// the instrumentation stays unconditional — no `if tracing` branches.
func (s *obsService) compute(ctx context.Context, name string) int {
	ctx, span := obs.StartSpan(ctx, "compute")
	defer span.End()

	time.Sleep(5 * time.Millisecond)
	obs.Debug(ctx, "computed", "name", name)
	return len(name) * 2
}

func main() {
	conf := flag.String("conf", "examples/observability/config.yaml", "config file")
	flag.Parse()

	if err := verticle.Run(context.Background(), &obsService{}, verticle.WithConf(*conf)); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}
