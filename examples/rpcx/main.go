// Command rpcx demonstrates pkg/core/rpcx: rpcx needs no IDL and no codegen
// — a service is a plain struct whose exported methods have the signature
//
//	func(ctx context.Context, args *Args, reply *Reply) error
//
// The server is created by verticle.Run from the `rpcx:` section and
// services are registered through the RPCXRegistrar hook; a client is
// dialed in Setup via env.RPCXClient (lazily, unlike grpcx).
//
// Run:
//
//	go run ./examples/rpcx
//
//	curl -i localhost:8080/api/v1/greet/world          # local call
//	curl -i localhost:8080/api/v1/greet-remote/world   # real rpcx round trip (self peer)
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"

	rpcxclient "github.com/smallnest/rpcx/client"
	rpcxserver "github.com/smallnest/rpcx/server"

	"github.com/smallnest/matex/pkg/core/errs"
	"github.com/smallnest/matex/pkg/core/httpx"
	"github.com/smallnest/matex/pkg/core/obs"
	"github.com/smallnest/matex/pkg/core/verticle"
)

// GreetArgs / GreetReply are the payloads. rpcx gob-encodes them, so any
// exported-field struct works — no .proto, no generated stubs.
type GreetArgs struct {
	Name string
}

type GreetReply struct {
	Greeting string
}

// Greeter is the rpcx service. Exported methods with the right signature
// are callable; register it with RegisterName("Greeter", &Greeter{}, "").
type Greeter struct {
	prefix string
}

func (g *Greeter) Greet(ctx context.Context, args *GreetArgs, reply *GreetReply) error {
	if args.Name == "" {
		// Only the message survives the RPC boundary — the client side
		// maps it back to a business code.
		return errs.Invalid(40001, "name is required")
	}
	reply.Greeting = g.prefix + args.Name
	obs.Info(ctx, "greet over rpcx", "name", args.Name)
	return nil
}

type rpcxConfig struct {
	Target string `json:"target" optional:""` // "host:port" or "network@host:port"
	Prefix string `json:"prefix" default:"Hello, "`
}

type rpcxService struct {
	cfg     rpcxConfig
	greeter *Greeter
	client  rpcxclient.XClient
}

func (s *rpcxService) Name() string { return "rpcxdemo" }

func (s *rpcxService) Setup(ctx context.Context, env *verticle.Env) error {
	if err := env.DecodeService(&s.cfg); err != nil {
		return err
	}
	s.greeter = &Greeter{prefix: s.cfg.Prefix}

	if s.cfg.Target != "" {
		c, err := env.RPCXClient("Greeter", s.cfg.Target)
		if err != nil {
			return err
		}
		s.client = c
		obs.Info(ctx, "rpcx client ready", "target", s.cfg.Target)
	}
	return nil
}

// RegisterRPCX is called by verticle.Run before serving, and only when the
// `rpcx:` section is configured.
func (s *rpcxService) RegisterRPCX(srv *rpcxserver.Server) error {
	return srv.RegisterName("Greeter", s.greeter, "")
}

func (s *rpcxService) BuildRouter(srv *httpx.Server) error {
	srv.Handle("GET", "/api/v1/greet/{name}", s.greetLocal)
	srv.Handle("GET", "/api/v1/greet-remote/{name}", s.greetRemote)
	return nil
}

// greetLocal calls the service in-process (no network).
func (s *rpcxService) greetLocal(ctx context.Context, r *http.Request) (any, error) {
	var reply GreetReply
	if err := s.greeter.Greet(ctx, &GreetArgs{Name: r.PathValue("name")}, &reply); err != nil {
		return nil, err
	}
	return map[string]any{"greeting": reply.Greeting, "where": "local"}, nil
}

// greetRemote goes through the real rpcx client.
func (s *rpcxService) greetRemote(ctx context.Context, r *http.Request) (any, error) {
	if s.client == nil {
		return nil, errs.Unavailable(50301, "no rpcx peer configured (set rpcxdemo.target)")
	}
	var reply GreetReply
	if err := s.client.Call(ctx, "Greet", &GreetArgs{Name: r.PathValue("name")}, &reply); err != nil {
		return nil, errs.Unavailable(50302, "rpcx call failed: %v", err)
	}
	return map[string]any{"greeting": reply.Greeting, "where": "remote"}, nil
}

func main() {
	conf := flag.String("conf", "examples/rpcx/config.yaml", "config file")
	flag.Parse()

	if err := verticle.Run(context.Background(), &rpcxService{}, verticle.WithConf(*conf)); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}
