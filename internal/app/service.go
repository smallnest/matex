// Package app assembles the demo service: it implements
// verticle.Service, wiring the configured infrastructure (from
// verticle.Env) into the helloworld domain.
package app

import (
	"context"

	rpcxserver "github.com/smallnest/rpcx/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	"github.com/smallnest/matex/internal/domain/helloworld"
	"github.com/smallnest/matex/pkg/core/config"
	"github.com/smallnest/matex/pkg/core/httpx"
	"github.com/smallnest/matex/pkg/core/kafka"
	"github.com/smallnest/matex/pkg/core/obs"
	"github.com/smallnest/matex/pkg/core/verticle"
)

// DemoConfig is the service-specific section of config.yaml (keyed by
// DemoService.Name()).
type DemoConfig struct {
	Greeting string `json:"greeting" default:"Hello, "`
	Consume  bool   `json:"consume" default:"false"`
}

// DemoService is the deployment unit.
type DemoService struct {
	cfg   DemoConfig
	hello *helloworld.Service
}

// Name is the config-section key and the log/metrics label.
func (s *DemoService) Name() string { return "demo" }

// Setup decodes the service config and builds the domain services.
func (s *DemoService) Setup(ctx context.Context, env *verticle.Env) error {
	if err := env.DecodeService(&s.cfg); err != nil {
		return err
	}
	s.hello = helloworld.New(helloworld.Deps{
		DB:       env.DB,
		Redis:    env.Redis,
		Memcache: env.Memcache,
		Producer: env.Kafka,
		Greeting: s.cfg.Greeting,
	})

	// Optional consumer: enable with `consume: true` in config.
	if s.cfg.Consume {
		env.Block("greeting-consumer", func(ctx context.Context) error {
			return kafka.RunConsumer(ctx, env.KafkaCfg,
				"demo-greetings", []string{helloworld.TopicGreetings},
				s.hello.HandleGreetingEvent)
		})
	}

	// gRPC client to a peer service (optional): the connection is
	// fail-fast dialed and closed by verticle on shutdown.
	//
	//   conn, err := env.GRPCClient(ctx, "user-service:9090")
	//   if err != nil { return err }
	//   s.userClient = userpb.NewUserServiceClient(conn)

	// rpcx client to a peer service (optional): same lifecycle story.
	//
	//   xc, err := env.RPCXClient("Greeter", "demo:8972")
	//   if err != nil { return err }
	//   reply := &helloworld.GreetReply{}
	//   err = xc.Call(ctx, "Greet", &helloworld.GreetArgs{Name: "x"}, reply)

	return nil
}

// RegisterGRPC registers the service's gRPC endpoints. It is called by
// verticle when the `grpc` section is configured. The minimal demo
// enables server reflection (listable with grpcurl); a real service
// registers its generated impls, e.g.
// `pb.RegisterGreeterServer(g, &greeter{})`.
func (s *DemoService) RegisterGRPC(g *grpc.Server) {
	reflection.Register(g)
}

// RegisterRPCX registers the service's rpcx endpoints. It is called by
// verticle when the `rpcx` section is configured. rpcx needs no IDL:
// plain structs with method signature (ctx, args, reply) error.
func (s *DemoService) RegisterRPCX(g *rpcxserver.Server) error {
	return g.RegisterName("Greeter", helloworld.NewRPCServer(s.hello), "")
}

// BuildRouter registers the domain routes.
func (s *DemoService) BuildRouter(srv *httpx.Server) error {
	h := helloworld.NewHandler(s.hello)
	srv.Handle("GET", "/api/v1/hello/{name}", h.Greet)
	srv.Handle("GET", "/api/v1/hello/stats/{name}", h.Stats)
	srv.Handle("POST", "/api/v1/hello/events", h.PublishEvent)
	return nil
}

// OnServiceConfigChange is the optional hot-reload hook: logs the new
// greeting so operators can see reloads work.
func (s *DemoService) OnServiceConfigChange(raw map[string]any) {
	var cfg DemoConfig
	if err := config.ParseMap(raw, &cfg); err != nil {
		obs.Warn(context.Background(), "reload config decode failed", "err", err)
		return
	}
	obs.Info(context.Background(), "service config reloaded", "greeting", cfg.Greeting)
}
