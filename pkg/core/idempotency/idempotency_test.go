package idempotency

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/smallnest/matex/pkg/core/errs"
	"github.com/smallnest/matex/pkg/core/httpx"
	"github.com/smallnest/matex/pkg/core/redistest"
)

const testKey = "Idempotency-Key"

// harness wires the middleware in front of a counting handler.
type harness struct {
	srv   *httpx.Server
	calls *int32
}

func newHarness(t *testing.T, store Store, cfg Config, h httpx.HandlerFunc) *harness {
	t.Helper()
	var calls int32
	srv := httpx.New(httpx.Config{Timeout: 5 * time.Second})
	srv.Use(Middleware(store, cfg))
	srv.Handle("POST", "/pay", func(ctx context.Context, r *http.Request) (any, error) {
		atomic.AddInt32(&calls, 1)
		return h(ctx, r)
	})
	srv.Handle("GET", "/pay", func(ctx context.Context, r *http.Request) (any, error) {
		atomic.AddInt32(&calls, 1)
		return map[string]string{"method": "get"}, nil
	})
	return &harness{srv: srv, calls: &calls}
}

func (h *harness) do(method, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/pay", nil)
	if key != "" {
		req.Header.Set(testKey, key)
	}
	rec := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func (h *harness) callCount() int32 { return atomic.LoadInt32(h.calls) }

// lastData pulls the envelope's data as a map.
func lastData(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return env.Data
}

func TestDuplicateReplaysTheFirstOutcome(t *testing.T) {
	h := newHarness(t, NewMemory(), Config{}, func(_ context.Context, r *http.Request) (any, error) {
		return map[string]any{"id": "payment-1"}, nil
	})

	first := h.do(http.MethodPost, "abc")
	if first.Code != http.StatusOK {
		t.Fatalf("first: code %d body %s", first.Code, first.Body.String())
	}

	second := h.do(http.MethodPost, "abc")
	if second.Code != http.StatusOK {
		t.Fatalf("second: code %d body %s", second.Code, second.Body.String())
	}
	if data := lastData(t, second); data["id"] != "payment-1" {
		t.Fatalf("replayed data = %v", data)
	}
	if got := h.callCount(); got != 1 {
		t.Fatalf("handler ran %d times; the duplicate must be answered from the store", got)
	}
}

func TestDifferentKeysBothRun(t *testing.T) {
	h := newHarness(t, NewMemory(), Config{}, func(_ context.Context, _ *http.Request) (any, error) {
		return map[string]string{"ok": "yes"}, nil
	})

	h.do(http.MethodPost, "a")
	h.do(http.MethodPost, "b")
	if got := h.callCount(); got != 2 {
		t.Fatalf("handler ran %d times, want 2", got)
	}
}

func TestRequestWithoutKeyIsNotDeduplicated(t *testing.T) {
	h := newHarness(t, NewMemory(), Config{}, func(_ context.Context, _ *http.Request) (any, error) {
		return map[string]string{"ok": "yes"}, nil
	})

	h.do(http.MethodPost, "")
	h.do(http.MethodPost, "")
	if got := h.callCount(); got != 2 {
		t.Fatalf("handler ran %d times; requests without a key must pass through", got)
	}
}

// TestUncoveredMethodsPassThrough: GET is idempotent by definition, so the
// key means nothing there.
func TestUncoveredMethodsPassThrough(t *testing.T) {
	h := newHarness(t, NewMemory(), Config{}, func(context.Context, *http.Request) (any, error) {
		return nil, errors.New("the POST handler should not have run")
	})

	h.do(http.MethodGet, "same-key")
	h.do(http.MethodGet, "same-key")
	if got := h.callCount(); got != 2 {
		t.Fatalf("handler ran %d times, want 2", got)
	}
}

func TestClientErrorsAreReplayed(t *testing.T) {
	h := newHarness(t, NewMemory(), Config{}, func(_ context.Context, r *http.Request) (any, error) {
		return nil, errs.Invalid(40001, "amount must be positive")
	})

	first := h.do(http.MethodPost, "abc")
	second := h.do(http.MethodPost, "abc")

	for i, rec := range []*httptest.ResponseRecorder{first, second} {
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("call %d: code %d body %s", i+1, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"code":40001`) {
			t.Fatalf("call %d: body %s", i+1, rec.Body.String())
		}
	}
	if got := h.callCount(); got != 1 {
		t.Fatalf("handler ran %d times; a deterministic rejection is replayable", got)
	}
}

// TestServerErrorsReleaseTheKey: a 5xx is our own bad moment, so the key
// must not pin that failure for the whole TTL.
func TestServerErrorsReleaseTheKey(t *testing.T) {
	var attempt int32
	h := newHarness(t, NewMemory(), Config{}, func(_ context.Context, _ *http.Request) (any, error) {
		if atomic.AddInt32(&attempt, 1) == 1 {
			return nil, errs.Internal(50001, "database is on fire")
		}
		return map[string]string{"id": "payment-1"}, nil
	})

	if rec := h.do(http.MethodPost, "abc"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("first: code %d", rec.Code)
	}
	rec := h.do(http.MethodPost, "abc")
	if rec.Code != http.StatusOK {
		t.Fatalf("retry: code %d body %s; the key was not released", rec.Code, rec.Body.String())
	}
	if got := h.callCount(); got != 2 {
		t.Fatalf("handler ran %d times, want 2", got)
	}
}

func TestNilResultIsReplayedAs204(t *testing.T) {
	h := newHarness(t, NewMemory(), Config{}, func(context.Context, *http.Request) (any, error) {
		return nil, nil
	})

	h.do(http.MethodPost, "abc")
	rec := h.do(http.MethodPost, "abc")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code %d, want 204 (the first request answered 204)", rec.Code)
	}
	if got := h.callCount(); got != 1 {
		t.Fatalf("handler ran %d times", got)
	}
}

// TestConcurrentDuplicateIsRejected is the race the key exists for: a
// second request must not run the write while the first is in flight.
func TestConcurrentDuplicateIsRejected(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFirst := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseFirst)

	h := newHarness(t, NewMemory(), Config{}, func(ctx context.Context, _ *http.Request) (any, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return map[string]string{"id": "payment-1"}, nil
		}
	})

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- h.do(http.MethodPost, "abc") }()

	// Wait until the first request owns the key.
	waitFor(t, func() bool { return h.callCount() == 1 })

	duplicate := h.do(http.MethodPost, "abc")
	if duplicate.Code != http.StatusConflict {
		t.Fatalf("concurrent duplicate: code %d body %s", duplicate.Code, duplicate.Body.String())
	}
	if !strings.Contains(duplicate.Body.String(), `"code":40902`) {
		t.Fatalf("body %s missing the in-progress code", duplicate.Body.String())
	}

	releaseFirst()
	select {
	case rec := <-done:
		if rec.Code != http.StatusOK {
			t.Fatalf("first request: code %d", rec.Code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the first request never finished")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition never became true")
}

// brokenStore fails everything, standing in for a store outage.
type brokenStore struct{}

func (brokenStore) Begin(context.Context, string, time.Duration) (bool, error) {
	return false, errors.New("store is down")
}
func (brokenStore) Load(context.Context, string) (Outcome, bool, error) {
	return Outcome{}, false, errors.New("store is down")
}
func (brokenStore) Finish(context.Context, string, *Outcome, time.Duration) error {
	return errors.New("store is down")
}

// TestStoreFailureFailsOpen: without the store the request still runs. The
// alternative — rejecting every write during a store blip — is worse.
func TestStoreFailureFailsOpen(t *testing.T) {
	h := newHarness(t, brokenStore{}, Config{}, func(context.Context, *http.Request) (any, error) {
		return map[string]string{"ok": "yes"}, nil
	})

	rec := h.do(http.MethodPost, "abc")
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d, want the request to pass through", rec.Code)
	}
}

func TestMemoryExpiry(t *testing.T) {
	store := NewMemory()
	cur := time.Unix(0, 0)
	store.now = func() time.Time { return cur }

	ok, err := store.Begin(context.Background(), "k", time.Minute)
	if err != nil || !ok {
		t.Fatalf("begin: ok=%v err=%v", ok, err)
	}
	if ok, _ := store.Begin(context.Background(), "k", time.Minute); ok {
		t.Fatal("the key was claimed twice")
	}

	cur = cur.Add(2 * time.Minute)
	if ok, _ := store.Begin(context.Background(), "k", time.Minute); !ok {
		t.Fatal("an expired key should be claimable again")
	}
}

func TestMemoryFinishAndRelease(t *testing.T) {
	store := NewMemory()
	ctx := context.Background()
	ttl := time.Minute

	_, _ = store.Begin(ctx, "k", ttl)
	if _, found, _ := store.Load(ctx, "k"); found {
		t.Fatal("a pending key must not report an outcome")
	}

	outcome := &Outcome{Data: json.RawMessage(`{"id":"1"}`)}
	if err := store.Finish(ctx, "k", outcome, ttl); err != nil {
		t.Fatal(err)
	}
	got, found, err := store.Load(ctx, "k")
	if err != nil || !found {
		t.Fatalf("load: found=%v err=%v", found, err)
	}
	if string(got.Data) != `{"id":"1"}` {
		t.Fatalf("outcome = %s", got.Data)
	}

	if err := store.Finish(ctx, "k", nil, ttl); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := store.Load(ctx, "k"); found {
		t.Fatal("releasing the key should drop it")
	}
	if store.Len() != 0 {
		t.Fatalf("store holds %d keys after release", store.Len())
	}
}

func TestClassify(t *testing.T) {
	if got := classify(map[string]string{"a": "b"}, nil); got == nil || got.Failed {
		t.Fatalf("success: %+v", got)
	}
	if got := classify(nil, nil); got == nil || len(got.Data) != 0 {
		t.Fatalf("nil data: %+v", got)
	}
	if got := classify(nil, errs.NotFound(40401, "gone")); got == nil || !got.Failed || got.Code != 40401 {
		t.Fatalf("client error: %+v", got)
	}
	if got := classify(nil, errs.Internal(50001, "boom")); got != nil {
		t.Fatalf("5xx must release the key, got %+v", got)
	}
	if got := classify(nil, errors.New("plain")); got != nil {
		t.Fatalf("a non-matex error maps to 500 and must release the key, got %+v", got)
	}
}

func TestOutcomeValue(t *testing.T) {
	if v, err := (Outcome{Failed: true, Kind: int(errs.KindConflict), Code: 40901, Msg: "taken"}).value(); err == nil || v != nil {
		t.Fatalf("failed outcome: v=%v err=%v", v, err)
	}
	if v, err := (Outcome{}).value(); err != nil || v != nil {
		t.Fatalf("empty outcome should replay as 204: v=%v err=%v", v, err)
	}
	if _, err := (Outcome{Data: json.RawMessage("{")}).value(); err == nil {
		t.Fatal("corrupt data must surface as an error")
	}
	if v, err := (Outcome{Data: json.RawMessage(`[1,2]`)}).value(); err != nil {
		t.Fatalf("array data: %v", err)
	} else if arr, ok := v.([]any); !ok || len(arr) != 2 {
		t.Fatalf("v = %#v", v)
	}
}

// TestRedisStore drives the shared implementation through miniredis.
func TestRedisStore(t *testing.T) {
	store := NewRedis(redistest.Start(t))
	ctx := context.Background()
	ttl := time.Minute

	ok, err := store.Begin(ctx, "idem:k", ttl)
	if err != nil || !ok {
		t.Fatalf("begin: ok=%v err=%v", ok, err)
	}
	if ok, err := store.Begin(ctx, "idem:k", ttl); err != nil || ok {
		t.Fatalf("second begin: ok=%v err=%v", ok, err)
	}
	if _, found, err := store.Load(ctx, "idem:k"); err != nil || found {
		t.Fatalf("pending load: found=%v err=%v", found, err)
	}

	outcome := &Outcome{Data: json.RawMessage(`{"id":"1"}`)}
	if err := store.Finish(ctx, "idem:k", outcome, ttl); err != nil {
		t.Fatal(err)
	}
	got, found, err := store.Load(ctx, "idem:k")
	if err != nil || !found {
		t.Fatalf("load: found=%v err=%v", found, err)
	}
	if string(got.Data) != `{"id":"1"}` {
		t.Fatalf("outcome = %s", got.Data)
	}

	if err := store.Finish(ctx, "idem:k", nil, ttl); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := store.Load(ctx, "idem:k"); found {
		t.Fatal("release should drop the key")
	}
}

// TestRedisStoreEndToEnd runs the middleware over the redis store.
func TestRedisStoreEndToEnd(t *testing.T) {
	h := newHarness(t, NewRedis(redistest.Start(t)), Config{}, func(context.Context, *http.Request) (any, error) {
		return map[string]any{"id": "payment-9"}, nil
	})

	h.do(http.MethodPost, "abc")
	second := h.do(http.MethodPost, "abc")
	if data := lastData(t, second); data["id"] != "payment-9" {
		t.Fatalf("replayed data = %v", data)
	}
	if got := h.callCount(); got != 1 {
		t.Fatalf("handler ran %d times", got)
	}
}

func TestNewRedisRejectsNil(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for a nil client")
		}
	}()
	NewRedis(nil)
}

func TestConfigDefaults(t *testing.T) {
	cfg := Config{}.withDefaults()
	if cfg.Header != testKey || cfg.TTL != 24*time.Hour || cfg.Prefix != "idem:" {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	for _, m := range []string{http.MethodPost, http.MethodPatch, http.MethodDelete} {
		if !cfg.covers(m) {
			t.Errorf("%s should be deduplicated by default", m)
		}
	}
	if cfg.covers(http.MethodGet) {
		t.Error("GET should not be deduplicated by default")
	}
}
