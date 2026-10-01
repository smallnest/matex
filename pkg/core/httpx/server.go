// Package httpx is the matex web layer: a thin, framework-free HTTP
// server on top of net/http (Go 1.22+ ServeMux with method patterns and
// path values).
//
// Business handlers use the envelope convention — they neither touch the
// ResponseWriter nor know about status codes:
//
//	type HandlerFunc func(ctx context.Context, r *http.Request) (any, error)
//
// The wrapper owns the whole response: panic recovery, per-request
// timeout, trace id, access logging, metrics, JSON rendering and error
// mapping (pkg/core/errs). Responses are uniformly:
//
//	success: 200 {"code":0,"msg":"ok","data":<data>}   (204 when data is nil)
//	error:   <status> {"code":<code>,"msg":"<msg>","data":null}
//
// For full control (streams, files, custom encodings) register raw
// handlers via HandleRaw.
package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/smallnest/matex/pkg/core/errs"
	"github.com/smallnest/matex/pkg/core/obs"
)

// Config configures the HTTP server.
type Config struct {
	Addr    string        `json:"addr" default:":8080"`
	Timeout time.Duration `json:"timeout" default:"10s"`
	MaxBody int64         `json:"max_body" default:"33554432"` // 32MB
}

// HandlerFunc is the matex handler signature.
type HandlerFunc func(ctx context.Context, r *http.Request) (any, error)

// Option customizes a Server.
type Option func(*Server)

// WithLogger overrides the logger (default: slog.Default()).
func WithLogger(l *slog.Logger) Option { return func(s *Server) { s.log = l } }

// WithMetrics enables /metrics and request metrics.
func WithMetrics(m *obs.Metrics) Option { return func(s *Server) { s.metrics = m } }

// WithReadyCheck overrides the readiness probe (default: always ready).
func WithReadyCheck(fn func(ctx context.Context) error) Option {
	return func(s *Server) { s.ready = fn }
}

// Server is the matex web server.
type Server struct {
	cfg     Config
	mux     *http.ServeMux
	log     *slog.Logger
	metrics *obs.Metrics
	ready   func(ctx context.Context) error
	http    *http.Server
}

// New creates a Server with /healthz, /readyz and (if metrics are
// provided) /metrics pre-registered.
func New(cfg Config, opts ...Option) *Server {
	s := &Server{cfg: cfg, mux: http.NewServeMux(), log: slog.Default(), ready: func(context.Context) error { return nil }}
	for _, o := range opts {
		o(s)
	}
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	s.mux.HandleFunc("GET /readyz", s.handleReady)
	if s.metrics != nil {
		s.mux.Handle("GET /metrics", s.metrics.Handler())
	}
	s.http = &http.Server{Addr: cfg.Addr, Handler: s.outer(s.mux)}
	return s
}

// Handle registers a matex handler for "METHOD /path" (ServeMux syntax,
// e.g. "POST /api/v1/users/{id}"). Panics on duplicate patterns — route
// conflicts must surface at startup.
func (s *Server) Handle(method, pattern string, h HandlerFunc) {
	s.mux.HandleFunc(method+" "+pattern, s.wrap(h))
}

// HandleRaw registers a raw http.HandlerFunc (escape hatch for
// streams, files, custom encodings). Named HandleRaw — not HandleFunc —
// to avoid confusion with net/http.HandlerFunc and this package's own
// HandlerFunc type.
func (s *Server) HandleRaw(pattern string, h http.HandlerFunc) {
	s.mux.HandleFunc(pattern, h)
}

// Addr returns the listen address.
func (s *Server) Addr() string { return s.cfg.Addr }

// ListenAndServe starts serving (blocks).
func (s *Server) ListenAndServe() error { return s.http.ListenAndServe() }

// Shutdown gracefully drains connections.
func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.ready(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// outer wraps the mux with recover and request body limiting.
func (s *Server) outer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				s.log.Error("panic", "err", p, "stack", string(debug.Stack()), "path", r.URL.Path)
				writeJSON(w, http.StatusInternalServerError, envelope{Code: 50000, Msg: "internal error"})
			}
		}()
		if s.cfg.MaxBody > 0 {
			r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBody)
		}
		next.ServeHTTP(w, r)
	})
}

// wrap turns a HandlerFunc into an http.HandlerFunc: trace id, timeout,
// response rendering, access log and metrics.
func (s *Server) wrap(h HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}

		traceID := r.Header.Get("X-Request-ID")
		if traceID == "" {
			traceID = newTraceID()
		}
		w.Header().Set("X-Request-ID", traceID)
		ctx := obs.WithTraceID(r.Context(), traceID)
		ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
		defer cancel()

		type result struct {
			data any
			err  error
		}
		ch := make(chan result, 1)
		go func() {
			defer func() {
				if p := recover(); p != nil {
					s.log.Error("handler panic", "err", p, "stack", string(debug.Stack()), "path", r.URL.Path)
					ch <- result{nil, errs.Internal(50000, "internal error")}
				}
			}()
			data, err := h(ctx, r)
			ch <- result{data, err}
		}()

		select {
		case res := <-ch:
			if res.err != nil {
				s.writeError(ctx, sw, res.err)
			} else {
				writeData(sw, res.data)
			}
		case <-ctx.Done():
			s.writeError(ctx, sw, errs.Timeout(50400, "request timeout"))
		}

		dur := time.Since(start)
		if !skipAccessLog(r.URL.Path) {
			obs.Log(ctx).Info("http", "method", r.Method, "path", r.URL.Path,
				"code", sw.code, "dur_ms", dur.Milliseconds(), "remote", r.RemoteAddr)
		}
		if s.metrics != nil {
			s.metrics.HTTPDuration.WithLabelValues(r.Pattern, r.Method, strconv.Itoa(sw.code)).Observe(dur.Seconds())
			s.metrics.HTTPRequests.WithLabelValues(r.Pattern, r.Method, strconv.Itoa(sw.code)).Inc()
		}
	}
}

func (s *Server) writeError(ctx context.Context, w http.ResponseWriter, err error) {
	status, code, msg := errs.Status(err)
	if status >= 500 {
		obs.Log(ctx).Error("request failed", "err", err, "status", status)
	}
	writeJSON(w, status, envelope{Code: code, Msg: msg})
}

type envelope struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data any    `json:"data"`
}

func writeData(w http.ResponseWriter, data any) {
	if data == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, envelope{Code: 0, Msg: "ok", Data: data})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// statusWriter records the status code (and stays write-through).
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func skipAccessLog(path string) bool {
	return path == "/metrics" || path == "/healthz"
}

func newTraceID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])[:8]
}

// ReadJSON decodes the request body into v (body size already limited
// by the server). Errors are ready to be wrapped into errs.Invalid.
func ReadJSON(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return fmt.Errorf("read json body: %w", err)
	}
	return nil
}
