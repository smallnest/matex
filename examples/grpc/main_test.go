package main

import (
	"context"
	"testing"
	"time"

	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/smallnest/matex/pkg/core/grpcx"
)

// TestHealthRoundTrip starts a real gRPC server on an ephemeral port and
// dials it with grpcx.Dial — server plus client, no .proto and no network.
func TestHealthRoundTrip(t *testing.T) {
	// No registration call: grpcx serves grpc.health.v1 by default.
	srv, err := grpcx.New(grpcx.Config{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

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

func TestCheckWithoutPeer(t *testing.T) {
	svc := &grpcService{}
	if _, err := svc.check(t.Context(), nil); err == nil {
		t.Fatal("expected an error when no peer is configured")
	}
}

// TestServiceRoutesToThePeerWithTheRealClient wires the example's BFF
// handler to a real peer and checks the status travels through HTTP.
func TestServiceRoutesToThePeerWithTheRealClient(t *testing.T) {
	peer, err := grpcx.New(grpcx.Config{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = peer.Serve() }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = peer.Shutdown(ctx)
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, err := grpcx.Dial(ctx, peer.Addr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	svc := &grpcService{cfg: grpcConfig{Target: peer.Addr()}, client: healthpb.NewHealthClient(conn)}
	got, err := svc.check(ctx, nil)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if got.(map[string]any)["status"] != "SERVING" {
		t.Fatalf("status = %v", got)
	}
}
