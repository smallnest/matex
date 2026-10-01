// Package app runs a service: an HTTP server, any number of background
// blocks and a stack of cleanup functions, under signal-driven
// graceful shutdown.
//
// Shutdown order: http drain (10s) → wait for blocks (10s) → closers in
// reverse registration order. A block returning a non-cancel error
// aborts the whole app (fail-fast: let the orchestrator restart us).
package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/signal"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/smallnest/matex/pkg/core/httpx"
	"github.com/smallnest/matex/pkg/core/obs"
)

type block struct {
	name string
	fn   func(ctx context.Context) error
}

// server is a runnable listener (http, grpc, ...) with a graceful stop.
// serve returns nil on an orderly shutdown; any other error is fatal.
type server struct {
	name     string
	addr     string
	serve    func() error
	shutdown func(context.Context) error
}

// App is the lifecycle container.
type App struct {
	name    string
	servers []server
	blocks  []block
	closers []func()
}

// New creates an App.
func New(name string) *App { return &App{name: name} }

// Serve registers a runnable server (http, grpc, ...). serve must
// return nil on a graceful stop; shutdown drains it within ctx.
func (a *App) Serve(name, addr string, serve func() error, shutdown func(context.Context) error) {
	a.servers = append(a.servers, server{name: name, addr: addr, serve: serve, shutdown: shutdown})
}

// ServeHTTP registers the web server to run.
func (a *App) ServeHTTP(s *httpx.Server) {
	a.Serve("http", s.Addr(), func() error {
		if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}, s.Shutdown)
}

// Block registers a background task (consumers, pollers...). It runs
// with the app context; returning a non-cancel error stops the app.
func (a *App) Block(name string, fn func(ctx context.Context) error) {
	a.blocks = append(a.blocks, block{name: name, fn: fn})
}

// AddCloser registers cleanup, executed in reverse order at shutdown.
func (a *App) AddCloser(f func()) { a.closers = append(a.closers, f) }

// Run blocks until SIGINT/SIGTERM or a fatal error.
func (a *App) Run(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, len(a.blocks)+len(a.servers))
	fail := func(err error) {
		select {
		case errCh <- err:
		default:
		}
	}

	var wg sync.WaitGroup
	for _, b := range a.blocks {
		wg.Go(func() {
			if err := b.fn(ctx); err != nil && !errors.Is(err, context.Canceled) {
				fail(fmt.Errorf("block %q: %w", b.name, err))
			}
		})
	}
	for _, srv := range a.servers {
		go func(srv server) {
			obs.Info(ctx, "server listening", "name", srv.name, "addr", srv.addr)
			if err := srv.serve(); err != nil {
				fail(fmt.Errorf("%s: %w", srv.name, err))
			}
		}(srv)
	}

	var runErr error
	select {
	case err := <-errCh:
		runErr = err
		stop()
	case <-ctx.Done():
	}
	a.shutdown(ctx, &wg)
	return runErr
}

func (a *App) shutdown(ctx context.Context, wg *sync.WaitGroup) {
	logCtx := context.WithoutCancel(ctx)
	for _, srv := range a.servers {
		sctx, cancel := context.WithTimeout(logCtx, 10*time.Second)
		if err := srv.shutdown(sctx); err != nil {
			obs.Error(logCtx, "server shutdown", "name", srv.name, "err", err)
		}
		cancel()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		obs.Error(logCtx, "shutdown timeout waiting for blocks", "name", a.name)
	}
	for _, closer := range slices.Backward(a.closers) {
		closer()
	}
	obs.Info(logCtx, "shutdown complete", "name", a.name)
}
