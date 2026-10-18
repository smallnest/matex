// Command audit demonstrates pkg/core/audit: one structured record per
// business action — who, what, which resource, how it ended — written either
// to the log pipeline (the default) or to a durable sink (a table, a Kafka
// topic, an append-only file).
//
// Run:
//
//	go run ./examples/audit
//
//	curl -i -X POST localhost:8086/api/v1/orders/42/refund -H 'X-API-Key: demo-key'
//	curl -i -X POST localhost:8086/api/v1/orders/42/refund -H 'X-API-Key: demo-key'  # 409, audited as failure
//	curl -s localhost:8086/api/v1/audit                                              # the in-memory trail
//
//	go test ./examples/audit
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync"

	"github.com/smallnest/matex/pkg/core/audit"
	"github.com/smallnest/matex/pkg/core/errs"
	"github.com/smallnest/matex/pkg/core/httpx"
	"github.com/smallnest/matex/pkg/core/verticle"
)

type auditConfig struct {
	APIKey string `json:"api_key" default:"demo-key"`
}

type auditService struct {
	cfg   auditConfig
	store *orderStore
}

func (s *auditService) Name() string { return "audit" }

func (s *auditService) Setup(_ context.Context, env *verticle.Env) error {
	if err := env.DecodeService(&s.cfg); err != nil {
		return err
	}
	// A second destination beside the default log sink: the demo keeps the
	// trail in memory so it can be read back. A real service writes to a
	// table or an append-only topic here — synchronously, and reporting
	// write errors, or the trail has holes.
	s.store = &orderStore{refunded: map[int64]bool{}}
	audit.SetSink(s.store)
	return nil
}

func (s *auditService) BuildRouter(srv *httpx.Server) error {
	srv.Handle("POST", "/api/v1/orders/{id}/refund", s.requireKey(s.refund))
	srv.Handle("GET", "/api/v1/audit", s.trail)
	return nil
}

func (s *auditService) requireKey(next httpx.HandlerFunc) httpx.HandlerFunc {
	return func(ctx context.Context, r *http.Request) (any, error) {
		if s.cfg.APIKey != "" && r.Header.Get("X-API-Key") != s.cfg.APIKey {
			return nil, errs.Unauthorized(40101, "missing or invalid X-API-Key")
		}
		return next(ctx, r)
	}
}

// orderStore is both the demo's business store and its audit sink.
type orderStore struct {
	mu       sync.Mutex
	refunded map[int64]bool
	events   []audit.Event
}

func (o *orderStore) Write(_ context.Context, e audit.Event) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, e)
}

func (o *orderStore) eventsCopy() []audit.Event {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]audit.Event(nil), o.events...)
}

// refund is one business action with one audit record per outcome. The
// record is written before returning, so the trail matches what the caller
// saw — a 409 is as auditable as a success.
func (s *auditService) refund(ctx context.Context, r *http.Request) (any, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return nil, errs.Invalid(40001, "id must be a number")
	}
	actor := r.Header.Get("X-User")
	if actor == "" {
		actor = "anonymous"
	}
	resource := fmt.Sprintf("order/%d", id)

	s.store.mu.Lock()
	done := s.store.refunded[id]
	s.store.mu.Unlock()
	if done {
		audit.Record(ctx, audit.Event{
			Actor: actor, Action: "order.refund", Resource: resource, Result: audit.Failure,
			Detail: map[string]string{"reason": "already refunded"},
		})
		return nil, errs.Conflict(40901, "order already refunded")
	}
	s.store.mu.Lock()
	s.store.refunded[id] = true
	s.store.mu.Unlock()
	audit.Record(ctx, audit.Event{
		Actor: actor, Action: "order.refund", Resource: resource, Result: audit.Success,
	})
	return map[string]any{"refunded": id}, nil
}

func (s *auditService) trail(_ context.Context, _ *http.Request) (any, error) {
	out := []any{}
	for _, e := range s.store.eventsCopy() {
		out = append(out, map[string]any{
			"at":       e.At,
			"actor":    e.Actor,
			"action":   e.Action,
			"resource": e.Resource,
			"result":   string(e.Result),
			"detail":   e.Detail,
			"trace_id": e.TraceID,
		})
	}
	return out, nil
}

func main() {
	conf := flag.String("conf", "examples/audit/config.yaml", "config file")
	flag.Parse()

	if err := verticle.Run(context.Background(), &auditService{}, verticle.WithConf(*conf)); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}
