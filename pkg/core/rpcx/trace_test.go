package rpcx_test

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// TestTraceRoundTrip drives one real in-process call and asserts the trace
// context crossed the wire: client and server spans share a trace id and the
// server span is parented to the client span. It installs a recording
// provider on the global tracer, which obs.Tracer() falls back to while no
// verticle has installed obs's own provider.
func TestTraceRoundTrip(t *testing.T) {
	prevProvider, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	rec := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}))
	t.Cleanup(func() {
		otel.SetTracerProvider(prevProvider)
		otel.SetTextMapPropagator(prevProp)
	})

	cl := startServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var r Reply
	if err := cl.Call(ctx, "Double", &Args{In: 21}, &r); err != nil {
		t.Fatalf("call: %v", err)
	}
	if r.Out != 42 {
		t.Fatalf("out = %d, want 42", r.Out)
	}

	var client, server sdktrace.ReadOnlySpan
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && (client == nil || server == nil) {
		client, server = nil, nil
		for _, s := range rec.Ended() {
			switch s.SpanKind() {
			case trace.SpanKindClient:
				client = s
			case trace.SpanKindServer:
				server = s
			}
		}
		if client == nil || server == nil {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if client == nil || server == nil {
		t.Fatalf("client/server spans not recorded (client=%v server=%v)", client, server)
	}
	if client.SpanContext().TraceID() != server.SpanContext().TraceID() {
		t.Fatalf("trace id differs across the hop: %s vs %s",
			client.SpanContext().TraceID(), server.SpanContext().TraceID())
	}
	if server.Parent().SpanID() != client.SpanContext().SpanID() {
		t.Fatalf("server span parent = %s, want client span %s",
			server.Parent().SpanID(), client.SpanContext().SpanID())
	}
}
