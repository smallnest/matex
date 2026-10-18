// Cross-process trace propagation for rpcx: the W3C trace context rides in
// the request metadata (traceparent key), which rpcx copies from
// ctx.Value(share.ReqMetaDataKey) onto the wire and back into the request
// message on the server. The server plugin opens a span parented to the
// caller's trace; the client wrapper opens one and stamps the metadata.
// Both are no-ops when tracing is off, so they are wired unconditionally.
package rpcx

import (
	"context"

	rpcxclient "github.com/smallnest/rpcx/client"
	"github.com/smallnest/rpcx/protocol"
	rpcxserver "github.com/smallnest/rpcx/server"
	"github.com/smallnest/rpcx/share"
	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/smallnest/matex/pkg/core/obs"
)

// mdCarrier adapts rpcx's request metadata (a map[string]string) to
// OpenTelemetry's TextMapCarrier.
type mdCarrier map[string]string

func (c mdCarrier) Get(k string) string { return c[k] }
func (c mdCarrier) Set(k, v string)     { c[k] = v }
func (c mdCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}

// traceSpanKey holds the live server span in the request's share.Context.
// rpcx passes the same *share.Context through PreHandleRequest → the
// service method → PostCall, so storing the span there lets PostCall close
// exactly the span PreHandleRequest opened.
type traceSpanKey struct{}

// tracePlugin opens a server span per request and closes it after the
// service method returns. Business handlers do not need to touch it; a
// handler's own obs.StartSpan calls won't nest under this span (the OTel
// span context key cannot be written from outside), which is documented.
type tracePlugin struct{}

var _ rpcxserver.PreHandleRequestPlugin = tracePlugin{}
var _ rpcxserver.PostCallPlugin = tracePlugin{}

func (tracePlugin) PreHandleRequest(ctx context.Context, r *protocol.Message) error {
	cctx := obs.Extract(ctx, mdCarrier(r.Metadata))
	_, span := obs.StartRPCSpan(cctx, r.ServicePath+"."+r.ServiceMethod, trace.SpanKindServer,
		attribute.String("rpc.system", "rpcx"),
		attribute.String("rpc.method", r.ServicePath+"."+r.ServiceMethod))
	if sc, ok := ctx.(*share.Context); ok {
		sc.SetValue(traceSpanKey{}, span)
	} else {
		span.End() // no share.Context to hang the span on: close it immediately
	}
	return nil
}

func (tracePlugin) PostCall(ctx context.Context, serviceName, methodName string, args, reply interface{}, err error) (interface{}, error) {
	if sc, ok := ctx.(*share.Context); ok {
		if v := sc.Value(traceSpanKey{}); v != nil {
			span := v.(trace.Span)
			if err != nil {
				span.SetStatus(otelcodes.Error, err.Error())
			}
			span.End()
			sc.DeleteKey(traceSpanKey{})
		}
	}
	return reply, err
}

// traceXClient embeds an XClient and adds a client span plus traceparent
// injection per call. rpcx's Failtry retries re-send the same request
// under the same span, as a single logical call should be.
type traceXClient struct {
	rpcxclient.XClient
}

func (c *traceXClient) Call(ctx context.Context, serviceMethod string, args, reply any) error {
	ctx, span := obs.StartRPCSpan(ctx, serviceMethod, trace.SpanKindClient,
		attribute.String("rpc.system", "rpcx"), attribute.String("rpc.method", serviceMethod))
	err := c.XClient.Call(withTrace(ctx), serviceMethod, args, reply)
	if err != nil {
		span.SetStatus(otelcodes.Error, err.Error())
	}
	span.End()
	return err
}

func (c *traceXClient) Go(ctx context.Context, serviceMethod string, args, reply any, done chan *rpcxclient.Call) (*rpcxclient.Call, error) {
	ctx, span := obs.StartRPCSpan(ctx, serviceMethod, trace.SpanKindClient,
		attribute.String("rpc.system", "rpcx"), attribute.String("rpc.method", serviceMethod))
	call, err := c.XClient.Go(withTrace(ctx), serviceMethod, args, reply, done)
	if err != nil {
		span.SetStatus(otelcodes.Error, err.Error())
		span.End()
		return call, err
	}
	// Async: close the span when the call completes, even if nobody reads
	// the Done channel (it is always closed by rpcx).
	go func() {
		<-call.Done
		if call.Error != nil {
			span.SetStatus(otelcodes.Error, call.Error.Error())
		}
		span.End()
	}()
	return call, nil
}

// withTrace injects the current trace context into the metadata map rpcx
// copies from the context onto the wire. The existing metadata is copied so
// the caller's map is never mutated.
func withTrace(ctx context.Context) context.Context {
	md := map[string]string{}
	if m, ok := ctx.Value(share.ReqMetaDataKey).(map[string]string); ok {
		for k, v := range m {
			md[k] = v
		}
	}
	obs.Inject(ctx, mdCarrier(md))
	return context.WithValue(ctx, share.ReqMetaDataKey, md)
}
