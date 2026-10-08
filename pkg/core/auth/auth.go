// Package auth is the matex authentication layer: it verifies bearer
// tokens and exposes the authenticated caller as a Principal on the
// request context.
//
// Two verification modes, picked by which field is configured:
//
//	secret     HS256/HS384/HS512 with a shared secret (internal callers, tests)
//	jwks_url   RS256/ES256/… with keys fetched from a JWKS endpoint (OIDC)
//
// The middleware speaks the httpx envelope — a request without a valid
// token is rejected with errs.Unauthorized before any handler runs, and
// route handlers read the caller from the context:
//
//	srv.Use(v.Middleware(), auth.RequireScope("orders:read"))
//	srv.Handle("GET", "/api/v1/orders", listOrders)
//
//	func listOrders(ctx context.Context, r *http.Request) (any, error) {
//		p, _ := auth.FromContext(ctx)
//		return orders.List(ctx, p.Subject)
//	}
//
// Handlers never parse the Authorization header themselves, and nothing
// in this package touches the ResponseWriter — that is the whole point of
// building on the envelope middleware slot.
package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/smallnest/matex/pkg/core/errs"
	"github.com/smallnest/matex/pkg/core/httpx"
)

// Business codes for the auth failures. They follow the errs scheme
// (<status>*100 + sub) and are stable across services so clients can
// branch on them.
const (
	CodeUnauthorized = 40101 // 401: no token, malformed, expired, bad signature
	CodeForbidden    = 40301 // 403: authenticated but not allowed
	CodeUnavailable  = 50301 // 503: the identity provider could not be reached
)

// Config configures a Verifier.
type Config struct {
	// Issuer, when set, must equal the token's `iss`.
	Issuer string `json:"issuer" optional:""`
	// Audience, when set, must be present in the token's `aud`.
	Audience string `json:"audience" optional:""`
	// Secret enables HMAC verification (HS256/384/512). Use it for
	// internal callers and tests, not for public traffic.
	Secret string `json:"secret" optional:""`
	// JWKSURL enables asymmetric verification, fetching the key set from
	// this endpoint (e.g. https://idp/.well-known/jwks.json).
	JWKSURL string `json:"jwks_url" optional:""`

	// Header and Scheme locate the token (default Authorization / Bearer).
	Header string `json:"header" default:"Authorization"`
	Scheme string `json:"scheme" default:"Bearer"`
	// Leeway tolerates clock skew when checking exp/nbf.
	Leeway time.Duration `json:"leeway" default:"30s"`
	// JWKSTTL is how long a fetched JWKS document is reused.
	JWKSTTL time.Duration `json:"jwks_ttl" default:"10m"`
	// JWKSClient overrides the HTTP client used for JWKS fetches.
	JWKSClient *http.Client `json:"-"`
}

// Principal is the authenticated caller: the token's identity claims in a
// form handlers can use without touching the JWT library.
type Principal struct {
	Subject string
	Issuer  string
	Scopes  []string
	Roles   []string
	// Claims is the raw claim set, for anything the fields above miss.
	Claims map[string]any
}

// HasScope reports whether the principal carries scope.
func (p *Principal) HasScope(scope string) bool { return contains(p.Scopes, scope) }

// HasRole reports whether the principal carries role.
func (p *Principal) HasRole(role string) bool { return contains(p.Roles, role) }

// HasAnyRole reports whether the principal carries at least one of roles.
func (p *Principal) HasAnyRole(roles ...string) bool {
	for _, r := range roles {
		if p.HasRole(r) {
			return true
		}
	}
	return false
}

type principalKey struct{}

// WithPrincipal returns a context carrying p.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// FromContext returns the principal installed by the middleware.
func FromContext(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(*Principal)
	return p, ok
}

// Verifier validates bearer tokens.
type Verifier struct {
	cfg  Config
	jwks *keySet // non-nil only in JWKS mode
}

// New builds a Verifier. Exactly one of Secret and JWKSURL must be set.
func New(cfg Config) (*Verifier, error) {
	if (cfg.Secret == "") == (cfg.JWKSURL == "") {
		return nil, errors.New("auth: configure exactly one of secret or jwks_url")
	}
	if cfg.Header == "" {
		cfg.Header = "Authorization"
	}
	if cfg.Scheme == "" {
		cfg.Scheme = "Bearer"
	}
	if cfg.Leeway <= 0 {
		cfg.Leeway = 30 * time.Second
	}
	if cfg.JWKSTTL <= 0 {
		cfg.JWKSTTL = 10 * time.Minute
	}
	v := &Verifier{cfg: cfg}
	if cfg.JWKSURL != "" {
		v.jwks = newKeySet(cfg.JWKSURL, cfg.JWKSTTL, cfg.JWKSClient)
	}
	return v, nil
}

