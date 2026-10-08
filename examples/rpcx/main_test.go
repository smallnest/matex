package main

import (
	"context"
	"testing"
	"time"

	"github.com/smallnest/matex/pkg/core/rpcx"
)

// TestRoundTrip starts a real rpcx server on an ephemeral port and calls it
// through the real client: server plus client, no broker, no network.
func TestRoundTrip(t *testing.T) {
	srv := rpcx.New(rpcx.Config{Network: "tcp", Addr: "127.0.0.1:0"})
	if err := srv.RPCX().RegisterName("Greeter", &Greeter{prefix: "Hi "}, ""); err != nil {
		t.Fatalf("register: %v", err)
	}

	go func() { _ = srv.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	// rpcx binds inside Serve, so wait for the listener to appear.
	addr := waitForAddr(t, srv)
	cl, err := rpcx.PeerClient("Greeter", addr)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	t.Cleanup(func() { _ = cl.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	var reply GreetReply
	if err := cl.Call(ctx, "Greet", &GreetArgs{Name: "world"}, &reply); err != nil {
		t.Fatalf("call: %v", err)
	}
	if reply.Greeting != "Hi world" {
		t.Fatalf("greeting = %q, want %q", reply.Greeting, "Hi world")
	}
}

func TestGreeterValidatesName(t *testing.T) {
	err := (&Greeter{}).Greet(t.Context(), &GreetArgs{}, &GreetReply{})
	if err == nil {
		t.Fatal("expected an error for an empty name")
	}
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
