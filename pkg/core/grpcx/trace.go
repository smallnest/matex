// Cross-process trace propagation for gRPC: the W3C trace context rides in
// the metadata (traceparent key), just as it rides in HTTP headers. The
// interceptors are no-ops when tracing is off — obs falls back to
// OpenTelemetry's no-op tracer and propagator — so they are wired
// unconditionally in New and Dial and business code does nothing.
package grpcx

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/smallnest/matex/pkg/core/obs"
)

// mdCarrier adapts grpc metadata to OpenTelemetry's TextMapCarrier so the
// W3C propagator can read/write traceparent in it.
type mdCarrier metadata.MD

func (c mdCarrier) Get(k string) string {
	if vs := metadata.MD(c).Get(k); len(vs) > 0 {
		return vs[0]
	}
	return ""
}

func (c mdCarrier) Set(k, v string) { metadata.MD(c).Set(k, v) }

func (c mdCarrier) Keys() []string {
	keys := make([]string, 0, len(metadata.MD(c)))
	for k := range metadata.MD(c) {
		keys = append(keys, k)
	}
	return keys
}

// rpcAttrs are the low-cardinality attributes OpenTelemetry's gRPC
// conventions expect, derived from the full method name.
func rpcAttrs(fullMethod string) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("rpc.system", "grpc"),
		attribute.String("rpc.method", fullMethod),
	}
}

// endRPCSpan records the outcome and closes the span.
func endRPCSpan(span trace.Span, err error) {
	if err != nil {
		span.SetStatus(otelcodes.Error, status.Code(err).String())
	}
	span.End()
}

// traceUnaryServer joins the caller's trace and opens a server span around
// the handler call.
func traceUnaryServer(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	ctx = obs.Extract(ctx, mdCarrier(md))
	ctx, span := obs.StartRPCSpan(ctx, info.FullMethod, trace.SpanKindServer, rpcAttrs(info.FullMethod)...)
	resp, err := handler(ctx, req)
	endRPCSpan(span, err)
	return resp, err
}

// tracedStream swaps the stream's context for the traced one, so the
// handler's ctx carries the span.
type tracedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (t tracedStream) Context() context.Context { return t.ctx }

func traceStreamServer(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	md, _ := metadata.FromIncomingContext(ss.Context())
	ctx := obs.Extract(ss.Context(), mdCarrier(md))
	ctx, span := obs.StartRPCSpan(ctx, info.FullMethod, trace.SpanKindServer, rpcAttrs(info.FullMethod)...)
	err := handler(srv, tracedStream{ServerStream: ss, ctx: ctx})
	endRPCSpan(span, err)
	return err
}

// traceUnaryClient opens a client span and stamps the trace context into
// the outgoing metadata.
func traceUnaryClient(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	ctx, span := obs.StartRPCSpan(ctx, method, trace.SpanKindClient, rpcAttrs(method)...)
	ctx = injectMetadata(ctx)
	err := invoker(ctx, method, req, reply, cc, opts...)
	endRPCSpan(span, err)
	return err
}

func traceStreamClient(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	ctx, span := obs.StartRPCSpan(ctx, method, trace.SpanKindClient, rpcAttrs(method)...)
	ctx = injectMetadata(ctx)
	cs, err := streamer(ctx, desc, cc, method, opts...)
	endRPCSpan(span, err)
	return cs, err
}

// injectMetadata writes the current trace context into the outgoing
// metadata. The existing metadata is copied so the caller's map is not
// mutated concurrently.
func injectMetadata(ctx context.Context) context.Context {
	md, _ := metadata.FromOutgoingContext(ctx)
	md = metadata.MD(md).Copy()
	obs.Inject(ctx, mdCarrier(md))
	return metadata.NewOutgoingContext(ctx, md)
}
