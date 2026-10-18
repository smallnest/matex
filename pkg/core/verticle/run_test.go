package verticle

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/smallnest/matex/pkg/core/feature"
	"github.com/smallnest/matex/pkg/core/httpx"
)

// fakeService records the lifecycle calls Run makes on it.
type fakeService struct {
	name     string
	setup    atomic.Int32
	router   atomic.Int32
	buildErr error
	sawFlag  func() bool // evaluated inside Setup, nil to skip
}

func (f *fakeService) Name() string { return f.name }

func (f *fakeService) Setup(_ context.Context, env *Env) error {
	if f.buildErr != nil {
		return f.buildErr
	}
	if f.sawFlag != nil && !f.sawFlag() {
		return errors.New("Setup ran before the feature flags were configured")
	}
	f.setup.Add(1)
	// Env is fully wired before Setup: metrics exist for custom collectors.
	if env == nil || env.Metrics == nil {
		return errors.New("env not wired before Setup")
	}
	return nil
}

func (f *fakeService) BuildRouter(s *httpx.Server) error {
	s.Handle("GET", "/ping", func(context.Context, *http.Request) (any, error) {
		return map[string]string{"pong": "1"}, nil
	})
	f.router.Add(1)
	return nil
}

// freeAddr reserves a port and releases it, so Run can bind it.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func writeConf(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRunServesUntilCancelled is the whole lifecycle in one test: config is
// loaded, logging and env are set up before Setup, the router is built, the
// HTTP server answers, and cancelling the context returns nil (a graceful
// shutdown is not an error).
func TestRunServesUntilCancelled(t *testing.T) {
	prevLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevLogger) }) // obs.Init installs a global logger

	// The address must be chosen up front: httpx.Server.Addr reports the
	// configured value, so ":0" would leave nothing to dial.
	addr := freeAddr(t)
	conf := writeConf(t, "http:\n  addr: \""+addr+"\"\nlog:\n  level: warn\nsvc:\n  greeting: hi\n")
	svc := &fakeService{name: "svc"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- Run(ctx, svc, WithConf(conf), WithoutConfigWatch()) }()

	deadline := time.Now().Add(10 * time.Second)
	for svc.router.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Run never built the router")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := svc.setup.Load(); got != 1 {
		t.Fatalf("Setup called %d times, want 1", got)
	}

	// The server is actually serving: /ping answers once the listener is up.
	var status int
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/ping") //nolint:noctx // test client with a bounded loop
		if err == nil {
			status = resp.StatusCode
			_ = resp.Body.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status != http.StatusOK {
		t.Fatalf("/ping status = %d, want 200", status)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run returned %v after cancel, want nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}

// TestRunConfiguresFeatureFlags: the `feature:` section reaches
// pkg/core/feature before Setup runs, so a service can branch on a flag in
// its very first line.
func TestRunConfiguresFeatureFlags(t *testing.T) {
	prevLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	conf := writeConf(t, "http:\n  addr: \""+freeAddr(t)+"\"\nlog:\n  level: warn\n"+
		"feature:\n  run-test-flag: true\nsvc:\n  greeting: hi\n")
	svc := &fakeService{name: "svc", sawFlag: func() bool { return feature.Enabled("run-test-flag") }}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- Run(ctx, svc, WithConf(conf), WithoutConfigWatch()) }()
	deadline := time.Now().Add(10 * time.Second)
	for svc.router.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Run never built the router")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !svc.sawFlag() {
		t.Fatal("the feature section was not applied before Setup")
	}
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Run = %v", err)
	}
}

// TestRunFailsWhenSetupFails: a Setup error stops the boot before anything
// is served, and Run returns a non-nil error.
func TestRunFailsWhenSetupFails(t *testing.T) {
	prevLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	conf := writeConf(t, "http:\n  addr: \"127.0.0.1:0\"\nlog:\n  level: warn\nsvc:\n  greeting: hi\n")
	svc := &fakeService{name: "svc", buildErr: context.DeadlineExceeded}

	err := Run(context.Background(), svc, WithConf(conf), WithoutConfigWatch())
	if err == nil {
		t.Fatal("Run returned nil although Setup failed")
	}
	if svc.router.Load() != 0 {
		t.Fatal("BuildRouter ran although Setup failed")
	}
}

// TestRunDoesNotListenOnRPCAddressesWithoutARegistrar: a service without
// GRPCRegistrar/RPCXRegistrar must not open those ports — an empty listener
// looks healthy to a probe while serving nothing.
func TestRunDoesNotListenOnRPCAddressesWithoutARegistrar(t *testing.T) {
	prevLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	grpcAddr, rpcxAddr := freeAddr(t), freeAddr(t)
	conf := writeConf(t, "http:\n  addr: \""+freeAddr(t)+"\"\nlog:\n  level: error\n"+
		"grpc:\n  addr: \""+grpcAddr+"\"\nrpcx:\n  addr: \""+rpcxAddr+"\"\nsvc:\n  greeting: hi\n")
	svc := &fakeService{name: "svc"}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- Run(ctx, svc, WithConf(conf), WithoutConfigWatch()) }()
	deadline := time.Now().Add(10 * time.Second)
	for svc.router.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Run never built the router")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, addr := range []string{grpcAddr, rpcxAddr} {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			t.Fatalf("%s is listening although the service registers no RPC services", addr)
		}
	}
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("Run = %v", err)
	}
}
