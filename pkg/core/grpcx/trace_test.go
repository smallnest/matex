package grpcx

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

// installGlobalRecorder points OpenTelemetry's global tracer and propagator
// at a recording provider. grpcx's interceptors call obs.Tracer(), which
// falls back to the global one while no verticle has installed its own —
// exactly the state this package's tests run in.
func installGlobalRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	prevProvider, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	rec := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}))
	t.Cleanup(func() {
		otel.SetTracerProvider(prevProvider)
		otel.SetTextMapPropagator(prevProp)
	})
	return rec
}

// TestTraceRoundTrip drives one unary call through a real in-process server:
// the client span must be the parent of the server span and both must share
// one trace id, exactly as two separate services would.
func TestTraceRoundTrip(t *testing.T) {
	rec := installGlobalRecorder(t)

	srv, err := New(Config{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	var handled bool
	srv.GRPC().RegisterService(&grpc.ServiceDesc{
		ServiceName: "test.Echo",
		HandlerType: (*any)(nil),
		Methods: []grpc.MethodDesc{
			{MethodName: "Ping", Handler: func(_ any, ctx context.Context, _ func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
				// Generated handlers thread the interceptor through exactly
				// this way; mirror it so the tracing interceptor really runs.
				handler := func(ctx context.Context, _ any) (any, error) {
					handled = true
					return &emptypb.Empty{}, nil
				}
				if interceptor == nil {
					return handler(ctx, &emptypb.Empty{})
				}
				return interceptor(ctx, &emptypb.Empty{}, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/test.Echo/Ping"}, handler)
			}},
		},
	}, nil)
	go func() { _ = srv.Serve() }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := Dial(ctx, srv.Addr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.Invoke(ctx, "/test.Echo/Ping", &emptypb.Empty{}, &emptypb.Empty{}); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !handled {
		t.Fatal("server handler was not called")
	}

	// The recorder may still be mid-batch; poll until both sides are in.
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
