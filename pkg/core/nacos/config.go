// Package nacos is the config-center adapter for pkg/core/verticle: a
// framework config and a service config live on the Nacos server, and
// verticle.Run merges them over the local file.
//
// Two DataIDs, one namespace and group per client:
//
//	matex.yaml          framework keys (http, log, db, redis, ...)
//	<service>.yaml      that service's own section
//
// The service section keeps everything the file watcher already does: the
// local file still hot-reloads it. The framework half is runtime-immutable
// (DB pools, listeners, tracing are built once at boot), so a change to it
// is logged and the process keeps running on the config it booted with;
// restart or roll out to apply it.
//
// The adapter is opt-in: New returns the value passed to verticle.WithConfig.
// A process that does not pass it never contacts Nacos. The verticle package
// depends on this one only through its ConfigSource interface, and business
// code reads the merged config exactly as before (env accessors,
// env.DecodeService, ConfigWatcher).
package nacos

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nacos-group/nacos-sdk-go/v2/clients"
	"github.com/nacos-group/nacos-sdk-go/v2/clients/config_client"
	"github.com/nacos-group/nacos-sdk-go/v2/common/constant"
	"github.com/nacos-group/nacos-sdk-go/v2/vo"

	"github.com/smallnest/matex/pkg/core/config"
	"github.com/smallnest/matex/pkg/core/obs"
)

// ConfigSource is the interface the verticle package declares (as
// verticle.ConfigSource). Client satisfies it; it is restated here so this
// package can prove conformance. Source is a shorter alias for it.
type Source = ConfigSource

// ConfigSource is the seam the verticle package uses. Its interface is
// declared there with the same methods, so neither package imports the
// other.
type ConfigSource interface {
	// Framework derives the framework keys of a remote body for svcName
	// (whose own section is left out), and returns the keys the framework
	// schema does not know.
	Framework(body []byte, svcName string) (map[string]any, []string, error)
	// Section derives a remote service section. An empty body means "this
	// DataID is not configured"; an error means a configured DataID with
	// unusable content.
	Section(body []byte, svcName string) (map[string]any, error)
	// Watch fetches now and calls onChange now and on every later change.
	// It returns once it has called onChange at least once.
	Watch(ctx context.Context, onChange func(fw, svc map[string]any)) error
}

// Config is the `nacos:` section of config.yaml.
type Config struct {
	ServerAddr  string `json:"server_addr" default:"127.0.0.1:8848"`          // comma-separated host:port, optional scheme
	Namespace   string `json:"namespace" optional:""`                         // namespace id; empty is public
	Group       string `json:"group" default:"DEFAULT_GROUP"`                 //
	DataID      string `json:"data_id" default:"matex.yaml"`                  // framework config
	ServiceData string `json:"service_data_id" optional:""`                   // service config; empty: the file's section is used
	Type        string `json:"type" default:"yaml"`                           // yaml | properties | text
	Timeout     string `json:"timeout" default:"10s"`                         // first-fetch budget
	Username    string `json:"username" env:"NACOS_USERNAME" optional:""`     // Nacos plain login
	Password    string `json:"password" env:"NACOS_PASSWORD" optional:""`     //
	AccessKey   string `json:"access_key" env:"NACOS_ACCESS_KEY" optional:""` // Aliyun RAM
	SecretKey   string `json:"secret_key" env:"NACOS_SECRET_KEY" optional:""` //
	StsToken    string `json:"sts_token" env:"NACOS_STS_TOKEN" optional:""`   // temporary RAM credentials
	CacheDir    string `json:"cache_dir" optional:""`                         // SDK snapshot files
	LogDir      string `json:"log_dir" optional:""`                           // SDK log; default <tmp>/matex-nacos/log
	LogLevel    string `json:"log_level" default:"warn"`
}

// Validate rejects settings that would otherwise fail later, inside the SDK.
func (c Config) Validate() error {
	if strings.TrimSpace(c.ServerAddr) == "" {
		return errors.New("nacos: server_addr is required")
	}
	if c.Username != "" && c.AccessKey != "" {
		return errors.New("nacos: set username/password or access_key/secret_key, not both")
	}
	if (c.AccessKey == "") != (c.SecretKey == "") {
		return errors.New("nacos: access_key and secret_key must be set together")
	}
	switch c.Type {
	case "yaml", "properties", "text":
	default:
		return fmt.Errorf("nacos: type %q must be yaml, properties or text", c.Type)
	}
	if _, err := time.ParseDuration(c.Timeout); err != nil {
		return fmt.Errorf("nacos: timeout: %w", err)
	}
	return nil
}

