package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/smallnest/matex/pkg/core/obs"
)

// enableTracing turns tracing on for one test and off again afterwards.
func enableTracing(t *testing.T) {
	t.Helper()
	prevPropagator := otel.GetTextMapPropagator()
	shutdown, err := obs.InitTracing(context.Background(), "httpx-test", obs.TraceConfig{Enabled: true})
	if err != nil {
		t.Fatalf("init tracing: %v", err)
	}
	t.Cleanup(func() {
		_ = shutdown(context.Background())
		if _, err := obs.InitTracing(context.Background(), "httpx-test", obs.TraceConfig{}); err != nil {
			t.Errorf("reset tracing: %v", err)
		}
		otel.SetTextMapPropagator(prevPropagator)
	})
}

// TestServerSpanAndTraceID is the end-to-end tracing contract: the caller's
// traceparent decides the trace id that the handler sees and that comes
// back as X-Request-ID.
func TestServerSpanAndTraceID(t *testing.T) {
	enableTracing(t)

	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"

	s := newTestServer(t, Config{Timeout: time.Second})
	s.Handle("GET", "/api/v1/echo/{name}", func(ctx context.Context, r *http.Request) (any, error) {
		return map[string]string{"trace_id": obs.TraceID(ctx), "name": r.PathValue("name")}, nil
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/echo/world", nil)
	req.Header.Set("traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")
	resp := serve(s, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("code %d body %s", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("X-Request-ID"); got != traceID {
		t.Fatalf("X-Request-ID = %q, want the propagated trace id %q", got, traceID)
	}
	if !strings.Contains(resp.Body.String(), traceID) {
		t.Fatalf("handler saw a different trace id: %s", resp.Body.String())
	}
}

// TestTraceIDStillWorksWithoutTracing guards the default path: with tracing
// off, the X-Request-ID contract is exactly as it was.
func TestTraceIDStillWorksWithoutTracing(t *testing.T) {
	if _, err := obs.InitTracing(context.Background(), "httpx-test", obs.TraceConfig{}); err != nil {
		t.Fatal(err)
	}

	s := newTestServer(t, Config{Timeout: time.Second})
	s.Handle("GET", "/x", func(ctx context.Context, _ *http.Request) (any, error) {
		return map[string]string{"trace_id": obs.TraceID(ctx)}, nil
	})

	t.Run("generated when absent", func(t *testing.T) {
		resp := serve(s, httptest.NewRequest(http.MethodGet, "/x", nil))
		id := resp.Header().Get("X-Request-ID")
		if id == "" {
			t.Fatal("no correlation id generated")
		}
		if strings.Contains(id, "-") || len(id) != 8 {
			t.Fatalf("generated id %q does not look like the short request id", id)
		}
		if !strings.Contains(resp.Body.String(), id) {
			t.Fatalf("handler and response disagree: %s vs %q", resp.Body.String(), id)
		}
	})

	t.Run("inbound id is echoed and visible to the handler", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("X-Request-ID", "trace-abc")
		resp := serve(s, req)
		if got := resp.Header().Get("X-Request-ID"); got != "trace-abc" {
			t.Fatalf("X-Request-ID = %q", got)
		}
		if !strings.Contains(resp.Body.String(), "trace-abc") {
			t.Fatalf("handler did not see the inbound id: %s", resp.Body.String())
		}
	})

	t.Run("a traceparent alone does not leak a trace id out of nowhere", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
		resp := serve(s, req)
		if got := resp.Header().Get("X-Request-ID"); got == "4bf92f3577b34da6a3ce929d0e0e4736" {
			t.Fatal("tracing is off, so the traceparent must not become the correlation id")
		}
	})
}
