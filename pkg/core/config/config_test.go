package config

import (
	"os"
	"testing"
	"time"
)

type subCfg struct {
	Host string `json:"host"`
	Port int    `json:"port" default:"8080"`
}

type testCfg struct {
	Name      string            `json:"name" default:"matex"`
	Debug     bool              `json:"debug" default:"false"`
	Retries   int               `json:"retries" default:"3"`
	Ratio     float64           `json:"ratio" optional:""`
	Timeout   time.Duration     `json:"timeout" default:"5s"`
	Slow      time.Duration     `json:"slow" optional:""`
	Tags      []string          `json:"tags" optional:""`
	Sub       subCfg            `json:"sub"`
	Opt       *subCfg           `json:"opt" optional:""`
	Labels    map[string]string `json:"labels" optional:""`
	MissingOK string            `json:"missing_ok" optional:""`
	EnvOnly   string            `json:"env_only" optional:"" env:"MATEX_TEST_ENV_ONLY"`
}

func TestParseDefaults(t *testing.T) {
	var c testCfg
	yaml := []byte(`
name: prod
debug: true
retries: 10
ratio: 0.25
timeout: 3s
slow: 250        # number = milliseconds
tags: [a, b]
sub:
  host: db.local
labels:
  env: test
  tier: web
`)
	if err := Parse(yaml, &c); err != nil {
		t.Fatal(err)
	}
	if c.Name != "prod" || !c.Debug || c.Retries != 10 || c.Ratio != 0.25 {
		t.Fatalf("basic: %+v", c)
	}
	if c.Timeout != 3*time.Second {
		t.Fatalf("timeout: %v", c.Timeout)
	}
	if c.Slow != 250*time.Millisecond {
		t.Fatalf("slow: %v", c.Slow)
	}
	if len(c.Tags) != 2 || c.Tags[0] != "a" {
		t.Fatalf("tags: %v", c.Tags)
	}
	if c.Sub.Host != "db.local" || c.Sub.Port != 8080 {
		t.Fatalf("sub: %+v", c.Sub)
	}
	if c.Labels["env"] != "test" || c.Labels["tier"] != "web" {
		t.Fatalf("labels: %v", c.Labels)
	}
	if c.MissingOK != "" {
		t.Fatalf("optional should be zero")
	}
}

func TestParseMissingKey(t *testing.T) {
	type cfg struct {
		Required string `json:"required"`
	}
	err := Parse([]byte(`other: x`), &cfg{})
	if err == nil {
		t.Fatal("expected missing-key error")
	}
}

func TestEnvExpansion(t *testing.T) {
	t.Setenv("MATEX_TEST_DSN", "postgres://env/host")
	var c struct {
		DSN string `json:"dsn"`
	}
	if err := Parse([]byte("dsn: ${MATEX_TEST_DSN}"), &c); err != nil {
		t.Fatal(err)
	}
	if c.DSN != "postgres://env/host" {
		t.Fatalf("dsn: %q", c.DSN)
	}
	// default expansion when var unset
	if err := Parse([]byte("dsn: ${MATEX_UNSET_VAR:postgres://fallback}"), &c); err != nil {
		t.Fatal(err)
	}
	if c.DSN != "postgres://fallback" {
		t.Fatalf("dsn fallback: %q", c.DSN)
	}
}

func TestEnvTagOverride(t *testing.T) {
	t.Setenv("MATEX_TEST_ENV_ONLY", "from-env")
	var c testCfg
	if err := Parse([]byte("debug: false\nenv_only: from-file\nsub:\n  host: h\n"), &c); err != nil {
		t.Fatal(err)
	}
	if c.EnvOnly != "from-env" {
		t.Fatalf("env_only: %q", c.EnvOnly)
	}
}

func TestStringSlicesFromEnv(t *testing.T) {
	var c struct {
		Brokers []string `json:"brokers" default:"localhost:9092"`
	}
	if err := Parse([]byte("brokers: [a, b]\n"), &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Brokers) != 2 {
		t.Fatalf("brokers: %v", c.Brokers)
	}
	// absent → default, comma split
	var c2 struct {
		Brokers []string `json:"brokers" default:"localhost:9092"`
	}
	if err := Parse([]byte("other: 1\n"), &c2); err != nil {
		t.Fatal(err)
	}
	if len(c2.Brokers) != 1 || c2.Brokers[0] != "localhost:9092" {
		t.Fatalf("brokers default: %v", c2.Brokers)
	}
}

func TestLoadMapCaseInsensitive(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "cfg*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("HTTP:\n  ADDR: ':9999'\nDemo:\n  Greeting: hi\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	m, err := LoadMap(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	http, ok := m["http"].(map[string]any)
	if !ok || http["addr"] != ":9999" {
		t.Fatalf("http: %#v", m["http"])
	}
	if demo, ok := m["demo"].(map[string]any); !ok || demo["greeting"] != "hi" {
		t.Fatalf("demo: %#v", m["demo"])
	}
}