// Verify validates a raw token and returns the caller it identifies.
// Errors are errs.Unauthorized so the HTTP layer maps them to 401.
func (v *Verifier) Verify(ctx context.Context, raw string) (*Principal, error) {
	if raw == "" {
		return nil, errs.Unauthorized(CodeUnauthorized, "missing token")
	}
	claims := jwt.MapClaims{}
	_, err := jwt.NewParser(v.parseOptions()...).ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		return v.key(ctx, t)
	})
	if err != nil {
		// Reaching the key source is our problem, not the caller's: an
		// unreachable JWKS endpoint must not masquerade as a bad token.
		if errors.Is(err, errKeySource) {
			return nil, errs.Wrap(errs.KindUnavailable, CodeUnavailable, err, "identity provider unavailable")
		}
		return nil, errs.Wrap(errs.KindUnauthorized, CodeUnauthorized, err, "invalid token")
	}
	return principalFrom(claims), nil
}

// Middleware authenticates every request. Requests without a token, or
// with an invalid one, are rejected before the handler runs.
func (v *Verifier) Middleware() httpx.Middleware {
	return func(next httpx.HandlerFunc) httpx.HandlerFunc {
		return func(ctx context.Context, r *http.Request) (any, error) {
			raw, err := v.bearerToken(r)
			if err != nil {
				return nil, err
			}
			if raw == "" {
				return nil, errs.Unauthorized(CodeUnauthorized, "missing token")
			}
			p, err := v.Verify(ctx, raw)
			if err != nil {
				return nil, err
			}
			return next(WithPrincipal(ctx, p), r)
		}
	}
}

// Optional authenticates when a token is present but lets anonymous
// requests through — for public endpoints that personalise for signed-in
// callers. A *malformed* token is still rejected: silently ignoring a bad
// credential hides bugs.
func (v *Verifier) Optional() httpx.Middleware {
	return func(next httpx.HandlerFunc) httpx.HandlerFunc {
		return func(ctx context.Context, r *http.Request) (any, error) {
			raw, err := v.bearerToken(r)
			if err != nil || raw == "" {
				return next(ctx, r)
			}
			p, err := v.Verify(ctx, raw)
			if err != nil {
				return nil, err
			}
			return next(WithPrincipal(ctx, p), r)
		}
	}
}

// Config returns the effective configuration (defaults applied).
func (v *Verifier) Config() Config { return v.cfg }

// bearerToken extracts the raw token. It returns ("", nil) when the header
// is absent, so callers can tell "no credential" from "malformed".
func (v *Verifier) bearerToken(r *http.Request) (string, error) {
	raw := r.Header.Get(v.cfg.Header)
	if raw == "" {
		return "", nil
	}
	prefix := v.cfg.Scheme + " "
	if !strings.HasPrefix(raw, prefix) {
		return "", errs.Unauthorized(CodeUnauthorized, "credential must use the %s scheme", v.cfg.Scheme)
	}
	token := strings.TrimSpace(strings.TrimPrefix(raw, prefix))
	if token == "" {
		return "", errs.Unauthorized(CodeUnauthorized, "missing token")
	}
	return token, nil
}

// parseOptions pins the acceptable algorithms. The whitelist is what
// prevents the classic algorithm-confusion attack (an HS256 token signed
// with the public key, presented to a verifier configured for RS256).
func (v *Verifier) parseOptions() []jwt.ParserOption {
	opts := []jwt.ParserOption{
		jwt.WithValidMethods(v.allowedMethods()),
		jwt.WithLeeway(v.cfg.Leeway),
		jwt.WithExpirationRequired(),
	}
	if v.cfg.Issuer != "" {
		opts = append(opts, jwt.WithIssuer(v.cfg.Issuer))
	}
	if v.cfg.Audience != "" {
		opts = append(opts, jwt.WithAudience(v.cfg.Audience))
	}
	return opts
}

func (v *Verifier) allowedMethods() []string {
	if v.cfg.Secret != "" {
		return []string{"HS256", "HS384", "HS512"}
	}
	return []string{"RS256", "RS384", "RS512", "ES256", "ES384", "ES512"}
}

// key resolves the verification key for a token: the shared secret in
// HMAC mode, the JWKS key matching the token's kid otherwise.
func (v *Verifier) key(ctx context.Context, t *jwt.Token) (any, error) {
	if v.cfg.Secret != "" {
		return []byte(v.cfg.Secret), nil
	}
	kid, _ := t.Header["kid"].(string)
	return v.jwks.key(ctx, t.Method.Alg(), kid)
}

// IssueRequest describes a token to mint. Fields are optional except
// Subject; empty slices and maps are simply omitted from the claims.
type IssueRequest struct {
	Subject string
	Scopes  []string
	Roles   []string
	// Extra merges additional claims. Reserved claims (sub, iat, exp, iss,
	// aud) cannot be overridden from here.
	Extra map[string]any
}

