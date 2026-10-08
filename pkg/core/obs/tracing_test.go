package obs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// inboundTrace is a well-formed W3C traceparent: version 00, trace id,
// parent span id, sampled flag.
const (
	inboundTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	inboundSpanID  = "00f067aa0ba902b7"
	inboundHeader  = "00-" + inboundTraceID + "-" + inboundSpanID + "-01"
)

// installRecorder points obs at a recording provider and restores the
// no-op one afterwards. It deliberately does not touch OpenTelemetry's
// process-global provider: that one can be set only once, so a test that
// did would leave tracing on for every test after it.
func installRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	prevPropagator := otel.GetTextMapPropagator()

	rec := tracetest.NewSpanRecorder()
	setProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	t.Cleanup(func() {
		setProvider(nil)
		otel.SetTextMapPropagator(prevPropagator)
	})
	return rec
}

func TestStartHTTPSpan(t *testing.T) {
	rec := installRecorder(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/echo/world", nil)
	// ServeMux records the matched pattern in this form (Go 1.22+).
	req.Pattern = "GET /api/v1/echo/{name}"
	req.RemoteAddr = "10.0.0.1:1234"

	_, span := StartHTTPSpan(context.Background(), req)
	EndHTTPSpan(span, http.StatusCreated)

	ended := rec.Ended()
	if len(ended) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(ended))
	}
	got := ended[0]
	if got.Name() != "GET /api/v1/echo/{name}" {
		t.Fatalf("span name = %q; it must use the route pattern, not the path", got.Name())
	}
	if got.SpanKind() != trace.SpanKindServer {
		t.Fatalf("span kind = %v", got.SpanKind())
	}
	attrs := map[string]string{}
	for _, kv := range got.Attributes() {
		attrs[string(kv.Key)] = kv.Value.Emit()
	}
	// The route attribute carries the path without the method.
	if attrs["http.request.method"] != "GET" || attrs["http.route"] != "/api/v1/echo/{name}" {
		t.Fatalf("attributes = %v", attrs)
	}
	if attrs["http.response.status_code"] != "201" {
		t.Fatalf("status attribute = %q", attrs["http.response.status_code"])
	}
}

// TestStartHTTPSpanContinuesInboundTrace is the cross-service case: the
// span must join the caller's trace rather than start a new one.
func TestStartHTTPSpanContinuesInboundTrace(t *testing.T) {
	rec := installRecorder(t)

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("traceparent", inboundHeader)

	ctx, span := StartHTTPSpan(context.Background(), req)
	if got := SpanTraceID(ctx); got != inboundTraceID {
		t.Fatalf("trace id = %q, want the caller's %q", got, inboundTraceID)
	}
	EndHTTPSpan(span, http.StatusOK)

	ended := rec.Ended()
	if len(ended) != 1 {
		t.Fatalf("recorded %d spans", len(ended))
	}
	if ended[0].Parent().SpanID().String() != inboundSpanID {
		t.Fatalf("parent span id = %q, want %q", ended[0].Parent().SpanID(), inboundSpanID)
	}
}

func TestHTTPPropagationRoundTrip(t *testing.T) {
	installRecorder(t)

	// An outgoing call stamps the trace onto the request headers...
	ctx, span := StartSpan(context.Background(), "call-downstream")
	defer span.End()

	out := http.Header{}
	InjectHTTP(ctx, out)
	if out.Get("traceparent") == "" {
		t.Fatal("InjectHTTP wrote no traceparent")
	}

	// ...and the receiving side picks the same trace back up.
	got := ExtractHTTP(context.Background(), out)
	if SpanTraceID(got) != SpanTraceID(ctx) {
		t.Fatalf("trace id changed across the hop: %q → %q", SpanTraceID(ctx), SpanTraceID(got))
	}
}

func TestStartSpanNestsUnderTheRequestSpan(t *testing.T) {
	rec := installRecorder(t)

	ctx, serverSpan := StartHTTPSpan(context.Background(), httptest.NewRequest(http.MethodGet, "/x", nil))
	_, child := StartSpan(ctx, "db.query")
	child.End()
	EndHTTPSpan(serverSpan, http.StatusOK)

	ended := rec.Ended()
	if len(ended) != 2 {
		t.Fatalf("recorded %d spans, want 2", len(ended))
	}
	var dbSpan sdktrace.ReadOnlySpan
	for _, s := range ended {
		if s.Name() == "db.query" {
			dbSpan = s
		}
	}
	if dbSpan == nil {
		t.Fatal("child span not recorded")
	}
	if dbSpan.Parent().SpanID() != serverSpan.SpanContext().SpanID() {
		t.Fatal("child span is not parented to the request span")
	}
}

