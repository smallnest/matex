package obs

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// scopeName identifies this framework as the tracer's instrumentation
// scope (the value that shows up in the "otel.scope.name" attribute).
const scopeName = "github.com/smallnest/matex"

// provider is the tracer provider obs hands out. It is kept separate from
// OpenTelemetry's process-global one on purpose: the global can only be
// set once and refuses to be set back, which would make "turn tracing off
// again" impossible — and untestable.
var (
	providerMu sync.RWMutex
	provider   trace.TracerProvider
)

// Tracer returns this framework's tracer.
func Tracer() trace.Tracer {
	providerMu.RLock()
	p := provider
	providerMu.RUnlock()
	if p != nil {
		return p.Tracer(scopeName)
	}
	return otel.Tracer(scopeName)
}

// setProvider swaps the provider obs uses; nil falls back to the global
// (no-op) one.
func setProvider(p trace.TracerProvider) {
	providerMu.Lock()
	provider = p
	providerMu.Unlock()
}

// tracingEnabled reports whether obs has a real provider installed.
func tracingEnabled() bool {
	providerMu.RLock()
	defer providerMu.RUnlock()
	return provider != nil
}

// noopSpan is what the disabled paths hand back: a valid Span whose every
// method is a no-op, so callers can End() it unconditionally.
var noopSpan = trace.SpanFromContext(context.Background())

// TraceConfig configures distributed tracing (OpenTelemetry).
//
// Tracing is opt-in: with Enabled false every helper here is a no-op and
// the package behaves exactly as before, using only the X-Request-ID
// correlation id. Turning it on adds real spans and a W3C trace context
// that propagates across services.
type TraceConfig struct {
	// Enabled turns tracing on.
	Enabled bool `json:"enabled" default:"false"`
	// Endpoint is the OTLP/gRPC collector address, e.g. "localhost:4317"
	// or "otel-collector.observability:4317". Empty keeps the spans local
	// (they still carry a trace id, which the logs then show) without
	// shipping them anywhere.
	Endpoint string `json:"endpoint" optional:""`
	// Insecure dials the collector without TLS (the common in-cluster
	// setup).
	Insecure bool `json:"insecure" optional:""`
	// SampleRatio is the head sampling ratio in [0,1]; 1 samples
	// everything. Sampled-elsewhere traces are kept whole either way.
	SampleRatio float64 `json:"sample_ratio" default:"1.0"`
	// ExportTimeout bounds one batch export to the collector.
	ExportTimeout time.Duration `json:"export_timeout" default:"10s"`
}

// InitTracing installs the tracer provider and the W3C trace context
// propagator. The returned shutdown func flushes pending spans — call it
// during graceful shutdown, before the process exits, or the last batch is
// lost.
//
// With cfg.Enabled false the returned func is a no-op and any provider
// installed by an earlier call is cleared, so tracing can be switched back
// off. Production code calls this once; the reset exists for tests.
func InitTracing(ctx context.Context, service string, cfg TraceConfig) (shutdown func(context.Context) error, err error) {
	if !cfg.Enabled {
		setProvider(nil)
		return func(context.Context) error { return nil }, nil
	}
	if cfg.ExportTimeout <= 0 {
		cfg.ExportTimeout = 10 * time.Second
	}

	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(resource.NewSchemaless(
			attribute.String("service.name", service),
		)),
		sdktrace.WithSampler(traceSampler(cfg.SampleRatio)),
	}
	if cfg.Endpoint != "" {
		exp, err := otlptracegrpc.New(ctx,
			otlptracegrpc.WithEndpoint(cfg.Endpoint),
			otlptracegrpc.WithInsecure(), // OTLP/gRPC is in-cluster; TLS is the mesh's job
			otlptracegrpc.WithTimeout(cfg.ExportTimeout),
		)
		if err != nil {
			return nil, fmt.Errorf("obs: otlp exporter: %w", err)
		}
		opts = append(opts, sdktrace.WithBatcher(exp, sdktrace.WithExportTimeout(cfg.ExportTimeout)))
	}

	tp := sdktrace.NewTracerProvider(opts...)
	setProvider(tp)
	// Publish globally too, so third-party instrumentation that reaches for
	// otel.Tracer joins the same traces.
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	return tp.Shutdown, nil
}

// traceSampler maps a ratio onto an OTel sampler. ParentBased keeps a trace
// that an upstream service already decided to sample — otherwise half a
// trace shows up in the backend, which is worse than none.
func traceSampler(ratio float64) sdktrace.Sampler {
	switch {
	case ratio >= 1:
		return sdktrace.ParentBased(sdktrace.AlwaysSample())
	case ratio <= 0:
		return sdktrace.ParentBased(sdktrace.NeverSample())
	default:
		return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))
	}
}

// StartSpan starts a span. With tracing disabled this is a no-op span, so
// instrumenting code never needs to check whether tracing is on.
func StartSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return Tracer().Start(ctx, name, trace.WithAttributes(attrs...))
}

// StartHTTPSpan continues the caller's trace (reading the W3C traceparent
// from the request headers) and opens a server span for it.
//
// The span name and the http.route attribute both come from the route
// pattern rather than the raw path: a span name has to have low
// cardinality to be usable. ServeMux stores the pattern as
// "METHOD /path" (Go 1.22+), so the method is split off for the attribute.
func StartHTTPSpan(ctx context.Context, r *http.Request) (context.Context, trace.Span) {
	if !tracingEnabled() {
		// Leave the context alone. Extracting a caller's traceparent here
		// would plant a live SpanContext in ctx and hijack the correlation
		// id, even though this process created no span at all.
		return ctx, noopSpan
	}
	ctx = otel.GetTextMapPropagator().Extract(ctx, propagation.HeaderCarrier(r.Header))

	name, route := r.Method+" unknown", r.URL.Path
	if r.Pattern != "" {
		name = r.Pattern
		if _, path, ok := strings.Cut(r.Pattern, " "); ok {
			route = path
		} else {
			route = r.Pattern
		}
	}
	ctx, span := Tracer().Start(ctx, name,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			attribute.String("http.request.method", r.Method),
			attribute.String("http.route", route),
			attribute.String("url.path", r.URL.Path),
			attribute.String("client.address", r.RemoteAddr),
		),
	)
	return ctx, span
}

// EndHTTPSpan records the outcome and closes the span.
func EndHTTPSpan(span trace.Span, status int) {
	span.SetAttributes(attribute.Int("http.response.status_code", status))
	span.End()
}

// ExtractHTTP rebuilds the trace context a caller propagated in headers.
func ExtractHTTP(ctx context.Context, h http.Header) context.Context {
	return otel.GetTextMapPropagator().Extract(ctx, propagation.HeaderCarrier(h))
}

// InjectHTTP writes the current trace context into headers, for outgoing
// calls. Call it after StartSpan so the downstream service joins the trace.
func InjectHTTP(ctx context.Context, h http.Header) {
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(h))
}

// SpanTraceID returns the trace id of the span active in ctx, or "" when
// there is none.
func SpanTraceID(ctx context.Context) string {
	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
		return sc.TraceID().String()
	}
	return ""
}
