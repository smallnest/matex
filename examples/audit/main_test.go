package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/smallnest/matex/pkg/core/audit"
	"github.com/smallnest/matex/pkg/core/httpx"
)

// newTestHandler runs the service exactly as verticle does: Setup (which
// installs the audit sink), then BuildRouter.
func newTestHandler(t *testing.T) (http.Handler, *auditService) {
	t.Helper()
	svc := &auditService{}
	svc.cfg = auditConfig{APIKey: "demo-key"}
	svc.store = &orderStore{refunded: map[int64]bool{}}
	audit.SetSink(svc.store)
	t.Cleanup(func() { audit.SetSink(nil) })

	srv := httpx.New(httpx.Config{Timeout: time.Second})
	if err := svc.BuildRouter(srv); err != nil {
		t.Fatalf("build router: %v", err)
	}
	return srv.Handler(), svc
}

func post(h http.Handler, path, key, user string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, nil)
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	if user != "" {
		req.Header.Set("X-User", user)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestEachOutcomeIsAudited(t *testing.T) {
	h, svc := newTestHandler(t)

	if rec := post(h, "/api/v1/orders/42/refund", "demo-key", "alice"); rec.Code != http.StatusOK {
		t.Fatalf("refund: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(h, "/api/v1/orders/42/refund", "demo-key", "alice"); rec.Code != http.StatusConflict {
		t.Fatalf("second refund: %d, want 409", rec.Code)
	}

	events := svc.store.eventsCopy()
	if len(events) != 2 {
		t.Fatalf("audited %d events, want 2: %+v", len(events), events)
	}
	ok, dup := events[0], events[1]
	if ok.Actor != "alice" || ok.Action != "order.refund" || ok.Resource != "order/42" || ok.Result != audit.Success {
		t.Fatalf("success event = %+v", ok)
	}
	if dup.Result != audit.Failure || dup.Detail == nil {
		t.Fatalf("failure event must carry the reason: %+v", dup)
	}
	// Record stamps time and trace id, so a trail can be joined to logs.
	if ok.At.IsZero() || ok.TraceID == "" {
		t.Fatalf("event not stamped: %+v", ok)
	}
}

func TestRejectedRequestsAreNotBusinessActions(t *testing.T) {
	h, svc := newTestHandler(t)
	if rec := post(h, "/api/v1/orders/42/refund", "wrong", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := svc.store.eventsCopy(); len(got) != 0 {
		t.Fatalf("an auth rejection was audited as a business action: %+v", got)
	}
}

func TestTrailReportsWhatWasAudited(t *testing.T) {
	h, _ := newTestHandler(t)
	post(h, "/api/v1/orders/7/refund", "demo-key", "bob")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/audit", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `"resource":"order/7"`) || !strings.Contains(body, `"actor":"bob"`) {
		t.Fatalf("trail: %d %s", rec.Code, body)
	}
}
