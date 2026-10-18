package httpx

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/smallnest/matex/pkg/core/obs"
)

// The wrapper's job under pressure: report a dead client as a disconnect
// (not a timeout), keep a timed-out handler alive to finish its effect, let
// a streaming handler outlive the timeout, and never trust an oversized
// correlation id.

func TestClientDisconnectIsNotReportedAsTimeout(t *testing.T) {
	s := newTestServer(t, Config{Timeout: time.Second})
	var seenErr error
	done := make(chan struct{})
	s.Handle("GET", "/x", func(ctx context.Context, _ *http.Request) (any, error) {
		defer close(done)
		select {
		case <-ctx.Done():
			seenErr = ctx.Err()
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
			return "late", nil
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the client has already gone
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil).WithContext(ctx))

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never observed the client disconnect")
	}
	if rec.Code == http.StatusGatewayTimeout {
		t.Fatal("a client disconnect must not be answered 504")
	}
	if seenErr == nil {
		t.Fatal("the handler must learn about the disconnect through its context")
	}
	if strings.Contains(rec.Body.String(), "timeout") {
		t.Fatalf("body blames the server: %s", rec.Body.String())
	}
}

func TestTimedOutHandlerKeepsRunningToCompletion(t *testing.T) {
	s := newTestServer(t, Config{Timeout: 50 * time.Millisecond})
	finished := make(chan struct{}, 1)
	s.Handle("GET", "/x", func(ctx context.Context, _ *http.Request) (any, error) {
		// A handler that ignores cancellation (e.g. blocked on a driver
		// call) still records its effect after the 504 was sent.
		<-ctx.Done()
		time.Sleep(100 * time.Millisecond)
		finished <- struct{}{}
		return "effect recorded", nil
	})

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("code = %d, want 504", rec.Code)
	}
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("handler was killed before it could finish")
	}
}

func TestStreamedResponseOutlivesTheRequestTimeout(t *testing.T) {
	s := newTestServer(t, Config{Timeout: 50 * time.Millisecond})
	s.HandleRaw("GET /stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for range 5 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(30 * time.Millisecond):
			}
			_, _ = io.WriteString(w, "data: tick\n\n")
			w.(http.Flusher).Flush()
		}
	})

	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	start := time.Now()
	resp, err := http.Get(srv.URL + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("stream cut off: %v", err)
	}
	if got := strings.Count(string(body), "tick"); got != 5 {
		t.Fatalf("received %d ticks of 5 in %v (stream was cut)", got, time.Since(start))
	}
}

func TestOversizedRequestIDIsTruncated(t *testing.T) {
	s := newTestServer(t, Config{Timeout: time.Second})
	var seen string
	s.Handle("GET", "/x", func(ctx context.Context, _ *http.Request) (any, error) {
		seen = obs.TraceID(ctx)
		return nil, nil
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("X-Request-ID", strings.Repeat("a", 10_000))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if len(seen) > maxTraceIDLen {
		t.Fatalf("handler saw a %d-byte trace id", len(seen))
	}
	if got := rec.Header().Get("X-Request-ID"); len(got) > maxTraceIDLen {
		t.Fatalf("response echoed a %d-byte id", len(got))
	}
}

func TestServerHasSocketTimeouts(t *testing.T) {
	s := New(Config{Addr: ":0", ReadHeaderTimeout: 3 * time.Second, IdleTimeout: 30 * time.Second})
	if s.http.ReadHeaderTimeout != 3*time.Second {
		t.Fatalf("ReadHeaderTimeout = %v", s.http.ReadHeaderTimeout)
	}
	if s.http.IdleTimeout != 30*time.Second {
		t.Fatalf("IdleTimeout = %v", s.http.IdleTimeout)
	}
}