func (c Config) readyTimeout() time.Duration {
	d, err := time.ParseDuration(c.Timeout)
	if err != nil || d <= 0 {
		return 10 * time.Second
	}
	return d
}

// Client is this process's connection to Nacos. Changes arrive on one
// reconciler goroutine, so every reload is serial and the last one wins.
type Client struct {
	cfg      Config
	svcName  string
	cc       config_client.IConfigClient
	ready    chan struct{} // closed after the first onChange
	done     context.CancelFunc
	reloadMu sync.Mutex
}

// New opens a client from the `nacos:` section. It does not fetch: Watch
// does, so the boot path has one place to wait on.
// serviceName becomes the SDK AppName (visible in the Nacos console) and the
// lowercased name whose section the framework body must not carry.
func New(cfg Config, serviceName string) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	servers, err := parseServers(cfg.ServerAddr)
	if err != nil {
		return nil, err
	}
	cc, err := clients.NewConfigClient(vo.NacosClientParam{
		ClientConfig:  newClientConfig(cfg, serviceName),
		ServerConfigs: servers,
	})
	if err != nil {
		return nil, fmt.Errorf("nacos: create client: %w", err)
	}
	return &Client{cfg: cfg, svcName: strings.ToLower(serviceName), cc: cc, ready: make(chan struct{})}, nil
}

// clientConfig maps the section onto the SDK. LogDir and CacheDir are
// always set: the SDK defaults both to directories under the working
// directory (./log and ./cache).
func newClientConfig(c Config, serviceName string) *constant.ClientConfig {
	return &constant.ClientConfig{
		NamespaceId: c.Namespace,
		TimeoutMs:   uint64(c.readyTimeout().Milliseconds()),
		Username:    c.Username,
		Password:    c.Password,
		AccessKey:   c.AccessKey,
		SecretKey:   c.SecretKey,
		RamConfig:   ramConfig(c),
		LogLevel:    c.LogLevel,
		LogDir:      firstNonEmpty(c.LogDir, filepath.Join(os.TempDir(), "matex-nacos", "log")),
		CacheDir:    firstNonEmpty(c.CacheDir, filepath.Join(os.TempDir(), "matex-nacos", "cache")),
		AppName:     serviceName,
	}
}

// ramConfig pins the ephemeral-credential flag the SDK derives from a
// security token, so an STS pair signs as temporary credentials instead of
// long-term ones.
func ramConfig(c Config) *constant.RamConfig {
	if c.StsToken == "" {
		return nil
	}
	return &constant.RamConfig{SecurityToken: c.StsToken}
}

func firstNonEmpty(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

var schemeRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://`)

// parseServers accepts "host:port[,host:port]" with an optional per-entry
// scheme ("https://nacos.example.com:8848"). The gRPC port defaults to the
// HTTP port +1000, the Nacos convention.
func parseServers(s string) ([]constant.ServerConfig, error) {
	var out []constant.ServerConfig
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		sc := constant.ServerConfig{Scheme: "http"}
		if scheme := schemeRe.FindString(part); scheme != "" {
			sc.Scheme = strings.TrimSuffix(scheme, "://")
			part = strings.TrimPrefix(part, scheme)
		}
		host, portStr, ok := strings.Cut(part, ":")
		if !ok || host == "" || strings.Contains(portStr, ":") {
			return nil, fmt.Errorf("nacos: server_addr %q: want host:port", part)
		}
		port, err := strconv.ParseUint(portStr, 10, 16)
		if err != nil || port == 0 {
			return nil, fmt.Errorf("nacos: server_addr %q: invalid port", part)
		}
		sc.IpAddr, sc.Port = host, port
		out = append(out, sc)
	}
	if len(out) == 0 {
		return nil, errors.New("nacos: server_addr is empty")
	}
	return out, nil
}

func (c *Client) param(dataID string) vo.ConfigParam {
	return vo.ConfigParam{DataId: dataID, Group: c.cfg.Group, Type: c.cfg.Type}
}

