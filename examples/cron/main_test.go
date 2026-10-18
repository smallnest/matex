package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/smallnest/matex/pkg/core/cron"
	"github.com/smallnest/matex/pkg/core/httpx"
	"github.com/smallnest/matex/pkg/core/obs"
	"github.com/smallnest/matex/pkg/core/redistest"
	"github.com/smallnest/matex/pkg/core/verticle"
)

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	waitForWithin(t, 2*time.Second, cond)
}

func waitForWithin(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition never became true")
}

// newService runs Setup (which applies the config defaults) and returns the
// service plus a handler over its routes. Infra handles are private to Env;
// a test that needs redis sets svc.redis itself (TestLeaderElection…).
func newService(t *testing.T) (*cronService, http.Handler) {
	t.Helper()
	svc := &cronService{}
	env := &verticle.Env{Metrics: obs.NewMetrics()}
	if err := svc.Setup(t.Context(), env); err != nil {
		t.Fatalf("setup: %v", err)
	}
	srv := httpx.New(httpx.Config{Timeout: 5 * time.Second})
	if err := svc.BuildRouter(srv); err != nil {
		t.Fatalf("build router: %v", err)
	}
	return svc, srv.Handler()
}

// fastScheduler builds the same job set on a short clock: the config default
// is meant for watching, not for tests. The cron and one-shot jobs keep
// their config timings, so they stay quiet inside the fast interval tests.
func fastScheduler(svc *cronService, interval time.Duration) {
	svc.sched = cron.New(cron.Config{})
	svc.sched.Add(svc.jobs(interval, interval, svc.cfg.DailyReport, svc.cfg.NotifyDelay)...)
}

func TestJobsRunOnTheirIntervals(t *testing.T) {
	svc, _ := newService(t)
	fastScheduler(svc, 5*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = svc.sched.Run(ctx)
	}()

	// heartbeat runs on start, so both jobs must be advancing.
	waitFor(t, func() bool {
		return svc.stats.count("heartbeat") >= 2 && svc.stats.count("reconcile") >= 2
	})

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the scheduler did not stop on cancellation")
	}
}

// TestRunOnStartBumpsImmediately: the heartbeat job is configured to run at
// startup rather than after the first interval.
func TestRunOnStartBumpsImmediately(t *testing.T) {
	svc, _ := newService(t)
	svc.sched = cron.New(cron.Config{})
	svc.sched.Add(svc.jobs(time.Hour, time.Hour, svc.cfg.DailyReport, svc.cfg.NotifyDelay)...)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = svc.sched.Run(ctx)
	}()

	waitFor(t, func() bool { return svc.stats.count("heartbeat") == 1 })
	// Only heartbeat has RunOnStart, so reconcile is still at zero.
	if got := svc.stats.count("reconcile"); got != 0 {
		t.Fatalf("reconcile ran %d times; it has no RunOnStart", got)
	}
	cancel()
	<-done
}

// TestJobFailureDoesNotStopTheLoop: every third reconcile run returns an
// error, and the schedule keeps going.
func TestJobFailureDoesNotStopTheLoop(t *testing.T) {
	svc, _ := newService(t)
	jobs := svc.jobs(time.Hour, time.Hour, svc.cfg.DailyReport, svc.cfg.NotifyDelay)
	reconcile := jobs[1]

	var failures int
	for i := 1; i <= 6; i++ {
		if err := reconcile.Run(context.Background()); err != nil {
			failures++
		}
	}
	if failures != 2 {
		t.Fatalf("failures = %d, want 2 (every third run)", failures)
	}
	if got := svc.stats.count("reconcile"); got != 6 {
		t.Fatalf("ran %d times, want 6 — a failure must not drop the job", got)
	}
}

// TestLeaderElectionRunsOneInstancePerInterval is the claim the package
// makes: two replicas ticking together still run the job once. Both
// schedulers start with RunOnStart, so they race for the same claim and the
// loser does nothing — with the interval set to a minute the miniredis claim
// stays held for the whole test.
func TestLeaderElectionRunsOneInstancePerInterval(t *testing.T) {
	rc := redistest.Start(t)

	replicaA, _ := newService(t)
	replicaB, _ := newService(t)
	replicaA.redis = rc
	replicaB.redis = rc

	stopA := startScheduler(t, replicaA, replicaA.jobs(time.Minute, time.Minute, replicaA.cfg.DailyReport, replicaA.cfg.NotifyDelay)...)
	defer stopA()
	// Wait for A to have taken the claim.
	waitFor(t, func() bool { return replicaA.stats.count("heartbeat") == 1 })

	stopB := startScheduler(t, replicaB, replicaB.jobs(time.Minute, time.Minute, replicaB.cfg.DailyReport, replicaB.cfg.NotifyDelay)...)
	defer stopB()
	// Give B's startup run a chance to try — and fail — the claim.
	time.Sleep(50 * time.Millisecond)

	total := replicaA.stats.count("heartbeat") + replicaB.stats.count("heartbeat")
	if total != 1 {
		t.Fatalf("two replicas ran the job %d times in one interval, want 1", total)
	}
}

