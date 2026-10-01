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
)

// Config configures the gRPC server and the default client dial timeout.
type Config struct {
	Addr        string        `json:"addr" optional:""`
	DialTimeout time.Duration `json:"dial_timeout" default:"3s"`
}

// Server wraps a *grpc.Server with its bound listener.
type Server struct {
	grpc *grpc.Server
	ln   net.Listener
}

// New creates a server listening on cfg.Addr. The server does not start
// serving until Serve is called.
func New(cfg Config, opts ...grpc.ServerOption) (*Server, error) {
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("grpcx: listen %s: %w", cfg.Addr, err)
	}
	return &Server{grpc: grpc.NewServer(opts...), ln: ln}, nil
}

// GRPC exposes the underlying server for service registration.
func (s *Server) GRPC() *grpc.Server { return s.grpc }

// Addr returns the bound address (useful when Addr was ":0").
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Serve blocks serving. Returns nil on graceful shutdown.
func (s *Server) Serve() error {
	err := s.grpc.Serve(s.ln)
	if errors.Is(err, grpc.ErrServerStopped) {
		return nil
	}
	return err
}

// Shutdown gracefully stops, force-stopping after ctx expires.
func (s *Server) Shutdown(ctx context.Context) error {
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
func Dial(ctx context.Context, target string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	all := append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, opts...)
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
