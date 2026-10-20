package config

import (
	"reflect"
	"testing"
)

// RemoteYAML keeps the framework half of a remote body and never lets the
// service's own section leak into it.
func TestRemoteYAMLKeepsFrameworkKeysOnly(t *testing.T) {
	body := []byte(`
http:
  addr: ":9000"
log:
  level: info
app:
  greeting: remote
`)

	raw, unknown, err := RemoteYAML(body, "app", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["app"]; ok {
		t.Fatal("service section leaked into the framework keys")
	}
	if got := raw["http"].(map[string]any)["addr"]; got != ":9000" {
		t.Fatalf("http.addr = %v, want :9000", got)
	}
	if len(unknown) != 0 {
		t.Fatalf("unknown = %v, want none without a schema", unknown)
	}
}

func TestRemoteYAMLReportsUnknownFrameworkKeys(t *testing.T) {
	var schema struct {
		HTTP struct {
			Addr string `json:"addr"`
		} `json:"http"`
	}
	raw, unknown, err := RemoteYAML([]byte("http:\n  addr: \":1\"\n  adr: \":2\"\n"), "app", &schema)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 1 {
		t.Fatalf("raw = %v", raw)
	}
	if !reflect.DeepEqual(unknown, []string{"http.adr"}) {
		t.Fatalf("unknown = %v, want [http.adr]", unknown)
	}
}

func TestRemoteYAMLRejectsBrokenYAML(t *testing.T) {
	if _, _, err := RemoteYAML([]byte("http: [unclosed"), "app", nil); err == nil {
		t.Fatal("broken remote YAML must be an error, not an empty config")
	}
}

// Extract understands the three shapes a Nacos doc can take.
func TestExtractSectionShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want map[string]any
	}{
		{
			name: "yaml with sections",
			body: "app:\n  port: 8080\nother:\n  x: 1\n",
			want: map[string]any{"port": 8080},
		},
		{
			name: "yaml of the section's own keys",
			body: "port: 8080\ngreeting: hi\n",
			want: map[string]any{"port": 8080, "greeting": "hi"},
		},
		{
			name: "properties with the section prefix",
			body: "app.port=8080\napp.db.dsn=postgres://x\nother.y=2\n",
			want: map[string]any{"port": "8080", "db": map[string]any{"dsn": "postgres://x"}},
		},
		{
			name: "properties without a prefix",
			body: "port=8080\ngreeting=hi: there\n",
			want: map[string]any{"port": "8080", "greeting": "hi: there"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Extract(c.body, "app")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %#v, want %#v", got, c.want)
			}
		})
	}
}

func TestExtractRejectsUnusableBodies(t *testing.T) {
	cases := map[string]string{
		"empty body":           "  \n",
		"whole framework file": "remote:\n  addr: nacos:8848\nhttp:\n  addr: \":1\"\n",
		"binary payload":       "dataId=matex\ngroup=DEFAULT_GROUP\ncontent=...\n",
		"broken yaml":          "app: [",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if got, err := Extract(body, "app"); err == nil {
				t.Fatalf("got %v, want an error", got)
			}
		})
	}
}

// Merge is a deep overlay: a remote section sets one field without wiping
// the file's other fields, and neither input is modified.
func TestMergeOverlaysNestedMaps(t *testing.T) {
	file := map[string]any{
		"http":  map[string]any{"addr": ":8080", "timeout": "10s"},
		"redis": map[string]any{"addr": "localhost:6379", "password": "local"},
		"log":   map[string]any{"level": "info"},
	}
	remote := map[string]any{
		"http":  map[string]any{"addr": ":9000"},
		"redis": map[string]any{"addr": "redis.prod:6379"},
	}
	got := Merge(file, remote)

	if addr := got["http"].(map[string]any); addr["addr"] != ":9000" || addr["timeout"] != "10s" {
		t.Fatalf("http = %v, want remote addr and file timeout", addr)
	}
	if r := got["redis"].(map[string]any); r["addr"] != "redis.prod:6379" || r["password"] != "local" {
		t.Fatalf("redis = %v, want remote addr and file password", r)
	}
	if l := got["log"].(map[string]any); l["level"] != "info" {
		t.Fatalf("file-only section lost: %v", got)
	}
	if file["http"].(map[string]any)["addr"] != ":8080" {
		t.Fatal("Merge modified the file map")
	}
}
