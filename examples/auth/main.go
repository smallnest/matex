// Command auth demonstrates the matex authentication layer
// (pkg/core/auth) plugged into the httpx middleware slot.
//
// The `auth` section of the config builds env.Auth; the service decides
// which routes it guards, and the middleware speaks the envelope — no
// handler touches the Authorization header, no handler writes a 401 by
// hand.
//
// Run:
//
//	go run ./examples/auth
//
//	TOKEN=$(curl -s -X POST localhost:8081/api/v1/login \
//	  -d '{"username":"bob","password":"bob-pw"}' | sed 's/.*"token":"\([^"]*\)".*/\1/')
//
//	curl -i localhost:8081/api/v1/profile                                   # 401
//	curl -i localhost:8081/api/v1/profile     -H "Authorization: Bearer $TOKEN"
//	curl -i localhost:8081/api/v1/admin/audit -H "Authorization: Bearer $TOKEN"
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/smallnest/matex/pkg/core/auth"
	"github.com/smallnest/matex/pkg/core/errs"
	"github.com/smallnest/matex/pkg/core/httpx"
	"github.com/smallnest/matex/pkg/core/verticle"
)

// demoUser stands in for whatever user store a real service would use.
type demoUser struct {
	password string
	scopes   []string
	roles    []string
}

var demoUsers = map[string]demoUser{
	"alice": {password: "alice-pw", scopes: []string{"profile:read"}, roles: []string{"user"}},
	"bob":   {password: "bob-pw", scopes: []string{"profile:read", "audit:read"}, roles: []string{"admin"}},
}

type authDemo struct {
	verifier *auth.Verifier
}

func (s *authDemo) Name() string { return "authdemo" }

func (s *authDemo) Setup(_ context.Context, env *verticle.Env) error {
	if env.Auth == nil {
		return errors.New("authdemo: the `auth` section must be configured")
	}
	s.verifier = env.Auth
	return nil
}

func (s *authDemo) BuildRouter(srv *httpx.Server) error {
	// Public: the one route that hands out tokens.
	srv.Handle("POST", "/api/v1/login", s.login)

	// Everything below this line needs a valid token.
	srv.Use(s.verifier.Middleware())
	srv.Handle("GET", "/api/v1/profile", profile)

	// Middleware accumulates, so this route needs the token *and* the
	// admin role. That is the whole authorization model: register public
	// routes, then Use, then the next tier.
	srv.Use(auth.RequireRole("admin"))
	srv.Handle("GET", "/api/v1/admin/audit", audit)

	return nil
}

// login trades credentials for a token. Note it never inspects a token
// itself — issuing is the verifier's job.
func (s *authDemo) login(_ context.Context, r *http.Request) (any, error) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := httpx.ReadJSON(r, &req); err != nil {
		return nil, errs.Invalid(40001, "bad request body: %v", err)
	}
	u, ok := demoUsers[req.Username]
	if !ok || u.password != req.Password {
		// Same message for "no such user" and "wrong password": telling
		// them apart hands an attacker a user-enumeration oracle.
		return nil, errs.Unauthorized(40101, "invalid credentials")
	}
	token, err := s.verifier.Issue(auth.IssueRequest{
		Subject: req.Username,
		Scopes:  u.scopes,
		Roles:   u.roles,
	}, time.Hour)
	if err != nil {
		return nil, errs.Internal(50001, "issue token: %v", err)
	}
	return map[string]any{"token": token, "token_type": "Bearer", "expires_in": 3600}, nil
}

// profile reads the principal the middleware put on the context.
func profile(ctx context.Context, _ *http.Request) (any, error) {
	p, _ := auth.FromContext(ctx)
	return map[string]any{"subject": p.Subject, "scopes": p.Scopes, "roles": p.Roles}, nil
}

// audit requires the admin role, which RequireRole already enforced.
func audit(ctx context.Context, _ *http.Request) (any, error) {
	p, _ := auth.FromContext(ctx)
	return map[string]any{
		"requested_by": p.Subject,
		"entries":      []string{"alice logged in", "bob rotated the signing key"},
	}, nil
}

func main() {
	conf := flag.String("conf", "examples/auth/config.yaml", "config file")
	flag.Parse()

	if err := verticle.Run(context.Background(), &authDemo{}, verticle.WithConf(*conf)); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}
