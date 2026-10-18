package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/smallnest/matex/pkg/core/feature"
	"github.com/smallnest/matex/pkg/core/httpx"
)

// newTestHandler builds the real routes and applies the config section the
// way verticle.Run does.
func newTestHandler(t *testing.T, conf map[string]bool) http.Handler {
	t.Helper()
	feature.Configure(conf)
	srv := httpx.New(httpx.Config{Timeout: time.Second})
	if err := (&featureService{}).BuildRouter(srv); err != nil {
		t.Fatalf("build router: %v", err)
	}
	return srv.Handler()
}

func get(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestConfigSelectsThePath(t *testing.T) {
	for _, tc := range []struct {
		on   bool
		want string
	}{
		{true, "checkout-v2"},
		{false, "checkout-v1"},
	} {
		h := newTestHandler(t, map[string]bool{"new-checkout": tc.on})
		if rec := get(t, h, http.MethodGet, "/api/v1/checkout"); !strings.Contains(rec.Body.String(), tc.want) {
			t.Fatalf("new-checkout=%v: body = %s, want %s", tc.on, rec.Body.String(), tc.want)
		}
	}
}

func TestRuntimeFlipTakesEffectOnTheNextRequest(t *testing.T) {
	h := newTestHandler(t, map[string]bool{"beta-search": false})
	if rec := get(t, h, http.MethodGet, "/api/v1/flags"); !strings.Contains(rec.Body.String(), `"beta-search":false`) {
		t.Fatalf("before: %s", rec.Body.String())
	}
	if rec := get(t, h, http.MethodPost, "/api/v1/flags/beta-search?on=true"); rec.Code != http.StatusOK {
		t.Fatalf("flip: %d %s", rec.Code, rec.Body.String())
	}
	if rec := get(t, h, http.MethodGet, "/api/v1/flags"); !strings.Contains(rec.Body.String(), `"beta-search":true`) {
		t.Fatalf("after: %s", rec.Body.String())
	}
}

func TestBadFlipValueIsRejected(t *testing.T) {
	h := newTestHandler(t, nil)
	if rec := get(t, h, http.MethodPost, "/api/v1/flags/x?on=maybe"); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
