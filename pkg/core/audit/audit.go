// Package audit records who did what to which resource, and whether it
// succeeded — the trail a compliance review or an incident investigation
// reads.
//
//	audit.Record(ctx, audit.Event{
//		Actor:    principal.ID,
//		Action:   "order.refund",
//		Resource: fmt.Sprintf("order/%d", id),
//		Result:   audit.Success,
//	})
//
// By default an event becomes one structured log line (obs.Info) carrying
// the actor, action, resource, result, the request's trace id and the
// service clock. SetSink routes events to a durable store instead (a table,
// a Kafka topic, an append-only file).
//
// The package guarantees the fields are complete and stamped, not that the
// trail is tamper-proof: ordering, retention, immutability and access control
// belong to the sink. Keep sinks honest — write them synchronously or hand
// them to a queue that cannot drop events; do not swallow write errors.
package audit

import (
	"context"
	"sync"
	"time"

	"github.com/smallnest/matex/pkg/core/obs"
)

// Result is the outcome of the audited operation.
type Result string

const (
	// Success means the operation was carried out.
	Success Result = "success"
	// Failure means the operation was refused or failed.
	Failure Result = "failure"
)

// Event is one audited operation.
type Event struct {
	// At is when it happened; Record fills it with the current time when zero.
	At time.Time
	// TraceID is the request's trace id; Record fills it from ctx when empty.
	TraceID string
	// Actor is who did it: user id, service name, "system".
	Actor string
	// Action is what was done, in dotted verb form: "order.refund".
	Action string
	// Resource is what it was done to: "order/42".
	Resource string
	// Result is Success or Failure.
	Result Result
	// Detail is optional structured context (reason, changed fields). It must
	// be JSON-friendly; never put secrets or full request bodies in it.
	Detail any
}

// Sink persists audit events. It is called synchronously from Record.
type Sink interface {
	Write(ctx context.Context, e Event)
}

var (
	mu   sync.RWMutex
	sink Sink = logSink{}
)

// SetSink replaces the destination for audit events. Passing nil restores the
// default structured-log sink.
func SetSink(s Sink) {
	mu.Lock()
	defer mu.Unlock()
	if s == nil {
		s = logSink{}
	}
	sink = s
}

// Record stamps e and writes it to the sink.
func Record(ctx context.Context, e Event) {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	if e.TraceID == "" {
		e.TraceID = obs.TraceID(ctx)
	}
	mu.RLock()
	s := sink
	mu.RUnlock()
	s.Write(ctx, e)
}

// logSink writes events as structured log lines: the default keeps the trail
// in whatever log pipeline the service already ships.
type logSink struct{}

func (logSink) Write(ctx context.Context, e Event) {
	obs.Info(ctx, "audit",
		"audit_at", e.At.UTC().Format(time.RFC3339Nano),
		"audit_actor", e.Actor,
		"audit_action", e.Action,
		"audit_resource", e.Resource,
		"audit_result", string(e.Result),
		"audit_detail", e.Detail,
	)
}
