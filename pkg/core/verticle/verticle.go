// Package verticle defines the matex service abstraction: a deployment
// unit is a Service with a name, a setup phase and a router phase. Run
// drives the whole lifecycle.
//
// The name is load-bearing: it prefixes logs and metrics, and it
// selects the service-specific section of the config file (a section
// whose key equals the lowercased name).
//
// Run sequence (fail-fast on every step):
//
//  1. load config.yaml (path from WithConf / CONFIG_FILE / default)
//  2. obs.Init(name, level)            — logging comes first
//  3. init infra (db/redis/memcache/kafka producer — only the sections
//     that are configured; the rest stay nil)
//  4. svc.Setup(ctx, env)              — service-specific state
//  5. httpx server + /healthz /readyz /metrics
//  6. svc.BuildRouter(srv)             — register routes
//  7. serve; watch the config file and notify svc of service-section
//     changes (ConfigWatcher)
//  8. SIGINT/SIGTERM → graceful shutdown (env blocks → infra close)
//
// # Example
//
//	type DemoService struct{ hello *helloworld.Service }
//
//	func (s *DemoService) Name() string { return "demo" }
//	func (s *DemoService) Setup(ctx context.Context, env *verticle.Env) error {
//	    var cfg DemoConfig
//	    if err := env.DecodeService(&cfg); err != nil { return err }
//	    s.hello = helloworld.New(helloworld.Deps{DB: env.DB, Redis: env.Redis, ...})
//	    return nil
//	}
//	func (s *DemoService) BuildRouter(s *httpx.Server) error {
//	    h := helloworld.NewHandler(s.hello) // parameter shadowing kept simple
//	    s.Handle("GET", "/api/v1/hello/{name}", h.Greet)
//	    return nil
//	}
//
//	func main() { verticle.Run(ctx, &DemoService{}) }
package verticle

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	rpcxclient "github.com/smallnest/rpcx/client"
	rpcxserver "github.com/smallnest/rpcx/server"
	"google.golang.org/grpc"

	"github.com/smallnest/matex/pkg/core/app"
	"github.com/smallnest/matex/pkg/core/config"
	"github.com/smallnest/matex/pkg/core/db"
	"github.com/smallnest/matex/pkg/core/grpcx"
	"github.com/smallnest/matex/pkg/core/httpx"
	"github.com/smallnest/matex/pkg/core/kafka"
	"github.com/smallnest/matex/pkg/core/memcache"
	"github.com/smallnest/matex/pkg/core/obs"
	"github.com/smallnest/matex/pkg/core/redis"
	"github.com/smallnest/matex/pkg/core/rpcx"
)

// Service is the deployment-unit contract.
type Service interface {
	// Name returns the service name: config section key, log prefix,
	// metrics label. Keep it stable and lowercase.
	Name() string

	// Setup initializes service state. Infra that was configured is
	// already available on env (nil when the section was absent).
	Setup(ctx context.Context, env *Env) error

	// BuildRouter registers routes on the HTTP server.
	BuildRouter(s *httpx.Server) error
}

// ConfigWatcher is the optional hot-reload hook.
type ConfigWatcher interface {
	// OnServiceConfigChange is called with the new service section
	// after the config file changed. Decode it with
	// config.ParseMap(raw, &cfg).
	OnServiceConfigChange(raw map[string]any)
}

// ReadyChecker is the optional readiness override. The default ready
// check pings every initialized infra client.
type ReadyChecker interface {
	Ready(ctx context.Context) error
}

// GRPCRegistrar is implemented by services that expose gRPC endpoints.
// Run creates the gRPC server (when the `grpc` section is configured)
// and calls RegisterGRPC before serving. Register your generated
// service impls here, e.g. pb.RegisterGreeterServer(s, &greeter{}).
type GRPCRegistrar interface {
	RegisterGRPC(s *grpc.Server)
}

