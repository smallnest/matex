package app

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/smallnest/matex/pkg/core/httpx"
)

// The shutdown order is the contract: servers drain first, then blocks
// stop, then closers run in reverse registration order.

// recorder collects the order in which lifecycle steps ran.
type recorder struct {
	mu  sync.Mutex
	seq []string
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq = append(r.seq, s)
}

func (r *recorder) get() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seq...)
}

func TestShutdownOrder(t *testing.T) {
	rec := &recorder{}
	a := New("order")

	a.Serve("srv", ":0", func() error { return nil }, func(context.Context) error {
		rec.add("server.shutdown")
		return nil
	})
	started := make(chan struct{})
	a.Block("worker", func(ctx context.Context) error {
		rec.add("block.start")
		close(started)
		<-ctx.Done()
		rec.add("block.stop")
		return ctx.Err()
	})
	a.AddCloser(func() { rec.add("closer.1") })
	a.AddCloser(func() { rec.add("closer.2") })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// A block that returns quickly can observe the cancellation before the
	// server shutdown runs; the block is still joined before any closer.
	shutOrStop := map[string]bool{"server.shutdown": true, "block.stop": true}
	want := []string{"block.start", "", "", "closer.2", "closer.1"}
	got := rec.get()
	if len(got) != len(want) {
		t.Fatalf("order = %v", got)
	}
	if !shutOrStop[got[1]] || !shutOrStop[got[2]] {
		t.Fatalf("order = %v: servers and blocks must finish before closers", got)
	}
	for i, w := range want {
		if w == "" {
			continue
		}
		if got[i] != w {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestFatalBlockStopsTheApp(t *testing.T) {
	a := New("fatal")
	closerRan := false
	a.Block("boom", func(context.Context) error { return errors.New("db gone") })
	a.AddCloser(func() { closerRan = true })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := a.Run(ctx)
	if err == nil || !contains(err.Error(), "boom") {
		t.Fatalf("Run = %v, want the block error", err)
	}
	if !closerRan {
		t.Fatal("closers must run after a fatal block")
	}
}

func TestCancelledBlockIsNotFatal(t *testing.T) {
	a := New("cancel")
	started := make(chan struct{})
	a.Block("worker", func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return context.Canceled
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run after cancellation: %v (a cancelled block must not abort the app)", err)
	}
}

func TestBadServerIsFatal(t *testing.T) {
	a := New("server")
	a.Serve("broken", ":0", func() error { return errors.New("bind failed") }, func(context.Context) error { return nil })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := a.Run(ctx)
	if err == nil || !contains(err.Error(), "bind failed") {
		t.Fatalf("Run = %v, want the server error", err)
	}
}

func TestServeHTTPStopsOnSignal(t *testing.T) {
	s := httpx.New(httpx.Config{Addr: "127.0.0.1:0"})
	s.Handle("GET", "/ping", func(context.Context, *http.Request) (any, error) { return "pong", nil })

	a := New("http")
	a.ServeHTTP(s)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	time.AfterFunc(100*time.Millisecond, cancel)
	if err := a.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestShutdownWaitsForSlowBlockUpToTimeout(t *testing.T) {
	// A block that ignores cancellation must not hang shutdown forever.
	done := make(chan struct{})
	a := New("stuck")
	release := make(chan struct{})
	a.Block("stuck", func(context.Context) error {
		close(done)
		<-release
		return nil
	})
	closed := false
	a.AddCloser(func() { closed = true })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-done
		cancel()
	}()
	start := time.Now()
	err := a.Run(ctx)
	elapsed := time.Since(start)
	close(release)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("shutdown took %v; it must give up after 10s", elapsed)
	}
	if !closed {
		t.Fatal("closers must run even when a block overruns")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
