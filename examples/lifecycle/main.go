// Command lifecycle demonstrates the service lifecycle in pkg/core/verticle
// and pkg/core/app:
//
//   - Service = Name() + Setup(ctx, env) + BuildRouter(srv)
//   - env.Block: a background task, started with the app and stopped when
//     its context is cancelled
//   - env.AddCloser: cleanup, run in reverse registration order on shutdown
//   - ConfigWatcher: hot reload of the service section of config.yaml
//   - ReadyChecker: a custom /readyz gate (e.g. warm-up)
//   - SIGINT/SIGTERM → http drain → blocks → closers
//
// Run:
//
//	go run ./examples/lifecycle
//	curl -i localhost:8080/readyz                  # 前 3s 不就绪（ready_window）
//	curl -i localhost:8080/api/v1/greeting/world
//
// Then edit the `lifecycle:` section of examples/lifecycle/config.yaml
// (change greeting) and watch the log: the new value applies without a
// restart. Ctrl-C shows the shutdown order.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/smallnest/matex/pkg/core/config"
	"github.com/smallnest/matex/pkg/core/errs"
	"github.com/smallnest/matex/pkg/core/httpx"
	"github.com/smallnest/matex/pkg/core/obs"
	"github.com/smallnest/matex/pkg/core/verticle"
)

// lifecycleConfig is the `lifecycle:` section of config.yaml.
type lifecycleConfig struct {
	Greeting    string        `json:"greeting" default:"Hello, "`
	Heartbeat   time.Duration `json:"heartbeat" default:"2s"`
	ReadyWindow time.Duration `json:"ready_window" default:"0s"`
}

type lifecycleService struct {
	mu      sync.RWMutex
	cfg     lifecycleConfig
	started time.Time
}

func (s *lifecycleService) Name() string { return "lifecycle" }

func (s *lifecycleService) Setup(ctx context.Context, env *verticle.Env) error {
	if err := env.DecodeService(&s.cfg); err != nil {
		return err
	}
	s.started = time.Now()

	// A background task: it starts with the app and must return when ctx
	// is done (returning a non-nil error aborts the whole app).
	env.Block("heartbeat", func(ctx context.Context) error {
		t := time.NewTicker(s.heartbeat())
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				obs.Info(ctx, "heartbeat stopped")
				return nil
			case <-t.C:
				obs.Info(ctx, "heartbeat", "greeting", s.greeting())
			}
		}
	})

	// Cleanup. Closers run in reverse order, and infrastructure closers
	// registered by Run are further out, so: http drain → blocks →
	// your closers → infra close.
	env.AddCloser(func() {
		obs.Info(context.Background(), "service closer: flushing in-flight work")
	})

	obs.Info(ctx, "service configured",
		"greeting", s.cfg.Greeting, "heartbeat", s.cfg.Heartbeat, "ready_window", s.cfg.ReadyWindow)
	return nil
}

func (s *lifecycleService) BuildRouter(srv *httpx.Server) error {
	srv.Handle("GET", "/api/v1/greeting/{name}", s.greet)
	srv.Handle("GET", "/api/v1/uptime", s.uptime)
	return nil
}

func (s *lifecycleService) greet(ctx context.Context, r *http.Request) (any, error) {
	name := r.PathValue("name")
	if name == "" {
		return nil, errs.Invalid(40001, "name is required")
	}
	obs.Info(ctx, "greet", "name", name)
	return map[string]any{"greeting": s.greeting() + name + "!"}, nil
}

func (s *lifecycleService) uptime(context.Context, *http.Request) (any, error) {
	return map[string]any{"uptime": time.Since(s.started).Round(time.Second).String()}, nil
}

// OnServiceConfigChange is the hot-reload hook: the watcher calls it with
// the new service section after config.yaml changed (debounced).
func (s *lifecycleService) OnServiceConfigChange(raw map[string]any) {
	var cfg lifecycleConfig
	if err := config.ParseMap(raw, &cfg); err != nil {
		obs.Warn(context.Background(), "reload rejected", "err", err)
		return
	}
	s.mu.Lock()
	before := s.cfg
	s.cfg = cfg
	s.mu.Unlock()
	obs.Info(context.Background(), "config reloaded",
		"greeting_before", before.Greeting, "greeting_after", cfg.Greeting,
		"heartbeat", cfg.Heartbeat)
}

// Ready is the custom readiness gate. The default /readyz already pings
// every configured infra client; this adds a warm-up window on top.
func (s *lifecycleService) Ready(context.Context) error {
	s.mu.RLock()
	window := s.cfg.ReadyWindow
	s.mu.RUnlock()
	if window > 0 && time.Since(s.started) < window {
		return errs.Unavailable(50301, "warming up")
	}
	return nil
}

func (s *lifecycleService) greeting() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.Greeting
}

func (s *lifecycleService) heartbeat() time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.cfg.Heartbeat <= 0 {
		return 2 * time.Second
	}
	return s.cfg.Heartbeat
}

func main() {
	conf := flag.String("conf", "examples/lifecycle/config.yaml", "config file")
	flag.Parse()

	if err := verticle.Run(context.Background(), &lifecycleService{}, verticle.WithConf(*conf)); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}
