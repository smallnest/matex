// Command http demonstrates the matex web layer (pkg/core/httpx): a
// framework-free net/http server whose handlers never touch the
// ResponseWriter.
//
//	func(ctx context.Context, r *http.Request) (any, error)
//
// The wrapper owns everything else: panic recovery, per-request timeout,
// trace id, access log, metrics, JSON rendering and error mapping.
//
// Run:
//
//	go run ./examples/http
//
//	curl -i localhost:8080/api/v1/echo/world
//	curl -i localhost:8080/api/v1/nothing
//	curl -i localhost:8080/api/v1/errors/notfound
//	curl -i -X POST localhost:8080/api/v1/users -d '{"name":"alice"}'   # → 401, needs the key
//	curl -i -X POST localhost:8080/api/v1/users -H 'X-API-Key: demo-key' -d '{"name":"alice"}'
//	curl -i localhost:8080/api/v1/admin/whoami -H 'X-API-Key: demo-key' -H 'X-Tag: alice'
//	curl -i localhost:8080/raw/plain
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"

	"github.com/smallnest/matex/pkg/core/errs"
	"github.com/smallnest/matex/pkg/core/httpx"
	"github.com/smallnest/matex/pkg/core/verticle"
)

type httpConfig struct {
	Greeting string `json:"greeting" default:"Hello, "`
	APIKey   string `json:"api_key" default:"demo-key"`
}

type httpService struct {
	cfg httpConfig
}

func (s *httpService) Name() string { return "httpdemo" }

func (s *httpService) Setup(_ context.Context, env *verticle.Env) error {
	return env.DecodeService(&s.cfg)
}

func (s *httpService) BuildRouter(srv *httpx.Server) error {
	// Outer middleware wraps the mux itself, so its header lands on every
	// response — including HandleRaw routes and the builtin probes.
	srv.UseOuter(serverHeader("matex/http-demo"))

	// Public routes: registered before any Use, so nothing guards them.
	srv.Handle("GET", "/api/v1/echo/{name}", s.echo)
	srv.Handle("GET", "/api/v1/nothing", nothing)
	srv.Handle("GET", "/api/v1/errors/{kind}", fail)
	// Escape hatch: full control over status/headers/body.
	srv.HandleRaw("GET /raw/plain", rawPlain)

	// Everything below this line runs through both middleware, outermost
	// first: tag the request, then demand the API key.
	srv.Use(withTag, s.requireAPIKey)
	srv.Handle("POST", "/api/v1/users", createUser)
	srv.Handle("GET", "/api/v1/admin/whoami", whoami)
	return nil
}

// serverHeader is an outer middleware: it needs the ResponseWriter, so it
// lives outside the mux rather than in the envelope chain.
func serverHeader(v string) httpx.OuterMiddleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Served-By", v)
			next.ServeHTTP(w, r)
		})
	}
}

type tagKey struct{}

// withTag is an envelope middleware: it enriches the request context so
// the handler can read the value. No ResponseWriter in sight.
func withTag(next httpx.HandlerFunc) httpx.HandlerFunc {
	return func(ctx context.Context, r *http.Request) (any, error) {
		return next(context.WithValue(ctx, tagKey{}, r.Header.Get("X-Tag")), r)
	}
}

// requireAPIKey short-circuits with an errs value — the wrapper turns it
// into 401 {"code":40101,...}. An empty configured key disables the check
// (matex convention: unconfigured means off).
func (s *httpService) requireAPIKey(next httpx.HandlerFunc) httpx.HandlerFunc {
	return func(ctx context.Context, r *http.Request) (any, error) {
		if s.cfg.APIKey != "" && r.Header.Get("X-API-Key") != s.cfg.APIKey {
			return nil, errs.Unauthorized(40101, "missing or invalid X-API-Key")
		}
		return next(ctx, r)
	}
}

// whoami proves ctx propagation: the tag injected by withTag is visible
// here, in the route handler.
func whoami(ctx context.Context, _ *http.Request) (any, error) {
	return map[string]any{"greeting": "you are behind both middleware", "tag": ctx.Value(tagKey{})}, nil
}

// echo shows path values and the success envelope.
func (s *httpService) echo(_ context.Context, r *http.Request) (any, error) {
	name := r.PathValue("name")
	if name == "" {
		return nil, errs.Invalid(40001, "name is required")
	}
	return map[string]string{"greeting": s.cfg.Greeting + name + "!"}, nil
}

// nothing returns a nil payload → the wrapper answers 204 No Content.
func nothing(context.Context, *http.Request) (any, error) { return nil, nil }

type createUserRequest struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// createUser shows body decoding and input validation. Note the error
// carries a business code; the wrapper renders status + {code,msg}.
func createUser(_ context.Context, r *http.Request) (any, error) {
	var req createUserRequest
	if err := httpx.ReadJSON(r, &req); err != nil {
		return nil, errs.Invalid(40002, "bad request body: %v", err)
	}
	if req.Name == "" {
		return nil, errs.Invalid(40001, "name is required")
	}
	return map[string]any{"id": 1, "name": req.Name, "email": req.Email}, nil
}

// fail maps a path segment to one errs kind per HTTP status.
func fail(_ context.Context, r *http.Request) (any, error) {
	switch r.PathValue("kind") {
	case "invalid":
		return nil, errs.Invalid(40001, "name is required")
	case "unauthorized":
		return nil, errs.Unauthorized(40101, "token expired")
	case "forbidden":
		return nil, errs.Forbidden(40301, "not your resource")
	case "notfound":
		return nil, errs.NotFound(40401, "user %d not found", 42)
	case "conflict":
		return nil, errs.Conflict(40901, "name already taken")
	case "ratelimited":
		return nil, errs.RateLimited(42901, "too many requests")
	case "internal":
		return nil, errs.Internal(50001, "boom")
	case "unavailable":
		return nil, errs.Unavailable(50301, "database is not configured")
	case "timeout":
		return nil, errs.Timeout(50401, "upstream timeout")
	default:
		return nil, errs.Invalid(40000, "unknown kind %q", r.PathValue("kind"))
	}
}

// rawPlain is a plain http.HandlerFunc: no envelope, any status you like.
func rawPlain(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte("raw handler: full control over status/headers/body\n"))
}

func main() {
	conf := flag.String("conf", "examples/http/config.yaml", "config file")
	flag.Parse()

	if err := verticle.Run(context.Background(), &httpService{}, verticle.WithConf(*conf)); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}
