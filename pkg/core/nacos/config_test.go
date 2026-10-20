package nacos

import (
	"reflect"
	"strings"
	"testing"

	"github.com/smallnest/matex/pkg/core/config"
)

func TestValidateRejectsConflictingCredentials(t *testing.T) {
	cases := map[string]Config{
		"no server":              {ServerAddr: ""},
		"login and RAM together": {ServerAddr: "n:8848", Username: "u", AccessKey: "ak", SecretKey: "sk"},
		"access key without key": {ServerAddr: "n:8848", AccessKey: "ak"},
		"unknown type":           {ServerAddr: "n:8848", Type: "json"},
		"bad timeout":            {ServerAddr: "n:8848", Timeout: "soon"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if err := c.Validate(); err == nil {
				t.Fatal("Validate accepted an unusable section")
			}
		})
	}
	ok := []Config{
		{ServerAddr: "n:8848", Timeout: "10s", Type: "yaml"},
		{ServerAddr: "n:8848", Timeout: "10s", Type: "yaml", Username: "u", Password: "p"},
		{ServerAddr: "n:8848", Timeout: "10s", Type: "yaml", AccessKey: "ak", SecretKey: "sk"},
	}
	for _, c := range ok {
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate(%+v) = %v", c, err)
		}
	}
}

// Both a namespace and a group are part of every request; the section
// carries them through to the SDK unchanged.
func TestConfigDecodesNamespaceGroupAndRank(t *testing.T) {
	yaml := `
server_addr: "https://nacos.example.com:8848,nacos2:8848"
namespace: "team-a"
group: "PLATFORM"
data_id: matex.yaml
service_data_id: demo.yaml
username: svc
password: secret
`
	var c Config
	if err := config.Parse([]byte(yaml), &c); err != nil {
		t.Fatal(err)
	}
	if c.Namespace != "team-a" || c.Group != "PLATFORM" || c.ServiceData != "demo.yaml" {
		t.Fatalf("decoded %+v", c)
	}
	servers, err := parseServers(c.ServerAddr)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		scheme, host string
		port         uint64
	}{{"https", "nacos.example.com", 8848}, {"http", "nacos2", 8848}}
	if len(servers) != len(want) {
		t.Fatalf("servers = %+v", servers)
	}
	for i, w := range want {
		if servers[i].Scheme != w.scheme || servers[i].IpAddr != w.host || servers[i].Port != w.port {
			t.Fatalf("servers[%d] = %+v, want %+v", i, servers[i], w)
		}
	}
	cc := newClientConfig(c, "demo")
	if cc.NamespaceId != "team-a" || cc.Username != "svc" || cc.Password != "secret" {
		t.Fatalf("client config lost the section: %+v", cc)
	}
	if !strings.Contains(cc.LogDir, "matex-nacos") || !strings.Contains(cc.CacheDir, "matex-nacos") {
		t.Fatalf("SDK dirs must leave the working directory: %q %q", cc.LogDir, cc.CacheDir)
	}
}

func TestParseServersRejectsBadAddresses(t *testing.T) {
	for _, s := range []string{"", "nacos", "nacos:", "nacos:0", "nacos:notaport", "a:1:2"} {
		if _, err := parseServers(s); err == nil {
			t.Fatalf("parseServers(%q) accepted", s)
		}
	}
}

// AccessKey without a client-side KMS setup still travels: the SDK takes
// the pair from RAM credentials or, when no provider matches, sends it as
// request parameters. It must not land in the Username field.
func TestAccessKeyStaysSeparateFromLogin(t *testing.T) {
	c := Config{ServerAddr: "n:8848", AccessKey: "ak", SecretKey: "sk", Timeout: "1s", Type: "yaml"}
	cc := newClientConfig(c, "demo")
	if cc.AccessKey != "ak" || cc.SecretKey != "sk" || cc.Username != "" || cc.Password != "" {
		t.Fatalf("credentials mixed up: %+v", cc)
	}
}

// The framework body feeds RemoteYAML with the service name, so the
// service's own top-level key never reaches the framework decode.
func TestFrameworkDropsServiceSection(t *testing.T) {
	c := &Client{}
	fw, _, err := c.Framework([]byte("http:\n  addr: \":1\"\ndemo:\n  greeting: hi\n"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fw["demo"]; ok {
		t.Fatalf("service section in framework keys: %v", fw)
	}
	if !reflect.DeepEqual(fw["http"], map[string]any{"addr": ":1"}) {
		t.Fatalf("http = %v", fw["http"])
	}
}

// An absent service DataID (empty content) decodes to nil, which verticle
// reads as "the section stays in the file".
func TestEmptySectionMeansNotConfigured(t *testing.T) {
	c := &Client{}
	svc, err := c.Section(nil, "demo")
	if err != nil || svc != nil {
		t.Fatalf("Section(nil) = %v, %v; want nil, nil", svc, err)
	}
}

// STS credentials keep their security token in RamConfig, the field the
// SDK's token provider matches on.
func TestStsTokenReachesRamConfig(t *testing.T) {
	c := Config{ServerAddr: "n:8848", AccessKey: "ak", SecretKey: "sk", StsToken: "tok", Timeout: "1s", Type: "yaml"}
	cc := newClientConfig(c, "demo")
	if cc.RamConfig == nil || cc.RamConfig.SecurityToken != "tok" {
		t.Fatalf("RamConfig = %+v, want the STS token", cc.RamConfig)
	}
}
