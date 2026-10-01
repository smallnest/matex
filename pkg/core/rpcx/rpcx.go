// Package rpcx wraps github.com/smallnest/rpcx with a verticle-friendly
// server (graceful shutdown) and client helpers. Business code registers
// its rpcx services through the verticle.RPCXRegistrar hook; the server
// lifecycle is owned here, and clients are created via Env so verticle
// closes them on shutdown.
//
// rpcx needs no IDL: services are plain structs whose exported methods
// have the signature func(ctx context.Context, args, reply) error.
package rpcx

import (
	"context"
	"errors"
	"fmt"
	"strings"

	rpcxclient "github.com/smallnest/rpcx/client"
	rpcxserver "github.com/smallnest/rpcx/server"
)

// Config configures the rpcx server.
type Config struct {
	Network string `json:"network" default:"tcp"` // tcp | quic | kcp | ws | http
	Addr    string `json:"addr" optional:""`
}

// Server wraps a *rpcxserver.Server.
type Server struct {
	srv     *rpcxserver.Server
	network string
	addr    string
}

// New creates a server. It does not bind until Serve is called.
func New(cfg Config) *Server {
	network := cfg.Network
	if network == "" {
		network = "tcp"
	}
	return &Server{srv: rpcxserver.NewServer(), network: network, addr: cfg.Addr}
}

// RPCX exposes the underlying server for service registration.
func (s *Server) RPCX() *rpcxserver.Server { return s.srv }

// Addr returns the listen address as "network@host:port".
func (s *Server) Addr() string { return s.network + "@" + s.addr }

// Serve blocks serving. Returns nil on graceful shutdown.
func (s *Server) Serve() error {
	err := s.srv.Serve(s.network, s.addr)
	if errors.Is(err, rpcxserver.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown gracefully stops (drains in-flight requests within ctx).
func (s *Server) Shutdown(ctx context.Context) error { return s.srv.Shutdown(ctx) }

// PeerClient creates a client to a single rpcx server. target is
// "network@host:port" (e.g. "tcp@user-service:8972"); the network
// defaults to tcp when omitted. Strategy: Failtry + RandomSelect.
func PeerClient(servicePath, target string) (rpcxclient.XClient, error) {
	if !strings.Contains(target, "@") {
		target = "tcp@" + target
	}
	d, err := rpcxclient.NewPeer2PeerDiscovery(target, "")
	if err != nil {
		return nil, fmt.Errorf("rpcx: discovery %s: %w", target, err)
	}
	return NewClient(servicePath, d), nil
}

// NewClient creates a client with a custom discovery (etcd, consul,
// multiple servers, ...). The caller owns the returned client's lifecycle.
func NewClient(servicePath string, d rpcxclient.ServiceDiscovery) rpcxclient.XClient {
	return rpcxclient.NewXClient(servicePath, rpcxclient.Failtry, rpcxclient.RandomSelect, d, rpcxclient.DefaultOption)
}
