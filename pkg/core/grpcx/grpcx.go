// Package grpcx wraps google.golang.org/grpc with a verticle-friendly
// server (bound listener + graceful shutdown) and a fail-fast client
// dialer. Business code registers its generated gRPC services through
// the verticle.GRPCRegistrar hook; it never hand-rolls the server
// lifecycle.
//
// Transport is insecure (plaintext) by default, which is the norm for
// intra-cluster service-to-service calls behind a mesh or mTLS-less
// VPC. Override with grpc.WithTransportCredentials(...).
package grpcx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

// Config configures the gRPC server and the default client dial timeout.
type Config struct {
	Addr        string        `json:"addr" optional:""`
	DialTimeout time.Duration `json:"dial_timeout" default:"3s"`
	// Health answers the standard grpc.health.v1 health checks (Kubernetes
	// native gRPC probes, load balancers, grpcurl). On by default.
	Health *bool `json:"health" default:"true"`
	// Reflection lets tools (grpcurl, grpcui) list services without the
	// .proto files. Off by default: it publishes the service surface to
	// anyone who can reach the port.
	Reflection bool `json:"reflection" default:"false"`
}

// Server wraps a *grpc.Server with its bound listener.
type Server struct {
	grpc *grpc.Server
	ln   net.Listener
	hs   *health.Server // nil when health checks are off
}

// New creates a server listening on cfg.Addr. The server does not start
// serving until Serve is called.
//
// Trace propagation (unary + stream) is always wired in; it is a no-op
// unless obs tracing is enabled. Pass extra ServerOptions to add your own
// interceptors — use grpc.ChainUnaryInterceptor so they compose with the
// tracing one instead of replacing it.
//
// Health and reflection are registered here when cfg asks for them, so no
// service has to remember to. The server starts NOT_SERVING and reports
// SERVING once Serve is called, which keeps "listening" and "ready" the
// same moment from a probe's point of view.
func New(cfg Config, opts ...grpc.ServerOption) (*Server, error) {
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("grpcx: listen %s: %w", cfg.Addr, err)
	}
	opts = append(opts,
		grpc.ChainUnaryInterceptor(traceUnaryServer),
		grpc.ChainStreamInterceptor(traceStreamServer),
	)
	s := &Server{grpc: grpc.NewServer(opts...), ln: ln}
	if cfg.Health == nil || *cfg.Health {
		s.hs = health.NewServer()
		s.hs.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
		healthpb.RegisterHealthServer(s.grpc, s.hs)
	}
	if cfg.Reflection {
		reflection.Register(s.grpc)
	}
	return s, nil
}

// SetServing flips health status per service name ("" is the whole server).
// Wire a check that fails (a dependency down) to StatusNOTSERVING so
// load balancers stop routing here before requests start failing.
func (s *Server) SetServing(service string, status healthpb.HealthCheckResponse_ServingStatus) {
	if s.hs != nil {
		s.hs.SetServingStatus(service, status)
	}
}

// GRPC exposes the underlying server for service registration.
func (s *Server) GRPC() *grpc.Server { return s.grpc }

// Addr returns the bound address (useful when Addr was ":0").
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Serve blocks serving. Returns nil on graceful shutdown.
func (s *Server) Serve() error {
	if s.hs != nil {
		s.hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	}
	err := s.grpc.Serve(s.ln)
	if errors.Is(err, grpc.ErrServerStopped) {
		return nil
	}
	return err
}

// Shutdown gracefully stops, force-stopping after ctx expires. Health flips
// to NOT_SERVING first, so probes stop sending traffic while in-flight
// calls drain.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.hs != nil {
		s.hs.Shutdown()
	}
	done := make(chan struct{})
	go func() {
		s.grpc.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		s.grpc.Stop()
		return ctx.Err()
	}
}

// Dial connects to target, blocking until the connection is READY or
// ctx expires (fail-fast at startup). The caller owns the returned conn.
//
// Trace propagation (unary + stream) is always wired in; it is a no-op
// unless obs tracing is enabled. A caller-supplied unary interceptor is
// composed (it runs outside the tracing one); use grpc.ChainUnaryInterceptor
// for predictable ordering.
func Dial(ctx context.Context, target string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	all := append([]grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(traceUnaryClient),
		grpc.WithChainStreamInterceptor(traceStreamClient),
	}, opts...)
	conn, err := grpc.NewClient(target, all...)
	if err != nil {
		return nil, fmt.Errorf("grpcx: dial %s: %w", target, err)
	}
	conn.Connect()
	state := conn.GetState()
	for state != connectivity.Ready && conn.WaitForStateChange(ctx, state) {
		state = conn.GetState()
	}
	if state != connectivity.Ready {
		_ = conn.Close()
		return nil, fmt.Errorf("grpcx: dial %s: not ready (state=%s)", target, state)
	}
	return conn, nil
}
