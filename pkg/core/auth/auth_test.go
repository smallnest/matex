package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/smallnest/matex/pkg/core/errs"
	"github.com/smallnest/matex/pkg/core/httpx"
)

const testSecret = "s3cr3t-that-is-long-enough-for-hs256"

// rsaKey is generated once: 2048-bit key generation is slow enough to
// notice across a dozen tests.
var rsaKey = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
})

func newHMAC(t *testing.T, cfg Config) *Verifier {
	t.Helper()
	if cfg.Secret == "" {
		cfg.Secret = testSecret
	}
	v, err := New(cfg)
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	return v
}

func signRS256(t *testing.T, priv *rsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	if kid != "" {
		tok.Header["kid"] = kid
	}
	s, err := tok.SignedString(priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

func signHS256(t *testing.T, secret string, claims jwt.MapClaims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

func hmacClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"sub":   "alice",
		"exp":   time.Now().Add(time.Hour).Unix(),
		"scope": "orders:read orders:write",
		"roles": []any{"admin", "ops"},
	}
}

// jwksDoc renders a JWKS document holding one RSA key.
func jwksDoc(t *testing.T, pub *rsa.PublicKey, kid string) string {
	t.Helper()
	doc := map[string]any{"keys": []map[string]string{{
		"kty": "RSA",
		"kid": kid,
		"use": "sig",
		"alg": "RS256",
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// jwksServer serves a mutable JWKS document, so tests can simulate key
// rotation and provider outages.
type jwksServer struct {
	mu   sync.Mutex
	body string
	code int
	hits int
}

func (s *jwksServer) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	body, code := s.body, s.code
	s.hits++
	s.mu.Unlock()
	if code != 0 {
		w.WriteHeader(code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

func (s *jwksServer) set(body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.body, s.code = body, 0
}

func (s *jwksServer) fail(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.code = code
}

func (s *jwksServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits
}

func TestNewRejectsAmbiguousConfig(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected error when neither secret nor jwks_url is set")
	}
	if _, err := New(Config{Secret: "s", JWKSURL: "http://x"}); err == nil {
		t.Fatal("expected error when both secret and jwks_url are set")
	}
}

func TestVerifyHMAC(t *testing.T) {
	v := newHMAC(t, Config{})

	p, err := v.Verify(context.Background(), signHS256(t, testSecret, hmacClaims()))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if p.Subject != "alice" {
		t.Fatalf("subject = %q", p.Subject)
	}
	if !p.HasScope("orders:read") || !p.HasScope("orders:write") {
		t.Fatalf("scopes = %v (space-separated `scope` not parsed)", p.Scopes)
	}
	if !p.HasRole("admin") || p.HasRole("nope") {
		t.Fatalf("roles = %v", p.Roles)
	}
}

func TestVerifyRejections(t *testing.T) {
	v := newHMAC(t, Config{})

	t.Run("wrong secret", func(t *testing.T) {
		if _, err := v.Verify(context.Background(), signHS256(t, "not-the-secret", hmacClaims())); err == nil {
			t.Fatal("expected signature failure")
		}
	})
	t.Run("expired", func(t *testing.T) {
		claims := hmacClaims()
		claims["exp"] = time.Now().Add(-time.Hour).Unix()
		_, err := v.Verify(context.Background(), signHS256(t, testSecret, claims))
		if err == nil {
			t.Fatal("expected expiry failure")
		}
		if errs.KindOf(err) != errs.KindUnauthorized {
			t.Fatalf("kind = %v, want unauthorized", errs.KindOf(err))
		}
	})
	t.Run("missing exp is rejected", func(t *testing.T) {
		claims := hmacClaims()
		delete(claims, "exp")
		if _, err := v.Verify(context.Background(), signHS256(t, testSecret, claims)); err == nil {
			t.Fatal("expected failure: exp is required")
		}
	})
	t.Run("garbage", func(t *testing.T) {
		if _, err := v.Verify(context.Background(), "not-a-jwt"); err == nil {
			t.Fatal("expected parse failure")
		}
	})
}

func TestVerifyIssuerAudience(t *testing.T) {
	v := newHMAC(t, Config{Issuer: "https://idp", Audience: "orders-api"})

	claims := hmacClaims()
	claims["iss"] = "https://idp"
	claims["aud"] = "orders-api"
	if _, err := v.Verify(context.Background(), signHS256(t, testSecret, claims)); err != nil {
		t.Fatalf("verify: %v", err)
	}

	claims["iss"] = "https://evil"
	if _, err := v.Verify(context.Background(), signHS256(t, testSecret, claims)); err == nil {
		t.Fatal("expected issuer mismatch to fail")
	}
}

func TestMiddleware(t *testing.T) {
	v := newHMAC(t, Config{})

	// guard builds a server whose route records what the handler saw.
	guard := func(mw ...httpx.Middleware) (*httpx.Server, *string) {
		seen := new(string)
		srv := httpx.New(httpx.Config{Timeout: time.Second})
		srv.Use(mw...)
		srv.Handle("GET", "/x", func(ctx context.Context, _ *http.Request) (any, error) {
			if p, ok := FromContext(ctx); ok {
				*seen = p.Subject
			}
			return "ok", nil
		})
		return srv, seen
	}

	do := func(srv *httpx.Server, authz string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	t.Run("valid token reaches the handler", func(t *testing.T) {
		srv, seen := guard(v.Middleware())
		rec := do(srv, "Bearer "+signHS256(t, testSecret, hmacClaims()))
		if rec.Code != http.StatusOK {
			t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
		}
		if *seen != "alice" {
			t.Fatalf("handler saw subject %q", *seen)
		}
	})

	t.Run("no header is 401", func(t *testing.T) {
		srv, seen := guard(v.Middleware())
		if rec := do(srv, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("code %d", rec.Code)
		}
		if *seen != "" {
			t.Fatal("handler ran without a token")
		}
	})

	t.Run("wrong scheme is 401", func(t *testing.T) {
		srv, _ := guard(v.Middleware())
		if rec := do(srv, "Basic abc"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("code %d", rec.Code)
		}
	})

	t.Run("optional lets anonymous through", func(t *testing.T) {
		srv, seen := guard(v.Optional())
		if rec := do(srv, ""); rec.Code != http.StatusOK {
			t.Fatalf("code %d", rec.Code)
		}
		if *seen != "" {
			t.Fatalf("anonymous request should carry no principal, got %q", *seen)
		}
	})

	t.Run("optional still rejects a bad token", func(t *testing.T) {
		srv, _ := guard(v.Optional())
		if rec := do(srv, "Bearer garbage"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("code %d", rec.Code)
		}
	})
}

func TestRequireScopeAndRole(t *testing.T) {
	v := newHMAC(t, Config{})
	token := signHS256(t, testSecret, hmacClaims())

	cases := []struct {
		name       string
		mw         httpx.Middleware
		wantStatus int
		wantCode   string
	}{
		{"scope present", RequireScope("orders:read"), http.StatusOK, `"code":0`},
		{"scope missing", RequireScope("orders:delete"), http.StatusForbidden, `"code":40301`},
		{"role present", RequireRole("admin"), http.StatusOK, `"code":0`},
		{"role missing", RequireRole("billing"), http.StatusForbidden, `"code":40301`},
		{"any role matches", RequireAnyRole("nope", "ops"), http.StatusOK, `"code":0`},
		{"no role matches", RequireAnyRole("nope", "billing"), http.StatusForbidden, `"code":40301`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httpx.New(httpx.Config{Timeout: time.Second})
			srv.Use(v.Middleware(), tc.mw)
			srv.Handle("GET", "/x", func(context.Context, *http.Request) (any, error) { return "ok", nil })

			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantCode) {
				t.Fatalf("body %s, want %s", rec.Body.String(), tc.wantCode)
			}
		})
	}
}

// TestGuardFailsClosed: a guard without an authenticating middleware ahead
// of it must deny, never allow.
func TestGuardFailsClosed(t *testing.T) {
	srv := httpx.New(httpx.Config{Timeout: time.Second})
	srv.Use(RequireScope("orders:read"))
	srv.Handle("GET", "/x", func(context.Context, *http.Request) (any, error) { return "ok", nil })

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code %d, want 401", rec.Code)
	}
}

func TestVerifyJWKS(t *testing.T) {
	pub := &rsaKey().PublicKey
	upstream := &jwksServer{}
	upstream.set(jwksDoc(t, pub, "key-1"))
	srv := httptest.NewServer(upstream)
	defer srv.Close()

	v, err := New(Config{JWKSURL: srv.URL, JWKSTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}

	claims := hmacClaims()
	claims["exp"] = time.Now().Add(time.Hour).Unix()
	raw := signRS256(t, rsaKey(), "key-1", claims)

	p, err := v.Verify(context.Background(), raw)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if p.Subject != "alice" {
		t.Fatalf("subject = %q", p.Subject)
	}

	// Second call must be served from cache, not refetched.
	before := upstream.count()
	if _, err := v.Verify(context.Background(), raw); err != nil {
		t.Fatalf("second verify: %v", err)
	}
	if got := upstream.count(); got != before {
		t.Fatalf("jwks refetched on a warm cache: %d → %d", before, got)
	}
}

// TestJWKSRotation: an unknown kid triggers an immediate refetch, so a
// rotated key starts working without waiting out the TTL.
func TestJWKSRotation(t *testing.T) {
	oldKey, newKey := rsaKey(), rsaKey()
	upstream := &jwksServer{}
	upstream.set(jwksDoc(t, &oldKey.PublicKey, "key-1"))
	srv := httptest.NewServer(upstream)
	defer srv.Close()

	v, err := New(Config{JWKSURL: srv.URL, JWKSTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	claims := jwt.MapClaims{"sub": "bob", "exp": time.Now().Add(time.Hour).Unix()}

	// Warm the cache with the old key.
	if _, err := v.Verify(context.Background(), signRS256(t, oldKey, "key-1", claims)); err != nil {
		t.Fatalf("old key: %v", err)
	}

	// The provider rotates.
	upstream.set(jwksDoc(t, &newKey.PublicKey, "key-2"))
	if _, err := v.Verify(context.Background(), signRS256(t, newKey, "key-2", claims)); err != nil {
		t.Fatalf("rotated key should be picked up on refetch: %v", err)
	}
}

// TestJWKSUnavailableIs503: an unreachable identity provider is an
// infrastructure failure, not a bad credential.
func TestJWKSUnavailableIs503(t *testing.T) {
	upstream := &jwksServer{}
	upstream.set(jwksDoc(t, &rsaKey().PublicKey, "key-1"))
	srv := httptest.NewServer(upstream)
	defer srv.Close()

	v, err := New(Config{JWKSURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	claims := jwt.MapClaims{"sub": "bob", "exp": time.Now().Add(time.Hour).Unix()}
	raw := signRS256(t, rsaKey(), "key-1", claims)

	upstream.fail(http.StatusInternalServerError)
	_, err = v.Verify(context.Background(), raw)
	if err == nil {
		t.Fatal("expected error")
	}
	status, _, _ := errs.Status(err)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503 (err=%v)", status, err)
	}
}

// TestAlgorithmConfusion: an HMAC token signed with the RSA public key as
// its secret must not be accepted by a JWKS-configured verifier.
func TestAlgorithmConfusion(t *testing.T) {
	upstream := &jwksServer{}
	pub := &rsaKey().PublicKey
	upstream.set(jwksDoc(t, pub, "key-1"))
	srv := httptest.NewServer(upstream)
	defer srv.Close()

	v, err := New(Config{JWKSURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	// The classic attack: sign HS256 using the public key's DER/PEM as the
	// shared secret.
	forged := signHS256(t, jwksDoc(t, pub, "key-1"), jwt.MapClaims{
		"sub": "attacker",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	if _, err := v.Verify(context.Background(), forged); err == nil {
		t.Fatal("HS256 token accepted by a JWKS verifier (algorithm confusion)")
	}
}

func TestIssueRoundTrip(t *testing.T) {
	v := newHMAC(t, Config{Issuer: "matex"})

	raw, err := v.Issue(IssueRequest{
		Subject: "svc-a",
		Scopes:  []string{"internal:call"},
		Roles:   []string{"ops"},
	}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	p, err := v.Verify(context.Background(), raw)
	if err != nil {
		t.Fatalf("verify issued token: %v", err)
	}
	if p.Subject != "svc-a" || !p.HasScope("internal:call") || !p.HasRole("ops") || p.Issuer != "matex" {
		t.Fatalf("principal: %+v", p)
	}
}

func TestIssueRejectsBadInput(t *testing.T) {
	v := newHMAC(t, Config{})
	if _, err := v.Issue(IssueRequest{}, time.Minute); err == nil {
		t.Fatal("expected error for a missing subject")
	}
	if _, err := v.Issue(IssueRequest{Subject: "a"}, 0); err == nil {
		t.Fatal("expected error for a non-positive ttl")
	}
}

// TestIssueExtraCannotForgeIdentity: Extra must not be able to override the
// claims that decide who the caller is or how long the token lives.
func TestIssueExtraCannotForgeIdentity(t *testing.T) {
	v := newHMAC(t, Config{})
	raw, err := v.Issue(IssueRequest{
		Subject: "alice",
		Extra: map[string]any{
			"sub": "root",
			"exp": time.Now().Add(100 * time.Hour).Unix(),
			"iam": "admin",
		},
	}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	p, err := v.Verify(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.Subject != "alice" {
		t.Fatalf("subject = %q; Extra forged the identity", p.Subject)
	}
	if p.Claims["iam"] != "admin" {
		t.Fatal("Extra claims should still be merged")
	}
}

func TestIssueRequiresHMAC(t *testing.T) {
	v, err := New(Config{JWKSURL: "http://127.0.0.1:1/jwks"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Issue(IssueRequest{Subject: "sub"}, time.Minute); err == nil {
		t.Fatal("expected Issue to reject non-HMAC mode")
	}
}

func TestConfigDefaultsAndDisplay(t *testing.T) {
	v := newHMAC(t, Config{})
	cfg := v.Config()
	if cfg.Header != "Authorization" || cfg.Scheme != "Bearer" {
		t.Fatalf("header/scheme defaults not applied: %+v", cfg)
	}
	if cfg.Leeway != 30*time.Second || cfg.JWKSTTL != 10*time.Minute {
		t.Fatalf("duration defaults not applied: %+v", cfg)
	}
}
