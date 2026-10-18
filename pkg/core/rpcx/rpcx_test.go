package rpcx_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/smallnest/matex/pkg/core/rpcx"
)

// A real rpcx server on an ephemeral port: server and client are both
// in-process, so this needs no broker or network.

type Args struct{ In int }

type Reply struct{ Out int }

type doubler struct{}

func (doubler) Double(_ context.Context, a *Args, r *Reply) error {
	r.Out = 2 * a.In
	return nil
}

func (doubler) Fail(_ context.Context, _ *Args, _ *Reply) error {
	return errors.New("boom")
}

// startServer serves a registered doubler and returns its client.
func startServer(t *testing.T) caller {
	t.Helper()
	srv := rpcx.New(rpcx.Config{Network: "tcp", Addr: "127.0.0.1:0"})
	if err := srv.RPCX().RegisterName("Doubler", doubler{}, ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		if err := <-errCh; err != nil {
			t.Errorf("Serve after Shutdown = %v, want nil", err)
		}
	})

	addr := waitForAddr(t, srv)
	c, err := rpcx.PeerClient("Doubler", addr)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

type caller interface {
	Call(ctx context.Context, method string, args, reply any) error
}

func TestRoundTripAndShutdown(t *testing.T) {
	cl := startServer(t)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var r Reply
	if err := cl.Call(ctx, "Double", &Args{In: 21}, &r); err != nil {
		t.Fatalf("call: %v", err)
	}
	if r.Out != 42 {
		t.Fatalf("out = %d, want 42", r.Out)
	}
}

func TestServiceErrorReachesTheClient(t *testing.T) {
	cl := startServer(t)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	err := cl.Call(ctx, "Fail", &Args{}, &Reply{})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want the service error", err)
	}
}

func TestAddrAndDefaultNetwork(t *testing.T) {
	s := rpcx.New(rpcx.Config{Addr: "127.0.0.1:8972"})
	if got := s.Addr(); got != "tcp@127.0.0.1:8972" {
		t.Fatalf("Addr() = %q, want the tcp default", got)
	}
	if got := rpcx.New(rpcx.Config{Network: "quic", Addr: ":1"}).Addr(); got != "quic@:1" {
		t.Fatalf("Addr() = %q", got)
	}
}

func TestPeerClientAddsTCPScheme(t *testing.T) {
	// A bare host:port must be accepted and fail only when dialed.
	cl, err := rpcx.PeerClient("S", "127.0.0.1:1")
	if err != nil {
		t.Fatalf("PeerClient: %v", err)
	}
	_ = cl.Close()
}

func waitForAddr(t *testing.T, srv *rpcx.Server) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if a := srv.RPCX().Address(); a != nil {
			return a.String()
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("rpcx server never started listening")
	return ""
}
