package main

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/smallnest/matex/pkg/core/grpcx"
)

// TestHealthRoundTrip starts a real gRPC server on an ephemeral port and
// dials it with grpcx.Dial — server plus client, no .proto and no network.
func TestHealthRoundTrip(t *testing.T) {
	srv, err := grpcx.New(grpcx.Config{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	// Register before serving so the first Check cannot race registration.
	hs := health.NewServer()
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(srv.GRPC(), hs)

	go func() { _ = srv.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	conn, err := grpcx.Dial(ctx, srv.Addr())
	if err != nil {
		t.Fatalf("dial %s: %v", srv.Addr(), err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if got := resp.GetStatus(); got != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("status = %s, want SERVING", got)
	}
}

func TestRegisterGRPC(t *testing.T) {
	svc := &grpcService{health: health.NewServer()}
	svc.RegisterGRPC(grpc.NewServer()) // must not panic
}

func TestCheckWithoutPeer(t *testing.T) {
	svc := &grpcService{}
	if _, err := svc.check(t.Context(), nil); err == nil {
		t.Fatal("expected an error when no peer is configured")
	}
}
