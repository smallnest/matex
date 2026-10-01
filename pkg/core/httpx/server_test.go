package httpx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/smallnest/matex/pkg/core/errs"
)

func newTestServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	s := New(cfg)
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	return s
}

func TestHandleSuccess(t *testing.T) {
	s := newTestServer(t, Config{Timeout: time.Second})
	s.Handle("GET", "/api/v1/hello/{name}", func(ctx context.Context, r *http.Request) (any, error) {
		return map[string]string{"greeting": "hi " + r.PathValue("name")}, nil
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/hello/world", nil)
	s.http.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code: %d body: %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Greeting string `json:"greeting"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Code != 0 || env.Data.Greeting != "hi world" {
		t.Fatalf("envelope: %+v", env)
	}
	if rec.Header().Get("X-Request-ID") == "" {
		t.Fatal("missing trace id header")
	}
}

func TestHandleNilDataIs204(t *testing.T) {
	s := newTestServer(t, Config{Timeout: time.Second})
	s.Handle("POST", "/api/v1/nothing", func(context.Context, *http.Request) (any, error) {
		return nil, nil
	})
	rec := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/nothing", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code: %d", rec.Code)
	}
}

func TestHandleErrorMapping(t *testing.T) {
	s := newTestServer(t, Config{Timeout: time.Second})
	s.Handle("GET", "/api/v1/missing", func(context.Context, *http.Request) (any, error) {
		return nil, errs.NotFound(40401, "user %d not found", 42)
	})
	rec := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/missing", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code: %d", rec.Code)
	}
	var env envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Code != 40401 || env.Data != nil {
		t.Fatalf("envelope: %+v", env)
	}
}

func TestHandleTimeout(t *testing.T) {
	s := newTestServer(t, Config{Timeout: 50 * time.Millisecond})
	s.Handle("GET", "/api/v1/slow", func(ctx context.Context, _ *http.Request) (any, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
			return "done", nil
		}
	})
	rec := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/slow", nil))
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("code: %d body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandlePanicIs500(t *testing.T) {
	s := newTestServer(t, Config{Timeout: time.Second})
	s.Handle("GET", "/api/v1/panic", func(context.Context, *http.Request) (any, error) {
		panic("boom")
	})
	rec := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/panic", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code: %d", rec.Code)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	s := newTestServer(t, Config{Timeout: time.Second})
	s.Handle("GET", "/api/v1/only-get", func(context.Context, *http.Request) (any, error) { return "ok", nil })
	rec := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/only-get", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code: %d", rec.Code)
	}
}

func TestHealthAndReady(t *testing.T) {
	s := newTestServer(t, Config{Timeout: time.Second})
	for path, want := range map[string]int{
		"/healthz": http.StatusOK,
		"/readyz":  http.StatusOK,
		"/nope":    http.StatusNotFound,
	} {
		rec := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Fatalf("%s: code %d want %d", path, rec.Code, want)
		}
	}
}

func TestReadJSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/x", stringAsBody(`{"a":1}`))
	var v struct {
		A int `json:"a"`
	}
	if err := ReadJSON(req, &v); err != nil || v.A != 1 {
		t.Fatalf("err=%v v=%+v", err, v)
	}
	req2 := httptest.NewRequest(http.MethodPost, "/x", stringAsBody(`not-json`))
	if err := ReadJSON(req2, &v); err == nil {
		t.Fatal("expected error")
	}
}