// TestTraceIDFallsBackWithoutSpans pins the compatibility contract: with
// tracing off (no span in ctx) the correlation id is still the one from
// WithTraceID.
func TestTraceIDFallsBackWithoutSpans(t *testing.T) {
	ctx := WithTraceID(context.Background(), "req-123")
	if got := TraceID(ctx); got != "req-123" {
		t.Fatalf("TraceID = %q, want req-123", got)
	}
	if SpanTraceID(ctx) != "" {
		t.Fatal("SpanTraceID must be empty without a span")
	}
}

func TestTraceIDPrefersTheActiveSpan(t *testing.T) {
	installRecorder(t)

	ctx := WithTraceID(context.Background(), "inbound-x-request-id")
	ctx, span := StartSpan(ctx, "work")
	defer span.End()

	if got, want := TraceID(ctx), SpanTraceID(ctx); got != want {
		t.Fatalf("TraceID = %q, want the span's %q", got, want)
	}
	if TraceID(ctx) == "inbound-x-request-id" {
		t.Fatal("the span trace id should win over the X-Request-ID fallback")
	}
}

// TestInitTracingDisabledIsNoop also covers the reset: a provider installed
// by an earlier call must be cleared, or tracing could never be turned off
// again.
func TestInitTracingDisabledIsNoop(t *testing.T) {
	setProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(tracetest.NewSpanRecorder())))

	shutdown, err := InitTracing(context.Background(), "svc", TraceConfig{})
	if err != nil {
		t.Fatalf("InitTracing: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	ctx, span := StartSpan(context.Background(), "work")
	defer span.End()
	if SpanTraceID(ctx) != "" {
		t.Fatal("tracing is disabled but spans are still being created")
	}
}

// TestInitTracingEnabledWithoutEndpoint: tracing on, no collector — spans
// still carry ids (so logs get a real trace id) but nothing is shipped.
func TestInitTracingEnabledWithoutEndpoint(t *testing.T) {
	shutdown, err := InitTracing(context.Background(), "svc", TraceConfig{Enabled: true})
	if err != nil {
		t.Fatalf("InitTracing: %v", err)
	}
	t.Cleanup(func() {
		_ = shutdown(context.Background())
		setProvider(nil)
	})

	ctx, span := StartSpan(context.Background(), "work")
	defer span.End()
	if SpanTraceID(ctx) == "" {
		t.Fatal("tracing enabled but the span has no trace id")
	}
	if len(otel.GetTextMapPropagator().Fields()) == 0 {
		t.Fatal("propagator not installed")
	}
}

// TestInitTracingShutdownFlushes: the returned shutdown must not error and
// must be safe to call when nothing was recorded.
func TestInitTracingShutdownFlushes(t *testing.T) {
	shutdown, err := InitTracing(context.Background(), "svc", TraceConfig{Enabled: true, SampleRatio: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { setProvider(nil) })

	_, span := StartSpan(context.Background(), "work")
	span.End()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestTraceSampler(t *testing.T) {
	// A ratio of 0 must actually drop: NeverSample is the only way to make
	// "tracing on but nothing collected" true for a whole service.
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(rec),
		sdktrace.WithSampler(traceSampler(0)),
	)
	_, span := tp.Tracer("test").Start(context.Background(), "dropped")
	span.End()
	if got := len(rec.Ended()); got != 0 {
		t.Fatalf("sample ratio 0 recorded %d spans, want 0", got)
	}

	rec2 := tracetest.NewSpanRecorder()
	tp2 := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(rec2),
		sdktrace.WithSampler(traceSampler(1)),
	)
	_, span2 := tp2.Tracer("test").Start(context.Background(), "kept")
	span2.End()
	if got := len(rec2.Ended()); got != 1 {
		t.Fatalf("sample ratio 1 recorded %d spans, want 1", got)
	}
}
