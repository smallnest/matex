package grpcx

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc/reflection"
)

func TestServerLifecycle(t *testing.T) {
	srv, err := New(Config{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	reflection.Register(srv.GRPC())
	if srv.Addr() == "" {
		t.Fatal("expected bound addr")
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve() }()
	time.Sleep(50 * time.Millisecond) // let it start serving

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("serve: %v", err) // Serve must return nil on graceful stop
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serve did not return after shutdown")
	}
}

func TestDialReady(t *testing.T) {
	srv, err := New(Config{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	reflection.Register(srv.GRPC())
	go func() { _ = srv.Serve() }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := Dial(ctx, srv.Addr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestDialUnreachableFailsFast(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close() // free the port so dial fails

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := Dial(ctx, addr); err == nil {
		t.Fatal("expected dial error for unreachable address")
	}
}