// Framework implements ConfigSource.
func (c *Client) Framework(body []byte, svcName string) (map[string]any, []string, error) {
	return config.RemoteYAML(body, svcName, nil)
}

// Section implements ConfigSource. An empty body means the DataID does not
// exist (Nacos answers a missing key with empty content), which verticle
// reads as "the section stays in the file".
func (c *Client) Section(body []byte, svcName string) (map[string]any, error) {
	if strings.TrimSpace(string(body)) == "" {
		return nil, nil
	}
	return config.Extract(string(body), svcName)
}

// Watch implements ConfigSource. It fetches both DataIDs once and calls
// onChange before returning, then subscribes; every later notification
// triggers a full re-fetch so the framework keys and the service section
// always come from one generation of the remote config. Cancelling ctx
// closes the client, which stops the listeners.
func (c *Client) Watch(ctx context.Context, onChange func(fw, svc map[string]any)) error {
	fw, svc, err := c.load()
	if err != nil {
		return err
	}
	onChange(fw, svc)

	var wctx context.Context
	wctx, c.done = context.WithCancel(context.Background())
	context.AfterFunc(ctx, c.Close)
	fire := make(chan struct{}, 1)
	poke := func() {
		select {
		case fire <- struct{}{}:
		default: // a reload is already queued; it will fetch the latest
		}
	}
	ids := []string{c.cfg.DataID}
	if c.cfg.ServiceData != "" {
		ids = append(ids, c.cfg.ServiceData)
	}
	for _, id := range ids {
		p := c.param(id)
		p.OnChange = func(_, _, _, _ string) { poke() }
		if err := c.cc.ListenConfig(p); err != nil {
			c.done()
			return fmt.Errorf("nacos: listen %s: %w", id, err)
		}
	}
	go c.reconcile(wctx, fire, onChange)
	return nil
}

// load fetches and parses both DataIDs. An absent service DataID (empty
// content) is not an error: it means the service section stays in the file.
func (c *Client) load() (map[string]any, map[string]any, error) {
	body, err := c.cc.GetConfig(c.param(c.cfg.DataID))
	if err != nil {
		return nil, nil, fmt.Errorf("nacos: get %s: %w", c.cfg.DataID, err)
	}
	if strings.TrimSpace(body) == "" {
		return nil, nil, fmt.Errorf("nacos: config %s not found (namespace %q, group %q)", c.cfg.DataID, c.cfg.Namespace, c.cfg.Group)
	}
	fw, unknown, err := c.Framework([]byte(body), c.svcName)
	if err != nil {
		return nil, nil, fmt.Errorf("nacos: %s: %w", c.cfg.DataID, err)
	}
	for _, k := range unknown {
		obs.Warn(context.Background(), "unknown framework config key (nacos)", "key", k, "data_id", c.cfg.DataID)
	}
	var svc map[string]any
	if c.cfg.ServiceData != "" {
		sbody, err := c.cc.GetConfig(c.param(c.cfg.ServiceData))
		if err != nil {
			return nil, nil, fmt.Errorf("nacos: get %s: %w", c.cfg.ServiceData, err)
		}
		if svc, err = c.Section([]byte(sbody), c.svcName); err != nil {
			return nil, nil, fmt.Errorf("nacos: %s: %w", c.cfg.ServiceData, err)
		}
	}
	return fw, svc, nil
}

// reconcile turns notifications into reloads, one at a time.
func (c *Client) reconcile(ctx context.Context, fire <-chan struct{}, onChange func(fw, svc map[string]any)) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-fire:
		}
		c.reloadMu.Lock()
		fw, svc, err := c.load()
		if err != nil {
			// Keep running on the previous generation: a half-broken
			// remote edit must not take the service down or apply half.
			obs.Warn(ctx, "nacos config reload failed; keeping the previous config", "err", err)
			c.reloadMu.Unlock()
			continue
		}
		c.reloadMu.Unlock()
		onChange(fw, svc)
	}
}

// Timeout is the first-fetch budget; verticle.ConfigSource implementations
// with a Timeout() method let Run use it instead of a fixed 10s.
func (c *Client) Timeout() time.Duration { return c.cfg.readyTimeout() }

// Close stops the reconciler and the SDK listeners. Safe to call twice.
func (c *Client) Close() {
	if c.done != nil {
		c.done()
	}
	c.cc.CloseClient()
}

var _ ConfigSource = (*Client)(nil)
