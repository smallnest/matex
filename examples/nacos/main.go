// Command nacos demonstrates the Nacos config center with verticle:
//
//   - the `nacos:` section of config.yaml switches the center on; without
//     it (and without WithConfig) the file is the only source
//   - a framework DataID (matex.yaml) overlays http/log/db/... over the file
//   - a service DataID (demo.yaml) owns the `demo:` section and applies live
//     through ConfigWatcher, exactly like a file edit
//
// Data must live in the demo namespace, group DEMO:
//
//	matex.yaml   http: {addr: ":8080"}      log: {level: info}
//	demo.yaml    greeting: "Hello from Nacos, "
//
// Run (needs a Nacos server; examples/nacos/config.yaml points at
// localhost:8848):
//
//	go run ./examples/nacos
//	curl -i localhost:8080/api/v1/greeting/world
//	# edit demo.yaml in the Nacos console -> the greeting changes in place
//
// The runnable test (main_test.go) needs no server: it uses a fake source.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sync"

	"github.com/smallnest/matex/pkg/core/config"
	"github.com/smallnest/matex/pkg/core/errs"
	"github.com/smallnest/matex/pkg/core/httpx"
	"github.com/smallnest/matex/pkg/core/obs"
	"github.com/smallnest/matex/pkg/core/verticle"
)

// demoConfig is the `demo:` section: the service DataID on Nacos.
type demoConfig struct {
	Greeting string `json:"greeting" default:"Hello, "`
}

type nacosService struct {
	mu       sync.RWMutex
	greeting string // the section field, copied out under mu
	version  int    // bumped on every reload, so a reload is observable
}

func (s *nacosService) Name() string { return "demo" }

func (s *nacosService) Setup(ctx context.Context, env *verticle.Env) error {
	var cfg demoConfig
	if err := env.DecodeService(&cfg); err != nil {
		return err
	}
	s.mu.Lock()
	s.greeting = cfg.Greeting
	s.mu.Unlock()
	obs.Info(ctx, "configured", "greeting", cfg.Greeting)
	return nil
}

func (s *nacosService) BuildRouter(srv *httpx.Server) error {
	srv.Handle("GET", "/api/v1/greeting/{name}", s.greet)
	return nil
}

func (s *nacosService) greet(ctx context.Context, r *http.Request) (any, error) {
	name := r.PathValue("name")
	if name == "" {
		return nil, errs.Invalid(40001, "name is required")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return map[string]any{
		"greeting": s.greeting + name + "!",
		"version":  s.version,
	}, nil
}

// OnServiceConfigChange applies the center's service DataID live. A bad
// value keeps the previous config, so a mistyped edit cannot take the
// service down.
//
// The decode is strict: the center's section is plain data, so a misspelled
// key (say "gretting") would otherwise decode to the zero value and the
// service would run on a blank greeting without any error.
func (s *nacosService) OnServiceConfigChange(raw map[string]any) {
	var cfg demoConfig
	if unknown := config.UnknownKeys(raw, &cfg); len(unknown) > 0 {
		obs.Warn(context.Background(), "reload rejected: unknown keys", "keys", unknown)
		return
	}
	if err := config.ParseMap(raw, &cfg); err != nil {
		obs.Warn(context.Background(), "reload rejected", "err", err)
		return
	}
	s.mu.Lock()
	s.greeting = cfg.Greeting
	s.version++
	v := s.version
	s.mu.Unlock()
	obs.Info(context.Background(), "config reloaded from the center", "greeting", cfg.Greeting, "version", v)
}

func main() {
	conf := flag.String("conf", "examples/nacos/config.yaml", "config file")
	flag.Parse()

	if err := verticle.Run(context.Background(), &nacosService{}, verticle.WithConf(*conf)); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}
