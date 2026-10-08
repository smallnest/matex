// Package obs provides observability: structured logging (slog),
// Prometheus metrics and OpenTelemetry tracing.
//
// Logging is JSON to stdout with a "service" field. Request context
// carries a correlation id that is attached to every log line via the
// ctx-aware package functions:
//
//	obs.Info(ctx, "user created", "user_id", 123)
//
// That id is the active OpenTelemetry span's trace id when tracing is
// enabled (see InitTracing), and the X-Request-ID otherwise — so logs
// line up with traces the moment tracing is switched on, with no change
// at the call sites.
package obs

import (
	"context"
	"log/slog"
	"os"
)

// LogConfig configures the logger.
type LogConfig struct {
	Level string `json:"level" default:"info"`
}

// Init sets up the default slog logger (JSON, stdout).
// Call it once at startup, before any other package is used.
func Init(service string, level string) error {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		lv = slog.LevelInfo
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lv})
	slog.SetDefault(slog.New(h).With("service", service))
	return nil
}

// Logger returns the default logger.
func Logger() *slog.Logger { return slog.Default() }

type traceIDKey struct{}

// WithTraceID stores the trace id in ctx.
func WithTraceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, traceIDKey{}, id)
}

// TraceID returns the correlation id of ctx: the active span's trace id
// when tracing is enabled, otherwise the request-scoped id installed by
// WithTraceID ("" when neither is present).
func TraceID(ctx context.Context) string {
	if id := SpanTraceID(ctx); id != "" {
		return id
	}
	if v, ok := ctx.Value(traceIDKey{}).(string); ok {
		return v
	}
	return ""
}

// Log returns a logger enriched with the ctx trace id when present.
func Log(ctx context.Context) *slog.Logger {
	if id := TraceID(ctx); id != "" {
		return slog.Default().With("trace_id", id)
	}
	return slog.Default()
}

// ctx-aware level helpers.
func Debug(ctx context.Context, msg string, args ...any) { Log(ctx).Debug(msg, args...) }
func Info(ctx context.Context, msg string, args ...any)  { Log(ctx).Info(msg, args...) }
func Warn(ctx context.Context, msg string, args ...any)  { Log(ctx).Warn(msg, args...) }
func Error(ctx context.Context, msg string, args ...any) { Log(ctx).Error(msg, args...) }
