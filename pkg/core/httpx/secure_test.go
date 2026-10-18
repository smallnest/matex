package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// do sends one request through the server's real handler chain.
func do(t *testing.T, s *Server, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rec, r)
	return rec
}

// TestSecureHeadersAreDefault: the headers must be on without the caller
// doing anything — a new server that forgets them is the common mistake.
func TestSecureHeadersAreDefault(t *testing.T) {
	s := newTestServer(t, Config{Timeout: time.Second})
	s.Handle("GET", "/api/v1/ping", func(context.Context, *http.Request) (any, error) {
		return map[string]string{"ok": "true"}, nil
	})

	rec := do(t, s, httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil))
	want := map[string]string{
		"X-Content-Type-Options":       "nosniff",
		"X-Frame-Options":              "DENY",
		"Referrer-Policy":              "no-referrer",
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Cross-Origin-Resource-Policy": "same-origin",
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if csp := rec.Header().Get("Content-Security-Policy"); csp == "" {
		t.Error("no Content-Security-Policy")
	}
	// HSTS would pin a plain-http client to a scheme the server does not
	// serve, so it is sent only when the request arrived over TLS.
	if got := rec.Header().Get("Strict-Transport-Security"); got != "" {
		t.Errorf("HSTS on plain http = %q, want empty", got)
	}
	// A TLS-terminating proxy forwards the original scheme, so HSTS must
	// follow the header when it is present.
	fwd := httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil)
	fwd.Header.Set("X-Forwarded-Proto", "https") // as a TLS-terminating proxy sends it
	if rec := do(t, s, fwd); rec.Header().Get("Strict-Transport-Security") == "" {
		t.Error("no HSTS behind an https-terminating proxy")
	}
}

// TestSecureHeadersCoverProbeAndRawRoutes: /healthz and HandleRaw bypass the
// envelope, which is why the headers sit at the net/http layer.
func TestSecureHeadersCoverProbeAndRawRoutes(t *testing.T) {
	s := newTestServer(t, Config{Timeout: time.Second})
	s.HandleRaw("GET /debug/stream", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("chunk"))
	})

	for _, path := range []string{"/healthz", "/readyz", "/debug/stream"} {
		rec := do(t, s, httptest.NewRequest(http.MethodGet, path, nil))
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q", path, got)
		}
	}
}

// TestErrorResponsesCarryTheHeaders: the guard renders a panic as JSON; the
// headers must already be set on that response too.
func TestErrorResponsesCarryTheHeaders(t *testing.T) {
	s := newTestServer(t, Config{Timeout: time.Second})
	s.Handle("GET", "/boom", func(context.Context, *http.Request) (any, error) {
		panic("boom")
	})

	rec := do(t, s, httptest.NewRequest(http.MethodGet, "/boom", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500", rec.Code)
	}
	if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Fatalf("X-Frame-Options = %q on the panic response", got)
	}
}

// TestNoCORSByDefault: a same-origin API must not invite other origins.
func TestNoCORSByDefault(t *testing.T) {
	s := newTestServer(t, Config{Timeout: time.Second})
	s.Handle("GET", "/api/v1/ping", func(context.Context, *http.Request) (any, error) { return "ok", nil })

	req := httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil)
	req.Header.Set("Origin", "https://evil.example")
	if got := do(t, s, req).Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want empty", got)
	}
}

func TestCORSAllowsListedOriginOnly(t *testing.T) {
	s := newTestServer(t, Config{Timeout: time.Second})
	s.UseOuter(CORSPolicy("https://app.example"))
	s.Handle("GET", "/api/v1/ping", func(context.Context, *http.Request) (any, error) { return "ok", nil })

	ok := httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil)
	ok.Header.Set("Origin", "https://app.example")
	rec := do(t, s, ok)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example" {
		t.Fatalf("allowed origin = %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("credentials = %q", got)
	}

	denied := httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil)
	denied.Header.Set("Origin", "https://evil.example")
	rec = do(t, s, denied)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("unlisted origin got %q", got)
	}
	if got := rec.Header().Get("Vary"); got != "Origin" {
		t.Fatalf("Vary = %q; a cache could serve one origin's answer to another", got)
	}
}

// TestCORSPreflightIsAnswered: a browser never sends the real request until
// the preflight is answered, so OPTIONS must not reach the handler.
func TestCORSPreflightIsAnswered(t *testing.T) {
	var reached bool
	s := newTestServer(t, Config{Timeout: time.Second})
	s.UseOuter(CORSPolicy("https://app.example"))
	s.Handle("DELETE", "/api/v1/orders/{id}", func(context.Context, *http.Request) (any, error) {
		reached = true
		return nil, nil
	})

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/orders/7", nil)
	req.Header.Set("Origin", "https://app.example")
	req.Header.Set("Access-Control-Request-Method", "DELETE")
	req.Header.Set("Access-Control-Request-Headers", "authorization")
	rec := do(t, s, req)

	if rec.Code != http.StatusNoContent || reached {
		t.Fatalf("preflight code = %d, handler reached = %v", rec.Code, reached)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "authorization" {
		t.Fatalf("allow-headers = %q", got)
	}
	if got := rec.Header().Get("Access-Control-Max-Age"); got == "" {
		t.Fatal("no Access-Control-Max-Age: every request would be preflighted again")
	}
}

// TestCORSWildcardIsCredentialFree: "*" and credentials contradict each
// other, so a wildcard policy must set only the origin.
func TestCORSWildcardIsCredentialFree(t *testing.T) {
	s := newTestServer(t, Config{Timeout: time.Second})
	s.UseOuter(CORSPolicy("*"))
	s.Handle("GET", "/api/v1/ping", func(context.Context, *http.Request) (any, error) { return "ok", nil })

	req := httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil)
	req.Header.Set("Origin", "https://any.example")
	rec := do(t, s, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("origin = %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("credentials = %q with a wildcard", got)
	}
}