// Issue builds a signed token. It exists so services can mint tokens for
// internal callers and tests without importing a JWT library themselves.
// It only works in HMAC mode — with asymmetric keys the issuer holds the
// private key, not the service verifying tokens.
func (v *Verifier) Issue(req IssueRequest, ttl time.Duration) (string, error) {
	if v.cfg.Secret == "" {
		return "", errors.New("auth: Issue requires HMAC mode (secret)")
	}
	if req.Subject == "" {
		return "", errors.New("auth: Issue requires a subject")
	}
	if ttl <= 0 {
		return "", errors.New("auth: Issue requires a positive ttl")
	}
	now := time.Now()
	claims := jwt.MapClaims{
		"sub": req.Subject,
		"iat": now.Unix(),
		"exp": now.Add(ttl).Unix(),
	}
	if len(req.Scopes) > 0 {
		claims["scope"] = strings.Join(req.Scopes, " ")
	}
	if len(req.Roles) > 0 {
		claims["roles"] = req.Roles
	}
	for k, val := range req.Extra {
		switch k {
		case "sub", "iat", "exp", "iss", "aud":
			continue // reserved: never let Extra forge identity or lifetime
		}
		claims[k] = val
	}
	if v.cfg.Issuer != "" {
		claims["iss"] = v.cfg.Issuer
	}
	if v.cfg.Audience != "" {
		claims["aud"] = v.cfg.Audience
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(v.cfg.Secret))
}

// RequireScope rejects the request unless the principal carries every
// listed scope. Use it after an authenticating middleware.
func RequireScope(scopes ...string) httpx.Middleware {
	return func(next httpx.HandlerFunc) httpx.HandlerFunc {
		return func(ctx context.Context, r *http.Request) (any, error) {
			p, err := mustPrincipal(ctx)
			if err != nil {
				return nil, err
			}
			for _, s := range scopes {
				if !p.HasScope(s) {
					return nil, errs.Forbidden(CodeForbidden, "missing required scope %q", s)
				}
			}
			return next(ctx, r)
		}
	}
}

// RequireRole rejects the request unless the principal carries every
// listed role.
func RequireRole(roles ...string) httpx.Middleware {
	return func(next httpx.HandlerFunc) httpx.HandlerFunc {
		return func(ctx context.Context, r *http.Request) (any, error) {
			p, err := mustPrincipal(ctx)
			if err != nil {
				return nil, err
			}
			for _, role := range roles {
				if !p.HasRole(role) {
					return nil, errs.Forbidden(CodeForbidden, "missing required role %q", role)
				}
			}
			return next(ctx, r)
		}
	}
}

// RequireAnyRole is RequireRole's permissive sibling: any one of the
// listed roles is enough.
func RequireAnyRole(roles ...string) httpx.Middleware {
	return func(next httpx.HandlerFunc) httpx.HandlerFunc {
		return func(ctx context.Context, r *http.Request) (any, error) {
			p, err := mustPrincipal(ctx)
			if err != nil {
				return nil, err
			}
			if !p.HasAnyRole(roles...) {
				return nil, errs.Forbidden(CodeForbidden, "requires one of roles %v", roles)
			}
			return next(ctx, r)
		}
	}
}

// mustPrincipal fails closed: a guard middleware running without an
// authenticating middleware ahead of it denies the request instead of
// letting it through.
func mustPrincipal(ctx context.Context) (*Principal, error) {
	p, ok := FromContext(ctx)
	if !ok {
		return nil, errs.Unauthorized(CodeUnauthorized, "no authenticated principal")
	}
	return p, nil
}

// principalFrom reads the claims we care about. Scope and role claims come
// in a few dialects, so both the space-separated string (OAuth2 `scope`)
// and the array form (`scp`, `scopes`, `roles`) are accepted.
func principalFrom(claims jwt.MapClaims) *Principal {
	p := &Principal{Claims: claims}
	p.Subject, _ = claims["sub"].(string)
	p.Issuer, _ = claims["iss"].(string)
	p.Scopes = claimStrings(claims, "scope", "scp", "scopes")
	p.Roles = claimStrings(claims, "roles", "role", "groups")
	return p
}

// claimStrings returns the first key that yields a non-empty list, in the
// order the keys were given.
func claimStrings(claims jwt.MapClaims, keys ...string) []string {
	for _, k := range keys {
		switch v := claims[k].(type) {
		case string:
			if fields := strings.Fields(v); len(fields) > 0 {
				return fields
			}
		case []string:
			if len(v) > 0 {
				return v
			}
		case []any:
			out := make([]string, 0, len(v))
			for _, e := range v {
				if s, ok := e.(string); ok {
					out = append(out, s)
				}
			}
			if len(out) > 0 {
				return out
			}
		}
	}
	return nil
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
