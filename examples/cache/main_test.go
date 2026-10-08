package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smallnest/matex/pkg/core/httpx"
	"github.com/smallnest/matex/pkg/core/obs"
	"github.com/smallnest/matex/pkg/core/redistest"
	"github.com/smallnest/matex/pkg/core/verticle"
)

// newTestService runs Setup against a miniredis, so the tests exercise the
// real cache path with no external dependency.
func newTestService(t *testing.T, cfg *cacheConfig) (*cacheService, http.Handler) {
	t.Helper()
	svc := &cacheService{}
	if err := svc.Setup(t.Context(), &verticle.Env{Metrics: obs.NewMetrics()}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	svc.redis = redistest.Start(t)
	svc.users = newUserStore()
	if cfg != nil {
		svc.cfg = *cfg
	}

	srv := httpx.New(httpx.Config{Timeout: 10 * time.Second})
	if err := svc.BuildRouter(srv); err != nil {
		t.Fatalf("build router: %v", err)
	}
	return svc, srv.Handler()
}

func do(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHitAvoidsASecondRead(t *testing.T) {
	svc, h := newTestService(t, nil) // the 50ms fake query makes a miss obvious

	first := do(h, http.MethodGet, "/api/v1/users/1", "")
	if first.Code != http.StatusOK {
		t.Fatalf("miss: code %d body %s", first.Code, first.Body.String())
	}
	if got := svc.users.reads(); got != 1 {
		t.Fatalf("source reads after the miss = %d, want 1", got)
	}

	second := do(h, http.MethodGet, "/api/v1/users/1", "")
	if second.Code != http.StatusOK {
		t.Fatalf("hit: code %d", second.Code)
	}
	if got := svc.users.reads(); got != 1 {
		t.Fatalf("source reads after the hit = %d; the cache did not serve it", got)
	}
}

// TestConcurrentMissesCollapse is the stampede case: N requests for a cold
// key must produce one source read, not N.
func TestConcurrentMissesCollapse(t *testing.T) {
	svc, h := newTestService(t, nil)

	const n = 8
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = do(h, http.MethodGet, "/api/v1/users/2", "").Code
		}(i)
	}
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("request %d: code %d", i, code)
		}
	}
	if got := svc.users.reads(); got != 1 {
		t.Fatalf("source reads = %d for %d concurrent requests; the misses did not collapse", got, n)
	}
}

func TestWriteInvalidatesTheCachedValue(t *testing.T) {
	svc, h := newTestService(t, nil)

	do(h, http.MethodGet, "/api/v1/users/1", "")
	if got := svc.users.reads(); got != 1 {
		t.Fatalf("reads = %d", got)
	}

	put := do(h, http.MethodPut, "/api/v1/users/1", `{"name":"alice v2"}`)
	if put.Code != http.StatusOK {
		t.Fatalf("put: code %d body %s", put.Code, put.Body.String())
	}

	// The next read must reload and see the new value.
	rec := do(h, http.MethodGet, "/api/v1/users/1", "")
	if !strings.Contains(rec.Body.String(), "alice v2") {
		t.Fatalf("stale value served after the write: %s", rec.Body.String())
	}
	if got := svc.users.reads(); got != 2 {
		t.Fatalf("reads = %d; invalidation should have forced a reload", got)
	}
}

func TestMissingUserIsNotCached(t *testing.T) {
	svc, h := newTestService(t, nil)

	for i := 1; i <= 2; i++ {
		rec := do(h, http.MethodGet, "/api/v1/users/999", "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("call %d: code %d body %s", i, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"code":40401`) {
			t.Fatalf("call %d: body %s", i, rec.Body.String())
		}
	}
	if got := svc.users.reads(); got != 2 {
		t.Fatalf("reads = %d; a not-found result must not be cached", got)
	}
}

// TestWithoutRedisStillServes: a nil client means "no cache configured", and
// the call site needs no branch for it.
func TestWithoutRedisStillServes(t *testing.T) {
	svc := &cacheService{users: newUserStore()}
	srv := httpx.New(httpx.Config{Timeout: 10 * time.Second})
	if err := svc.BuildRouter(srv); err != nil {
		t.Fatal(err)
	}

	rec := do(srv.Handler(), http.MethodGet, "/api/v1/users/1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
	}
	// Uncached, so a second request reads again.
	do(srv.Handler(), http.MethodGet, "/api/v1/users/1", "")
	if got := svc.users.reads(); got != 2 {
		t.Fatalf("reads = %d; without a client every request must load", got)
	}
}

func TestValidation(t *testing.T) {
	_, h := newTestService(t, nil)

	if rec := do(h, http.MethodPut, "/api/v1/users/1", `{"name":""}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty name: code %d", rec.Code)
	}
	if rec := do(h, http.MethodPut, "/api/v1/users/1", `{`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body: code %d", rec.Code)
	}
}

func TestConfigDefaultsAndStats(t *testing.T) {
	svc, h := newTestService(t, nil)
	if svc.cfg.TTL != 30*time.Second || svc.cfg.Jitter != 5*time.Second {
		t.Fatalf("config defaults not applied: %+v", svc.cfg)
	}

	rec := do(h, http.MethodGet, "/api/v1/stats", "")
	var env struct {
		Data struct {
			SourceReads int `json:"source_reads"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Data.SourceReads != 0 {
		t.Fatalf("source_reads = %d before any request", env.Data.SourceReads)
	}
}