// TestOneShotLeaderElection: two replicas scheduling the same one-shot on
// offset start times still run it once. The winner's claim never expires, so
// the replica that starts later finds the slot permanently held.
func TestOneShotLeaderElection(t *testing.T) {
	rc := redistest.Start(t)

	replicaA, _ := newService(t)
	replicaB, _ := newService(t)
	replicaA.redis = rc
	replicaB.redis = rc

	stopA := startScheduler(t, replicaA, replicaA.jobs(time.Minute, time.Minute, "@every 1h", 5*time.Millisecond)...)
	defer stopA()
	// Wait for A's one-shot to have fired and claimed the slot for good.
	waitFor(t, func() bool { return replicaA.stats.count("notify") == 1 })

	stopB := startScheduler(t, replicaB, replicaB.jobs(time.Minute, time.Minute, "@every 1h", 5*time.Millisecond)...)
	defer stopB()
	// Give B's one-shot a chance to fire — and fail — the claim.
	time.Sleep(50 * time.Millisecond)

	total := replicaA.stats.count("notify") + replicaB.stats.count("notify")
	if total != 1 {
		t.Fatalf("two replicas ran the one-shot %d times, want 1", total)
	}
}

// startScheduler runs the given jobs in the background and returns a stop
// func that waits for it to unwind.
func startScheduler(t *testing.T, svc *cronService, jobs ...cron.Job) func() {
	t.Helper()
	svc.sched = cron.New(cron.Config{Redis: svc.redis})
	svc.sched.Add(jobs...)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = svc.sched.Run(ctx)
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("the scheduler did not stop")
		}
	}
}

func TestJobsEndpointReportsCounters(t *testing.T) {
	svc, h := newService(t)
	svc.stats.record("heartbeat")
	svc.stats.record("heartbeat")
	svc.stats.record("reconcile")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
	}

	var env struct {
		Data struct {
			Jobs map[string]struct {
				Runs int    `json:"runs"`
				Last string `json:"last"`
			} `json:"jobs"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if got := env.Data.Jobs["heartbeat"].Runs; got != 2 {
		t.Fatalf("heartbeat runs = %d, want 2", got)
	}
	if got := env.Data.Jobs["reconcile"].Runs; got != 1 {
		t.Fatalf("reconcile runs = %d, want 1", got)
	}
	if env.Data.Jobs["heartbeat"].Last == "" {
		t.Fatal("the last run time is not reported")
	}
}

func TestConfigDefaults(t *testing.T) {
	svc, _ := newService(t)
	if svc.cfg.HeartbeatInterval != 500*time.Millisecond ||
		svc.cfg.ReconcileInterval != 2*time.Second ||
		svc.cfg.DailyReport != "@every 5s" ||
		svc.cfg.NotifyDelay != 10*time.Second ||
		svc.cfg.JobTimeout != 5*time.Second {
		t.Fatalf("defaults not applied: %+v", svc.cfg)
	}
	names := make([]string, 0, 4)
	for _, j := range svc.sched.Jobs() {
		names = append(names, j.Name)
	}
	want := []string{"heartbeat", "reconcile", "report", "notify"}
	if len(names) != len(want) {
		t.Fatalf("registered jobs = %v", names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("registered jobs = %v", names)
		}
	}
	// No redis means no leader election, which is the single-replica case.
	if svc.redis != nil {
		t.Fatal("redis should be nil without a redis section")
	}
}

// TestCronJobFiresOnItsSchedule drives the cron-expression job end to end.
// "@every 1s" is the fastest cadence robfig's real clock supports; the config
// default ("@every 5s") would not fire inside a test.
func TestCronJobFiresOnItsSchedule(t *testing.T) {
	svc, _ := newService(t)
	svc.sched = cron.New(cron.Config{})
	svc.sched.Add(svc.jobs(time.Hour, time.Hour, "@every 1s", time.Hour)...)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = svc.sched.Run(ctx)
	}()

	waitForWithin(t, 3*time.Second, func() bool { return svc.stats.count("report") >= 2 })
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the scheduler did not stop on cancellation")
	}
}

// TestOneShotFiresExactlyOnce: a one-shot runs once, then stays quiet.
func TestOneShotFiresExactlyOnce(t *testing.T) {
	svc, _ := newService(t)
	svc.sched = cron.New(cron.Config{})
	svc.sched.Add(svc.jobs(time.Hour, time.Hour, "@every 1h", 5*time.Millisecond)...)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = svc.sched.Run(ctx)
	}()

	waitFor(t, func() bool { return svc.stats.count("notify") == 1 })
	time.Sleep(20 * time.Millisecond)
	if got := svc.stats.count("notify"); got != 1 {
		t.Fatalf("one-shot ran %d times, want exactly 1", got)
	}
	cancel()
	<-done
}
