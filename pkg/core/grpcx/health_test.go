package grpcx

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection/grpc_reflection_v1"
)

// startTestServer boots a server and returns a dialed connection.
func startTestServer(t *testing.T, cfg Config) *grpc.ClientConn {
	t.Helper()
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := Dial(ctx, srv.Addr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func checkHealth(t *testing.T, conn *grpc.ClientConn) healthpb.HealthCheckResponse_ServingStatus {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("health check: %v", err)
	}
	return resp.GetStatus()
}

// TestHealthRegisteredByDefault: a probe can ask the standard health service
// without the service doing anything, and the answer is SERVING.
func TestHealthRegisteredByDefault(t *testing.T) {
	conn := startTestServer(t, Config{Addr: "127.0.0.1:0"})
	if got := checkHealth(t, conn); got != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("health = %v, want SERVING", got)
	}
}

func TestHealthCanBeDisabled(t *testing.T) {
	off := false
	conn := startTestServer(t, Config{Addr: "127.0.0.1:0", Health: &off})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{}); err == nil {
		t.Fatal("health checks answered although they were disabled")
	}
}

// TestReflectionIsOptIn: reflection publishes the service surface, so it
// must stay off unless asked for.
func TestReflectionIsOptIn(t *testing.T) {
	list := func(conn *grpc.ClientConn) error {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		stream, err := grpc_reflection_v1.NewServerReflectionClient(conn).ServerReflectionInfo(ctx)
		if err != nil {
			return nil // the v1 reflection method is absent
		}
		return stream.Send(&grpc_reflection_v1.ServerReflectionRequest{
			MessageRequest: &grpc_reflection_v1.ServerReflectionRequest_ListServices{ListServices: ""},
		})
	}

	if err := list(startTestServer(t, Config{Addr: "127.0.0.1:0"})); err != nil {
		t.Fatalf("reflection answered although it is off: %v", err)
	}

	conn := startTestServer(t, Config{Addr: "127.0.0.1:0", Reflection: true})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stream, err := grpc_reflection_v1.NewServerReflectionClient(conn).ServerReflectionInfo(ctx)
	if err != nil {
		t.Fatalf("reflection stream: %v", err)
	}
	if err := stream.Send(&grpc_reflection_v1.ServerReflectionRequest{
		MessageRequest: &grpc_reflection_v1.ServerReflectionRequest_ListServices{ListServices: ""},
	}); err != nil {
		t.Fatalf("send on reflection stream: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("recv on reflection stream: %v", err)
	}
	_ = stream.CloseSend()
}