// RPCXRegistrar is implemented by services that expose rpcx endpoints.
// Run creates the rpcx server (when the `rpcx` section is configured)
// and calls RegisterRPCX before serving, e.g.
// s.RegisterName("Greeter", &greeter{}, "").
type RPCXRegistrar interface {
	RegisterRPCX(s *rpcxserver.Server) error
}

// FrameworkConfig is the shared (non-service) part of config.yaml.
type FrameworkConfig struct {
	HTTP     httpx.Config    `json:"http"`
	GRPC     grpcx.Config    `json:"grpc" optional:""`
	RPCX     rpcx.Config     `json:"rpcx" optional:""`
	Log      obs.LogConfig   `json:"log"`
	DB       db.Config       `json:"db" optional:""`
	Redis    redis.Config    `json:"redis" optional:""`
	Memcache memcache.Config `json:"memcache" optional:""`
	Kafka    kafka.Config    `json:"kafka" optional:""`
}

// Env carries the initialized infrastructure and service utilities
// into Setup. Fields are nil when the config section was absent —
// code defensively (`if env.DB != nil`).
type Env struct {
	Name     string
	ConfPath string

	DB       *db.DB
	Redis    *redis.Client
	Memcache *memcache.Client
	Kafka    *kafka.Producer
	KafkaCfg kafka.Config
	GRPCCfg  grpcx.Config
	RPCXCfg  rpcx.Config
	Metrics  *obs.Metrics

	section envSection
	blocks  []block
	closers []func()
}

type block struct {
	name string
	fn   func(ctx context.Context) error
}

// Block registers a background task for the service (e.g. a kafka
// consumer loop). See app.App.Block.
func (e *Env) Block(name string, fn func(ctx context.Context) error) {
	e.blocks = append(e.blocks, block{name: name, fn: fn})
}

// AddCloser registers cleanup for the service (reverse order at
// shutdown). Infrastructure closers are added by Run itself.
func (e *Env) AddCloser(f func()) { e.closers = append(e.closers, f) }

// DecodeService decodes the service section of config.yaml into v
// (same tag semantics as config.Load).
func (e *Env) DecodeService(v any) error {
	return config.ParseMap(e.section.get(), v)
}

// GRPCClient dials a peer gRPC service and registers the connection for
// shutdown cleanup. Call it in Setup to initialize clients; target is a
// host:port or a K8s Service DNS name. The connection is fail-fast: it
// is verified READY within grpc.dial_timeout before returning.
func (e *Env) GRPCClient(ctx context.Context, target string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	timeout := e.GRPCCfg.DialTimeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := grpcx.Dial(ctx, target, opts...)
	if err != nil {
		return nil, err
	}
	e.AddCloser(func() { _ = conn.Close() })
	return conn, nil
}

// RPCXClient creates a peer-to-peer rpcx client and registers it for
// shutdown cleanup. target is "network@host:port" or "host:port" (tcp
// default). Call it in Setup to initialize clients.
func (e *Env) RPCXClient(servicePath, target string) (rpcxclient.XClient, error) {
	c, err := rpcx.PeerClient(servicePath, target)
	if err != nil {
		return nil, err
	}
	e.AddCloser(func() { _ = c.Close() })
	return c, nil
}

// RPCXClientWith creates an rpcx client with a custom discovery (etcd,
// consul, multiple-server, ...) and registers it for shutdown cleanup.
func (e *Env) RPCXClientWith(servicePath string, d rpcxclient.ServiceDiscovery) rpcxclient.XClient {
	c := rpcx.NewClient(servicePath, d)
	e.AddCloser(func() { _ = c.Close() })
	return c
}

// setServiceSection swaps the service section (called by the watcher).
func (e *Env) setServiceSection(m map[string]any) { e.section.set(m) }

type options struct {
	confPath string
	noWatch  bool
}

// Option customizes Run.
type Option func(*options)

// WithConf pins the config path (falls back to CONFIG_FILE env and
// then configs/config.yaml).
func WithConf(path string) Option {
	return func(o *options) {
		if path != "" {
			o.confPath = path
		}
	}
}

