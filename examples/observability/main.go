// Command observability demonstrates pkg/core/obs:
//
//   - structured JSON logs on stdout, every line carries "service"
//   - a trace id taken from X-Request-ID (or generated) attached to every
//     log line via obs.Info(ctx, …) and returned in the response body
//   - Prometheus metrics: the framework's http_server_* plus custom
//     counters/gauges registered in Setup
//
// Run:
//
//	go run ./examples/observability
//
//	curl -i -H 'X-Request-ID: demo-1' localhost:8080/api/v1/work/alice
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

	// ctx carries the trace id injected by httpx, so every line is
	// correlatable; no need to pass a logger around.
	obs.Info(ctx, s.cfg.Prefix+" started", "name", name)

	value := len(name) * 2
	time.Sleep(5 * time.Millisecond)

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

func main() {
	conf := flag.String("conf", "examples/observability/config.yaml", "config file")
	flag.Parse()

	if err := verticle.Run(context.Background(), &obsService{}, verticle.WithConf(*conf)); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}
