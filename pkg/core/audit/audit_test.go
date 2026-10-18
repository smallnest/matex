package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/smallnest/matex/pkg/core/obs"
)

// captureLogs points slog (which obs logs through) at a buffer.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	prev := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestDefaultSinkLogsEveryField(t *testing.T) {
	buf := captureLogs(t)

	ctx := obs.WithTraceID(context.Background(), "trace-1")
	Record(ctx, Event{Actor: "user-7", Action: "order.refund", Resource: "order/42", Result: Success})

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("audit line is not JSON: %v (%s)", err, buf.String())
	}
	want := map[string]string{
		"msg":            "audit",
		"audit_actor":    "user-7",
		"audit_action":   "order.refund",
		"audit_resource": "order/42",
		"audit_result":   "success",
		"trace_id":       "trace-1",
	}
	for k, v := range want {
		if got, _ := line[k].(string); got != v {
			t.Errorf("%s = %q, want %q (line: %s)", k, line[k], v, buf.String())
		}
	}
	if at, _ := line["audit_at"].(string); at == "" {
		t.Error("audit_at is missing; the event was not stamped")
	}
}

func TestDetailAndFailureResult(t *testing.T) {
	buf := captureLogs(t)
	Record(context.Background(), Event{
		Actor: "admin", Action: "user.delete", Resource: "user/9", Result: Failure,
		Detail: map[string]string{"reason": "not the owner"},
	})
	if !strings.Contains(buf.String(), `"audit_result":"failure"`) ||
		!strings.Contains(buf.String(), "not the owner") {
		t.Fatalf("line = %s", buf.String())
	}
}

func TestSetSinkRoutes(t *testing.T) {
	buf := captureLogs(t)
	var got []Event
	rec := sinkFunc(func(_ context.Context, e Event) { got = append(got, e) })
	SetSink(rec)
	t.Cleanup(func() { SetSink(nil) })

	when := time.Unix(1700000000, 0)
	Record(obs.WithTraceID(context.Background(), "trace-2"), Event{At: when, Actor: "a", Action: "x", Result: Success})
	if len(got) != 1 {
		t.Fatalf("sink saw %d events, want 1", len(got))
	}
	// An explicit time is kept, not overwritten.
	if !got[0].At.Equal(when) || got[0].TraceID != "trace-2" {
		t.Fatalf("event = %+v; At kept and TraceID stamped expected", got[0])
	}
	if buf.Len() != 0 {
		t.Fatalf("the default sink also wrote: %s", buf.String())
	}

	// nil restores the log sink.
	SetSink(nil)
	Record(context.Background(), Event{Actor: "a", Action: "x", Result: Success})
	if buf.Len() == 0 {
		t.Fatal("SetSink(nil) did not restore the default log sink")
	}
}

type sinkFunc func(context.Context, Event)

func (f sinkFunc) Write(ctx context.Context, e Event) { f(ctx, e) }
