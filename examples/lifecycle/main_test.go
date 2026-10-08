package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestConfigReloadSwapsTheGreeting(t *testing.T) {
	svc := &lifecycleService{}

	svc.OnServiceConfigChange(map[string]any{
		"greeting":  "Yo, ",
		"heartbeat": "1s",
	})

	if got := svc.greeting(); got != "Yo, " {
		t.Fatalf("greeting = %q, want %q", got, "Yo, ")
	}
	if got := svc.heartbeat(); got != time.Second {
		t.Fatalf("heartbeat = %s, want 1s", got)
	}
}

func TestConfigReloadRejectsBadValues(t *testing.T) {
	svc := &lifecycleService{cfg: lifecycleConfig{
		Greeting:  "keep me",
		Heartbeat: time.Minute,
	}}

	// A malformed duration makes ParseMap fail; a rejected reload must
	// leave the running config untouched.
	svc.OnServiceConfigChange(map[string]any{
		"greeting":  "should not apply",
		"heartbeat": "not-a-duration",
	})

	if got := svc.greeting(); got != "keep me" {
		t.Fatalf("a rejected reload must not change the config, got %q", got)
	}
	if got := svc.heartbeat(); got != time.Minute {
		t.Fatalf("heartbeat = %s, want 1m", got)
	}
}

func TestReadyWindow(t *testing.T) {
	svc := &lifecycleService{started: time.Now(), cfg: lifecycleConfig{ReadyWindow: time.Minute}}
	if err := svc.Ready(t.Context()); err == nil {
		t.Fatal("expected not-ready during the warm-up window")
	}

	svc.cfg.ReadyWindow = 0
	if err := svc.Ready(t.Context()); err != nil {
		t.Fatalf("expected ready, got %v", err)
	}
}

func TestGreetHandler(t *testing.T) {
	svc := &lifecycleService{cfg: lifecycleConfig{Greeting: "Hi "}}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/greeting/world", nil)
	req.SetPathValue("name", "world")

	data, err := svc.greet(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	got := data.(map[string]any)["greeting"]
	if got != "Hi world!" {
		t.Fatalf("greeting = %v", got)
	}
}
