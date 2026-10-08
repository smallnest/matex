package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/smallnest/matex/pkg/core/httpx"
)

// newTestHandler builds the real routes on a bare httpx.Server and returns
// its http.Handler, so tests drive the full wrapper (trace id, timeout,
// error mapping) without binding a port.
func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	srv := httpx.New(httpx.Config{Timeout: time.Second, MaxBody: 1 << 20})
	svc := &httpService{cfg: httpConfig{Greeting: "hi ", APIKey: "demo-key"}}
	if err := svc.BuildRouter(srv); err != nil {
		t.Fatalf("build router: %v", err)
	}
	return srv.Handler()
}

func TestRoutes(t *testing.T) {
	h := newTestHandler(t)

	cases := []struct {
		name       string
		method     string
		path       string
		body       string
		headers    map[string]string
		wantStatus int
		wantBody   string
		// noTrace: these bypass the wrapper (HandleRaw, or no route → the
		// ServeMux answers directly), so there is no X-Request-ID header.
		noTrace bool
	}{
		{"echo uses path value", http.MethodGet, "/api/v1/echo/world", "", nil, http.StatusOK, `"greeting":"hi world!"`, false},
		{"nil payload is 204", http.MethodGet, "/api/v1/nothing", "", nil, http.StatusNoContent, "", false},
		{"create user needs the api key", http.MethodPost, "/api/v1/users", `{"name":"alice","email":"a@b.c"}`,
			map[string]string{"X-API-Key": "demo-key"}, http.StatusOK, `"name":"alice"`, false},
		{"bad body", http.MethodPost, "/api/v1/users", `{`,
			map[string]string{"X-API-Key": "demo-key"}, http.StatusBadRequest, `"code":40002`, false},
		{"missing name", http.MethodPost, "/api/v1/users", `{"email":"a@b.c"}`,
			map[string]string{"X-API-Key": "demo-key"}, http.StatusBadRequest, `"code":40001`, false},
		{"guarded route without the key", http.MethodPost, "/api/v1/users", `{"name":"alice"}`,
			nil, http.StatusUnauthorized, `"code":40101`, false},
		{"guarded route with a wrong key", http.MethodPost, "/api/v1/users", `{"name":"alice"}`,
			map[string]string{"X-API-Key": "nope"}, http.StatusUnauthorized, `"code":40101`, false},
		{"middleware injects the tag into ctx", http.MethodGet, "/api/v1/admin/whoami", "",
			map[string]string{"X-API-Key": "demo-key", "X-Tag": "alice"}, http.StatusOK, `"tag":"alice"`, false},
		{"whoami also needs the key", http.MethodGet, "/api/v1/admin/whoami", "", nil, http.StatusUnauthorized, `"code":40101`, false},
		{"not found", http.MethodGet, "/api/v1/errors/notfound", "", nil, http.StatusNotFound, `"code":40401`, false},
		{"conflict", http.MethodGet, "/api/v1/errors/conflict", "", nil, http.StatusConflict, `"code":40901`, false},
		{"unavailable", http.MethodGet, "/api/v1/errors/unavailable", "", nil, http.StatusServiceUnavailable, `"code":50301`, false},
		{"timeout", http.MethodGet, "/api/v1/errors/timeout", "", nil, http.StatusGatewayTimeout, `"code":50401`, false},
		{"unknown kind", http.MethodGet, "/api/v1/errors/nope", "", nil, http.StatusBadRequest, `"code":40000`, false},
		{"raw handler", http.MethodGet, "/raw/plain", "", nil, http.StatusAccepted, "raw handler", true},
		{"unknown route", http.MethodGet, "/nope", "", nil, http.StatusNotFound, "", true},
		{"wrong method", http.MethodPost, "/api/v1/echo/x", "", nil, http.StatusMethodNotAllowed, "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req *http.Request
			if tc.body == "" {
				req = httptest.NewRequest(tc.method, tc.path, nil)
			} else {
				req = httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			}
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantBody != "" && !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Fatalf("body = %s, want it to contain %s", rec.Body.String(), tc.wantBody)
			}
			if got := rec.Header().Get("X-Request-ID"); !tc.noTrace && got == "" {
				t.Error("missing X-Request-ID")
			}
			if got := rec.Header().Get("X-Served-By"); got != "matex/http-demo" {
				t.Errorf("X-Served-By = %q; the outer middleware should cover every route", got)
			}
		})
	}
}

func TestTraceIDIsEchoed(t *testing.T) {
	h := newTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/echo/x", nil)
	req.Header.Set("X-Request-ID", "trace-abc")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Request-ID"); got != "trace-abc" {
		t.Fatalf("X-Request-ID = %q, want trace-abc", got)
	}
}

func TestBuiltinProbes(t *testing.T) {
	srv := httpx.New(httpx.Config{Timeout: time.Second})
	for _, path := range []string{"/healthz", "/readyz"} {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: %d", path, rec.Code)
		}
	}
}

// TestOuterMiddlewareCoversProbes: the outer middleware lives outside the
// mux, so the builtin probes carry its header too.
func TestOuterMiddlewareCoversProbes(t *testing.T) {
	srv := httpx.New(httpx.Config{Timeout: time.Second})
	srv.UseOuter(serverHeader("matex/http-demo"))

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if got := rec.Header().Get("X-Served-By"); got != "matex/http-demo" {
		t.Fatalf("X-Served-By = %q", got)
	}
}
