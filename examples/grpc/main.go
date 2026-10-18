// Command grpc demonstrates pkg/core/grpcx:
//
//   - a gRPC server created by verticle.Run from the `grpc:` section
//   - the standard health service and reflection, registered by grpcx itself
//     (config `health` / `reflection`) — no RegisterGRPC needed here
//   - service registration through the GRPCRegistrar hook (shown below)
//   - a fail-fast client dialed in Setup via env.GRPCClient (closed
//     automatically on shutdown)
//
// Because grpcx provides the health service, the example needs no
// .proto/protoc. In a real project you register generated code instead,
// by implementing RegisterGRPC:
//
//	func (s *Service) RegisterGRPC(g *grpc.Server) {
//		pb.RegisterGreeterServer(g, s)   // 生成的代码
//	}
//
// Run:
//
//	go run ./examples/grpc                       # http :8080 + grpc :9090
//	grpcurl -plaintext localhost:9090 grpc.health.v1.Health/Check
//	grpcurl -plaintext localhost:9090 list       # reflection is on in config.yaml
//
// To exercise the client path, start a second instance (or any peer that
// serves grpc.health.v1) and set that instance's `grpcdemo.target` to the
// first one's grpc address:
//
//	curl -i localhost:8080/api/v1/health     # → {"status":"SERVING", …}
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"

	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/smallnest/matex/pkg/core/errs"
	"github.com/smallnest/matex/pkg/core/httpx"
	"github.com/smallnest/matex/pkg/core/obs"
	"github.com/smallnest/matex/pkg/core/verticle"
)

type grpcConfig struct {
	Target  string `json:"target" optional:""`  // peer host:port; empty → server only
	Service string `json:"service" optional:""` // health service name; "" → overall
}

type grpcService struct {
	cfg    grpcConfig
	client healthpb.HealthClient
}

func (s *grpcService) Name() string { return "grpcdemo" }

func (s *grpcService) Setup(ctx context.Context, env *verticle.Env) error {
	if err := env.DecodeService(&s.cfg); err != nil {
		return err
	}

	// A peer client. Dialing here is deliberate: an unreachable
	// dependency should fail the startup, not the first request.
	if s.cfg.Target != "" {
		conn, err := env.GRPCClient(ctx, s.cfg.Target)
		if err != nil {
			return err
		}
		s.client = healthpb.NewHealthClient(conn)
		obs.Info(ctx, "grpc client ready", "target", s.cfg.Target)
	}
	return nil
}

func (s *grpcService) BuildRouter(srv *httpx.Server) error {
	srv.Handle("GET", "/api/v1/health", s.check)
	return nil
}

// check calls the peer over gRPC and exposes the result over HTTP —
// a common pattern for a BFF / gateway style service.
func (s *grpcService) check(ctx context.Context, _ *http.Request) (any, error) {
	if s.client == nil {
		return nil, errs.Unavailable(50301, "no gRPC peer configured (set grpcdemo.target)")
	}
	resp, err := s.client.Check(ctx, &healthpb.HealthCheckRequest{Service: s.cfg.Service})
	if err != nil {
		return nil, errs.Unavailable(50302, "gRPC check failed: %v", err)
	}
	return map[string]any{
		"target":  s.cfg.Target,
		"service": s.cfg.Service,
		"status":  resp.GetStatus().String(),
	}, nil
}

func main() {
	conf := flag.String("conf", "examples/grpc/config.yaml", "config file")
	flag.Parse()

	if err := verticle.Run(context.Background(), &grpcService{}, verticle.WithConf(*conf)); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}
