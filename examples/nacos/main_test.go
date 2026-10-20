package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/smallnest/matex/pkg/core/verticle"
)

type gen struct{ fw, svc map[string]any }

// fakeCenter stands in for Nacos: it serves one framework doc and one
// service doc, and push delivers a later version.
type fakeCenter struct {
	fw, svc map[string]any
	push    chan gen
}

func (f *fakeCenter) Framework([]byte, string) (map[string]any, []string, error) {
	return f.fw, nil, nil
}

func (f *fakeCenter) Section([]byte, string) (map[string]any, error) { return f.svc, nil }

func (f *fakeCenter) Watch(ctx context.Context, onChange func(fw, svc map[string]any)) error {
	onChange(f.fw, f.svc)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case g := <-f.push:
			onChange(g.fw, g.svc)
		}
	}
}

// The center's service DataID configures the greeting at boot, and a later
// change reaches the service live through verticle.Run's watcher.
func TestCenterConfiguresAndReloadsLive(t *testing.T) {
	center := &fakeCenter{
		fw: map[string]any{
			"log":  map[string]any{"level": "warn"},
			"http": map[string]any{"addr": freeAddr(t)},
		},
		svc:  map[string]any{"greeting": "boot, "},
		push: make(chan gen, 1),
	}
	svc := &nacosService{}
	conf := writeConf(t, "demo:\n  greeting: \"from the file, \"\n")

	prevLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevLogger) })
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- verticle.Run(ctx, svc, verticle.WithConf(conf), verticle.WithoutConfigWatch(), verticle.WithConfig(center))
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-errCh; err != nil {
			t.Errorf("Run returned %v", err)
		}
	})

	// The center's DataID wins over the file at boot. The handler may be
	// polled before Run finishes Setup, so wait for the server first.
	waitFor(t, func() bool {
		resp, err := http.Get("http://" + center.fw["http"].(map[string]any)["addr"].(string) + "/api/v1/greeting/world")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return true
	})
	if got := call(t, svc)["greeting"]; got != "boot, world!" {
		t.Fatalf("boot greeting = %v, want the center's value", got)
	}

	center.push <- gen{center.fw, map[string]any{"greeting": "live, "}}
	waitFor(t, func() bool { return call(t, svc)["greeting"] == "live, world!" })
	if v := call(t, svc)["version"]; v != 1 {
		t.Fatalf("version = %v after one reload, want 1", v)
	}
}

// A rejected reload leaves the running config in place.
func TestReloadRejectsBadValues(t *testing.T) {
	svc := &nacosService{greeting: "keep, "}
	// A misspelled key is rejected: applying it would blank the greeting.
	svc.OnServiceConfigChange(map[string]any{"gretting": "typo, "})
	if got := call(t, svc)["greeting"]; got != "keep, world!" {
		t.Fatalf("a reload with an unknown key changed the greeting to %v", got)
	}
	if v := call(t, svc)["version"]; v != 0 {
		t.Fatalf("version = %v, want 0", v)
	}
}

func writeConf(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

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

func call(t *testing.T, svc *nacosService) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/greeting/world", nil)
	req.SetPathValue("name", "world")
	data, err := svc.greet(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return data.(map[string]any)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never became true")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
