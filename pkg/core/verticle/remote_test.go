package verticle

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSource stands in for a config center. Watch delivers the first
// generation and returns; send pushes a later one.
type fakeSource struct {
	fw, svc map[string]any
	err     error // returned by Watch after delivery
	deliver bool  // deliver the first generation before returning
	push    chan gen
	watches atomic.Int32
}

type gen = [2]map[string]any

func newFakeSource(fw, svc map[string]any) *fakeSource {
	return &fakeSource{fw: fw, svc: svc, deliver: true, push: make(chan gen, 4)}
}

func (f *fakeSource) Framework([]byte, string) (map[string]any, []string, error) {
	return f.fw, nil, nil
}

func (f *fakeSource) Section([]byte, string) (map[string]any, error) { return f.svc, nil }

func (f *fakeSource) Watch(ctx context.Context, onChange func(fw, svc map[string]any)) error {
	f.watches.Add(1)
	if f.deliver {
		onChange(f.fw, f.svc)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case g := <-f.push:
			onChange(g[0], g[1])
		}
	}
}

// reloadService records hot reloads.
type reloadService struct {
	fakeService
	greeting atomic.Value // string
	reloads  chan string
}

func (s *reloadService) Setup(ctx context.Context, env *Env) error {
	if err := s.fakeService.Setup(ctx, env); err != nil {
		return err
	}
	if s.reloads == nil {
		return nil
	}
	var cfg struct {
		Greeting string `json:"greeting" default:"file"`
	}
	if err := env.DecodeService(&cfg); err != nil {
		return err
	}
	s.greeting.Store(cfg.Greeting)
	return nil
}

func (s *reloadService) OnServiceConfigChange(raw map[string]any) {
	g, _ := raw["greeting"].(string)
	s.greeting.Store(g)
	select {
	case s.reloads <- g:
	default: // not blocking the reload loop when the test is not reading
	}
}

// startRun runs a service with the given options until the test ends and
// waits for the router to be built.
func startRun(t *testing.T, svc *reloadService, conf string, opts ...Option) {
	t.Helper()
	// Run installs a global slog logger at obs.Init; put it back so nothing
	// written after this test reaches a closed test log.
	prevLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevLogger) })
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- Run(ctx, svc, append([]Option{WithConf(conf), WithoutConfigWatch()}, opts...)...) }()
	t.Cleanup(func() {
		cancel()
		if err := <-errCh; err != nil {
			t.Errorf("Run returned %v", err)
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for svc.router.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Run never built the router")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func warnConf(t *testing.T) string {
	t.Helper()
	return writeConf(t, "http:\n  addr: \""+freeAddr(t)+"\"\nlog:\n  level: warn\nsvc:\n  greeting: from-file\n")
}

// The center's service section wins over the file at boot.
func TestRunUsesConfigCenterServiceSection(t *testing.T) {
	svc := &reloadService{reloads: make(chan string, 4)}
	svc.name = "svc"
	src := newFakeSource(map[string]any{"log": map[string]any{"level": "warn"}}, map[string]any{"greeting": "from-center"})

	startRun(t, svc, warnConf(t), WithConfig(src))

	if got := svc.greeting.Load(); got != "from-center" {
		t.Fatalf("boot greeting = %v, want the center's value", got)
	}
	if n := src.watches.Load(); n != 1 {
		t.Fatalf("source watched %d times, want 1", n)
	}
}

// A later service-section change is applied live through ConfigWatcher.
func TestRunAppliesCenterServiceChangesLive(t *testing.T) {
	svc := &reloadService{reloads: make(chan string, 4)}
	svc.name = "svc"
	src := newFakeSource(map[string]any{}, map[string]any{"greeting": "v1"})

	startRun(t, svc, warnConf(t), WithConfig(src))

	src.push <- gen{map[string]any{}, map[string]any{"greeting": "v2"}}
	deadline := time.Now().Add(5 * time.Second)
	for svc.greeting.Load() != "v2" {
		if time.Now().After(deadline) {
			t.Fatalf("greeting = %v, the service section change never reached the service", svc.greeting.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := svc.greeting.Load(); got != "v2" {
		t.Fatalf("greeting = %v after reload", got)
	}
}

// Without a service DataID the file's section is used and framework keys
// still come from the center.
func TestRunKeepsFileSectionWithoutServiceData(t *testing.T) {
	svc := &reloadService{reloads: make(chan string, 4)}
	svc.name = "svc"
	src := newFakeSource(map[string]any{"log": map[string]any{"level": "warn"}}, nil)

	startRun(t, svc, warnConf(t), WithConfig(src))

	if got := svc.greeting.Load(); got != "from-file" {
		t.Fatalf("greeting = %v, want the file's value", got)
	}
}

// A center that fails its first fetch fails the boot: running on a stale
// or missing config is worse than not starting.
func TestRunFailsWhenConfigCenterFails(t *testing.T) {
	svc := &reloadService{reloads: make(chan string, 1)}
	svc.name = "svc"
	src := &fakeSource{err: errors.New("unauthorized")}

	prevLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	err := Run(context.Background(), svc, WithConf(warnConf(t)), WithoutConfigWatch(), WithConfig(src))
	if err == nil {
		t.Fatal("Run succeeded with a failing config center")
	}
}

// The `nacos:` section in the file switches the center on; an unreachable
// server fails the boot within the section's timeout, not after a default.
func TestRunReadsNacosSectionFromFile(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// A listener that never answers: the boot must time out, not hang.
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	addr := freeAddr(t)
	conf := writeConf(t, "http:\n  addr: \""+addr+"\"\nlog:\n  level: error\n"+
		"nacos:\n  server_addr: \""+ln.Addr().String()+"\"\n  timeout: 300ms\n  log_dir: \""+t.TempDir()+"\"\n  cache_dir: \""+t.TempDir()+"\"\n")
	svc := &reloadService{reloads: make(chan string, 1)}
	svc.name = "svc"

	start := time.Now()
	err = Run(context.Background(), svc, WithConf(conf), WithoutConfigWatch())
	if err == nil {
		t.Fatal("Run succeeded although nacos never answered")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("boot took %v; the nacos timeout was not applied", d)
	}
}

// A source that delivers nothing must not let Run proceed.
func TestRunFailsWhenConfigCenterDeliversNothing(t *testing.T) {
	svc := &reloadService{reloads: make(chan string, 1)}
	svc.name = "svc"
	src := newFakeSource(nil, nil)
	src.deliver = false
	src.err = nil

	prevLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	err := Run(context.Background(), svc, WithConf(warnConf(t)), WithoutConfigWatch(), WithConfig(src))
	if err == nil {
		t.Fatal("Run succeeded although the center delivered nothing")
	}
}
