package httpx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/smallnest/matex/pkg/core/errs"
)

func serve(s *Server, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// TestMiddlewareOrder pins the execution order: the first middleware
// passed to Use is the outermost, so pre-processing runs in registration
// order and post-processing in reverse.
func TestMiddlewareOrder(t *testing.T) {
	s := newTestServer(t, Config{Timeout: time.Second})

	var trace []string
	tag := func(name string) Middleware {
		return func(next HandlerFunc) HandlerFunc {
			return func(ctx context.Context, r *http.Request) (any, error) {
				trace = append(trace, name+":in")
				data, err := next(ctx, r)
				trace = append(trace, name+":out")
				return data, err
			}
		}
	}
	s.Use(tag("a"), tag("b"))
	s.Handle("GET", "/x", func(context.Context, *http.Request) (any, error) {
		trace = append(trace, "handler")
		return "ok", nil
	})

	if rec := serve(s, httptest.NewRequest(http.MethodGet, "/x", nil)); rec.Code != http.StatusOK {
		t.Fatalf("code: %d body: %s", rec.Code, rec.Body.String())
	}
	want := "a:in,b:in,handler,b:out,a:out"
	if got := strings.Join(trace, ","); got != want {
		t.Fatalf("order = %s, want %s", got, want)
	}
}

// TestMiddlewareShortCircuit: returning before next() skips the handler
// and the error is rendered with the framework's usual envelope.
func TestMiddlewareShortCircuit(t *testing.T) {
	s := newTestServer(t, Config{Timeout: time.Second})

	reached := false
	s.Use(func(next HandlerFunc) HandlerFunc {
		return func(context.Context, *http.Request) (any, error) {
			return nil, errs.Unauthorized(40101, "token expired")
		}
	})
	s.Handle("GET", "/guarded", func(context.Context, *http.Request) (any, error) {
		reached = true
		return "secret", nil
	})

	rec := serve(s, httptest.NewRequest(http.MethodGet, "/guarded", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code: %d body: %s", rec.Code, rec.Body.String())
	}
	if reached {
		t.Fatal("handler ran despite the middleware short-circuit")
	}
	if !strings.Contains(rec.Body.String(), `"code":40101`) {
		t.Fatalf("body: %s", rec.Body.String())
	}
}

type ctxKey struct{}

// TestMiddlewareContextPropagation: what a middleware puts on the context
// (a principal, a tenant, a trace) is visible to the handler and to any
// middleware further in.
func TestMiddlewareContextPropagation(t *testing.T) {
	s := newTestServer(t, Config{Timeout: time.Second})

	s.Use(func(next HandlerFunc) HandlerFunc {
		return func(ctx context.Context, r *http.Request) (any, error) {
			return next(context.WithValue(ctx, ctxKey{}, "alice"), r)
		}
	})
	s.Handle("GET", "/whoami", func(ctx context.Context, _ *http.Request) (any, error) {
		return map[string]any{"user": ctx.Value(ctxKey{})}, nil
	})

	rec := serve(s, httptest.NewRequest(http.MethodGet, "/whoami", nil))
	var env struct {
		Data struct {
			User string `json:"user"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Data.User != "alice" {
		t.Fatalf("ctx value not propagated: %q", env.Data.User)
	}
}

// TestMiddlewareSeesTraceIDAndTimeout: middleware runs inside the wrapper,
// so the request-scoped context it receives already carries the trace id.
func TestMiddlewareSeesTraceIDAndTimeout(t *testing.T) {
	s := newTestServer(t, Config{Timeout: time.Second})

	deadlineSeen := false
	s.Use(func(next HandlerFunc) HandlerFunc {
		return func(ctx context.Context, r *http.Request) (any, error) {
			_, deadlineSeen = ctx.Deadline()
			return next(ctx, r)
		}
	})
	s.Handle("GET", "/x", func(context.Context, *http.Request) (any, error) { return "ok", nil })

	rec := serve(s, httptest.NewRequest(http.MethodGet, "/x", nil))
	if !deadlineSeen {
		t.Fatal("middleware did not see the per-request timeout")
	}
	if rec.Header().Get("X-Request-ID") == "" {
		t.Fatal("missing X-Request-ID")
	}
}

// TestUseGuardsOnlyLaterRoutes pins the registration-order semantics that
// make a public/protected split possible: routes registered before Use
// stay open, routes after it are guarded.
func TestUseGuardsOnlyLaterRoutes(t *testing.T) {
	s := newTestServer(t, Config{Timeout: time.Second})

	s.Handle("GET", "/public", func(context.Context, *http.Request) (any, error) { return "open", nil })
	s.Use(func(next HandlerFunc) HandlerFunc {
		return func(context.Context, *http.Request) (any, error) {
			return nil, errs.Unauthorized(40101, "login required")
		}
	})
	s.Handle("GET", "/private", func(context.Context, *http.Request) (any, error) { return "secret", nil })

	if rec := serve(s, httptest.NewRequest(http.MethodGet, "/public", nil)); rec.Code != http.StatusOK {
		t.Fatalf("/public: code %d, want 200 (route predates Use)", rec.Code)
	}
	if rec := serve(s, httptest.NewRequest(http.MethodGet, "/private", nil)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("/private: code %d, want 401 (route follows Use)", rec.Code)
	}
}

// TestOuterMiddlewareCoversMux: outer middleware sits outside the mux and
// therefore also covers builtin probes and HandleRaw routes.
func TestOuterMiddlewareCoversMux(t *testing.T) {
	s := newTestServer(t, Config{Timeout: time.Second})
	s.UseOuter(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Served-By", "outer")
			next.ServeHTTP(w, r)
		})
	})
	s.HandleRaw("GET /raw", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	s.Handle("GET", "/api", func(context.Context, *http.Request) (any, error) { return "ok", nil })

	for _, path := range []string{"/healthz", "/raw", "/api"} {
		rec := serve(s, httptest.NewRequest(http.MethodGet, path, nil))
		if got := rec.Header().Get("X-Served-By"); got != "outer" {
			t.Errorf("%s: outer middleware not applied (X-Served-By=%q)", path, got)
		}
	}
}

// TestOuterMiddlewareStaysInsideGuard: the guard (recover/body limit) is
// outermost, so a panicking outer middleware still yields a 500.
func TestOuterMiddlewareStaysInsideGuard(t *testing.T) {
	s := newTestServer(t, Config{Timeout: time.Second})
	s.UseOuter(func(http.Handler) http.Handler {
		return http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("outer boom") })
	})

	rec := serve(s, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code: %d", rec.Code)
	}
}

// TestOuterMiddlewareOrder: like Use, the first registered outer
// middleware is the outermost.
func TestOuterMiddlewareOrder(t *testing.T) {
	s := newTestServer(t, Config{Timeout: time.Second})
	var trace []string
	tag := func(name string) OuterMiddleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				trace = append(trace, name+":in")
				next.ServeHTTP(w, r)
				trace = append(trace, name+":out")
			})
		}
	}
	s.UseOuter(tag("a"), tag("b"))
	s.Handle("GET", "/x", func(context.Context, *http.Request) (any, error) {
		trace = append(trace, "handler")
		return "ok", nil
	})

	serve(s, httptest.NewRequest(http.MethodGet, "/x", nil))
	if got := strings.Join(trace, ","); got != "a:in,b:in,handler,b:out,a:out" {
		t.Fatalf("order = %s", got)
	}
}

// TestMiddlewareBodyLimitStillApplies guards the refactoring: the guard
// layer keeps enforcing max_body.
func TestMiddlewareBodyLimitStillApplies(t *testing.T) {
	s := newTestServer(t, Config{Timeout: time.Second, MaxBody: 8})
	s.Handle("POST", "/x", func(_ context.Context, r *http.Request) (any, error) {
		var v map[string]any
		if err := ReadJSON(r, &v); err != nil {
			return nil, errs.Invalid(40002, "bad body: %v", err)
		}
		return v, nil
	})

	rec := serve(s, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"name":"a-very-long-value"}`)))
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code: %d body: %s", rec.Code, rec.Body.String())
	}
}
