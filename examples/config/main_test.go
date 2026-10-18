package main

import (
	"os"
	"strings"
	"testing"

	"github.com/smallnest/matex/pkg/core/config"
)

// The demo is the reference for tag semantics: file values, ${VAR}
// expansion, env overrides and defaults must combine as documented.

func TestLoadFromExampleFile(t *testing.T) {
	var cfg Config
	if err := config.Load("config.yaml", &cfg); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTP.Timeout.String() != "5s" || cfg.DB.MaxOpenConns != 8 {
		t.Fatalf("file values not applied: %+v", cfg)
	}
	if len(cfg.Features) != 2 || cfg.Features[0] != "cache" {
		t.Fatalf("yaml list features = %v", cfg.Features)
	}
	if !cfg.Debug {
		t.Fatal("debug = false, want true from the file")
	}
	if cfg.DB.MaxIdleConns != 0 {
		t.Fatalf("optional key present in the file resolves to %d, want the zero value", cfg.DB.MaxIdleConns)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	t.Setenv("DB_DSN", "postgres://env/overridden")

	var cfg Config
	if err := config.Load("config.yaml", &cfg); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DB.DSN != "postgres://env/overridden" {
		t.Fatalf("dsn = %q, want the env value", cfg.DB.DSN)
	}
}

func TestDefaultsAndExpansionWithoutFile(t *testing.T) {
	var cfg Config
	// A file that sets none of the optional keys: everything else comes
	// from the tags.
	if err := config.Parse([]byte("http: {}\ndb: {dsn: x}\n"), &cfg); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.HTTP.Timeout.String() != "10s" || cfg.DB.MaxOpenConns != 16 {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	if strings.Join(cfg.Features, ",") != "a,b" {
		t.Fatalf("default list = %v", cfg.Features)
	}
	if cfg.Debug {
		t.Fatal("debug default must be false")
	}
}

func TestVarExpansionInYAML(t *testing.T) {
	doc := []byte("http:\n  addr: ${HTTP_ADDR:0.0.0.0:1}\ndb:\n  dsn: x\n")

	t.Setenv("HTTP_ADDR", "127.0.0.1:7777")
	var cfg Config
	if err := config.Parse(doc, &cfg); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.HTTP.Addr != "127.0.0.1:7777" {
		t.Fatalf("addr = %q, want the HTTP_ADDR expansion", cfg.HTTP.Addr)
	}

	unsetenv(t, "HTTP_ADDR")
	var fallback Config
	if err := config.Parse(doc, &fallback); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if fallback.HTTP.Addr != "0.0.0.0:1" {
		t.Fatalf("addr = %q, want the ${VAR:default} fallback", fallback.HTTP.Addr)
	}
}

func unsetenv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")    // register the restore
	_ = os.Unsetenv(key) // ...then actually unset it
}

func TestMissingRequiredKeyFails(t *testing.T) {
	var strict struct {
		Required string `json:"required"`
	}
	if err := config.Parse([]byte("other: 1"), &strict); err == nil {
		t.Fatal("a key with neither default nor optional must be required")
	}
}