// WithoutConfigWatch disables the config-file hot reload.
func WithoutConfigWatch() Option { return func(o *options) { o.noWatch = true } }

// Run drives the whole service lifecycle. It blocks until shutdown.
func Run(ctx context.Context, svc Service, opts ...Option) error {
	var o options
	for _, fn := range opts {
		fn(&o)
	}
	confPath := o.confPath
	if confPath == "" {
		if env := os.Getenv("CONFIG_FILE"); env != "" {
			confPath = env
		} else {
			confPath = "configs/config.yaml"
		}
	}

	// Step 1: config
	raw, err := config.LoadMap(confPath)
	if err != nil {
		return fmt.Errorf("verticle: load config: %w", err)
	}
	var fc FrameworkConfig
	if err := config.ParseMap(raw, &fc); err != nil {
		return fmt.Errorf("verticle: parse framework config: %w", err)
	}
	serviceRaw, _ := raw[strings.ToLower(svc.Name())].(map[string]any)

	// Step 2: logging first
	if err := obs.Init(svc.Name(), fc.Log.Level); err != nil {
		return fmt.Errorf("verticle: init logging: %w", err)
	}

	env := &Env{
		Name:     svc.Name(),
		ConfPath: confPath,
		KafkaCfg: fc.Kafka,
		GRPCCfg:  fc.GRPC,
		RPCXCfg:  fc.RPCX,
	}
	env.setServiceSection(serviceRaw)

	// Step 3: infrastructure (fail-fast; only configured sections)
	if fc.DB.DSN != "" {
		p, err := db.Open(ctx, fc.DB)
		if err != nil {
			return fmt.Errorf("verticle: init db: %w", err)
		}
		env.DB = p
		obs.Info(ctx, "db initialized")
	}
	if fc.Redis.Addr != "" {
		rc, err := redis.Open(fc.Redis)
		if err != nil {
			return fmt.Errorf("verticle: init redis: %w", err)
		}
		env.Redis = rc
		obs.Info(ctx, "redis initialized", "addr", fc.Redis.Addr)
	}
	if len(fc.Memcache.Addrs) > 0 {
		env.Memcache = memcache.Open(fc.Memcache)
		obs.Info(ctx, "memcache initialized", "addrs", fc.Memcache.Addrs)
	}
	if len(fc.Kafka.Brokers) > 0 {
		p, err := kafka.NewProducer(fc.Kafka)
		if err != nil {
			return fmt.Errorf("verticle: init kafka: %w", err)
		}
		env.Kafka = p
		obs.Info(ctx, "kafka producer initialized", "brokers", fc.Kafka.Brokers)
	}

	// Step 4: service setup
	if err := svc.Setup(ctx, env); err != nil {
		return fmt.Errorf("verticle: setup %s: %w", svc.Name(), err)
	}

	// Step 5/6: http server + routes
	env.Metrics = obs.NewMetrics()
	srv := httpx.New(fc.HTTP,
		httpx.WithMetrics(env.Metrics),
		httpx.WithLogger(slog.Default().With("service", svc.Name())),
		httpx.WithReadyCheck(makeReady(svc, env)),
	)
	if err := svc.BuildRouter(srv); err != nil {
		return fmt.Errorf("verticle: build router %s: %w", svc.Name(), err)
	}

	// Step 6b: gRPC server + registration (optional)
	var grpcSrv *grpcx.Server
	if fc.GRPC.Addr != "" {
		grpcSrv, err = grpcx.New(fc.GRPC)
		if err != nil {
			return fmt.Errorf("verticle: init grpc: %w", err)
		}
		if r, ok := svc.(GRPCRegistrar); ok {
			r.RegisterGRPC(grpcSrv.GRPC())
			obs.Info(ctx, "grpc services registered", "addr", fc.GRPC.Addr)
		} else {
			obs.Warn(ctx, "grpc server configured but service does not implement GRPCRegistrar", "addr", fc.GRPC.Addr)
		}
	}

	// Step 6c: rpcx server + registration (optional)
	var rpcxSrv *rpcx.Server
	if fc.RPCX.Addr != "" {
		rpcxSrv = rpcx.New(fc.RPCX)
		if r, ok := svc.(RPCXRegistrar); ok {
			if err := r.RegisterRPCX(rpcxSrv.RPCX()); err != nil {
				return fmt.Errorf("verticle: register rpcx %s: %w", svc.Name(), err)
			}
			obs.Info(ctx, "rpcx services registered", "addr", rpcxSrv.Addr())
		} else {
			obs.Warn(ctx, "rpcx server configured but service does not implement RPCXRegistrar", "addr", fc.RPCX.Addr)
		}
	}

	// Step 7: run
	a := app.New(svc.Name())
	a.ServeHTTP(srv)
	if grpcSrv != nil {
		a.Serve("grpc", fc.GRPC.Addr, grpcSrv.Serve, grpcSrv.Shutdown)
	}
	if rpcxSrv != nil {
		a.Serve("rpcx", rpcxSrv.Addr(), rpcxSrv.Serve, rpcxSrv.Shutdown)
	}
	for _, b := range env.blocks {
		a.Block(b.name, b.fn)
	}
	// infrastructure closers first (outermost), then service closers:
	// shutdown unwinds in reverse, so services close before infra.
	if env.Kafka != nil {
		a.AddCloser(env.Kafka.Close)
	}
	if env.Redis != nil {
		a.AddCloser(func() { _ = env.Redis.Close() })
	}
	if env.DB != nil {
		a.AddCloser(func() { _ = env.DB.Close() })
	}
	for _, c := range env.closers {
		a.AddCloser(c)
	}

	if !o.noWatch {
		stopWatch := watchConfig(ctx, confPath, svc, env)
		defer stopWatch()
	}

	obs.Info(ctx, "service started", "name", svc.Name(), "conf", confPath)
	return a.Run(ctx)
}

