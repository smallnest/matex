package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/smallnest/matex/pkg/core/errs"
)

// TestPprofIsOffByDefault: profiles leak memory contents and cost CPU, so
// they must never appear without being asked for.
func TestPprofIsOffByDefault(t *testing.T) {
	s := newTestServer(t, Config{Timeout: time.Second})

	for _, path := range []string{"/debug/pprof/", "/debug/pprof/cmdline", "/debug/pprof/heap"} {
		rec := serve(s, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: code %d, want 404 while the profiler is off", path, rec.Code)
		}
	}
}

func TestPprofWhenEnabled(t *testing.T) {
	s := newTestServer(t, Config{Timeout: time.Second, Pprof: true})

	// The index lists the profiles and serves them by path suffix.
	rec := serve(s, httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("index: code %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "goroutine") {
		t.Fatalf("index body does not look like the profile list: %s", rec.Body.String())
	}

	// A profile is served by the index, not by a JSON handler.
	rec = serve(s, httptest.NewRequest(http.MethodGet, "/debug/pprof/goroutine?debug=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("goroutine: code %d", rec.Code)
	}
	if strings.HasPrefix(rec.Body.String(), `{"code":0`) {
		t.Fatal("the profile went through the JSON envelope")
	}

	// The named endpoints are mounted too.
	for _, path := range []string{"/debug/pprof/cmdline", "/debug/pprof/symbol"} {
		if rec := serve(s, httptest.NewRequest(http.MethodGet, path, nil)); rec.Code != http.StatusOK {
			t.Errorf("%s: code %d", path, rec.Code)
		}
	}
}

// TestPprofBypassesEnvelopeMiddleware: profiles are raw handlers, so a
// service's envelope middleware neither sees them nor can reject them.
func TestPprofBypassesEnvelopeMiddleware(t *testing.T) {
	s := newTestServer(t, Config{Timeout: time.Second, Pprof: true})
	s.Use(func(next HandlerFunc) HandlerFunc {
		return func(context.Context, *http.Request) (any, error) {
			return nil, errs.Unauthorized(40101, "not allowed")
		}
	})
	s.Handle("GET", "/api", func(context.Context, *http.Request) (any, error) { return "ok", nil })

	if rec := serve(s, httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil)); rec.Code != http.StatusOK {
		t.Fatalf("the profiler was blocked by envelope middleware: code %d", rec.Code)
	}
	// The same middleware does guard a normal route, so the contrast is real.
	if rec := serve(s, httptest.NewRequest(http.MethodGet, "/api", nil)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the route was not guarded: code %d", rec.Code)
	}
}