// makeReady builds the readiness check: pings initialized infra, then
// defers to the service's ReadyChecker if implemented.
func makeReady(svc Service, env *Env) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		if env.DB != nil {
			if err := env.DB.Ping(ctx); err != nil {
				return fmt.Errorf("db: %w", err)
			}
		}
		if env.Redis != nil {
			if err := env.Redis.Ping(ctx); err != nil {
				return fmt.Errorf("redis: %w", err)
			}
		}
		if rc, ok := svc.(ReadyChecker); ok {
			return rc.Ready(ctx)
		}
		return nil
	}
}

// watchConfig reloads the config file on changes and notifies the
// service. The 100ms debounce absorbs editors' write-then-rename
// sequences (editors commonly write a temp file and rename it).
func watchConfig(ctx context.Context, path string, svc Service, env *Env) (stop func()) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		obs.Warn(ctx, "config watch unavailable", "err", err)
		return func() {}
	}
	if err := w.Add(filepath.Dir(path)); err != nil {
		obs.Warn(ctx, "config watch unavailable", "err", err)
		_ = w.Close()
		return func() {}
	}
	base := filepath.Base(path)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if filepath.Base(ev.Name) != base {
					continue
				}
				if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
					continue
				}
				time.Sleep(100 * time.Millisecond)
				raw, err := config.LoadMap(path)
				if err != nil {
					obs.Warn(ctx, "config reload failed", "err", err)
					continue
				}
				section, _ := raw[strings.ToLower(svc.Name())].(map[string]any)
				env.setServiceSection(section)
				if cw, ok := svc.(ConfigWatcher); ok {
					cw.OnServiceConfigChange(section)
				}
				obs.Info(ctx, "config reloaded")
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				obs.Warn(ctx, "config watch error", "err", err)
			}
		}
	}()
	return func() {
		_ = w.Close()
		<-done
	}
}
