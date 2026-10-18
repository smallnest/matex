package cron

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/smallnest/matex/pkg/core/redis"
	"github.com/smallnest/matex/pkg/core/redistest"
)

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

// start runs a scheduler in the background and returns a stop func that
// waits for it to unwind.
func start(t *testing.T, s *Scheduler) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("scheduler did not stop on cancellation")
		}
	})
	return cancel
}

func TestJobRunsOnItsInterval(t *testing.T) {
	var runs atomic.Int32
	s := New(Config{})
	s.Add(Job{Name: "tick", Interval: 5 * time.Millisecond, Run: func(context.Context) error {
		runs.Add(1)
		return nil
	}})

	start(t, s)
	waitFor(t, func() bool { return runs.Load() >= 3 })
}

func TestRunOnStart(t *testing.T) {
	var runs atomic.Int32
	s := New(Config{})
	s.Add(Job{
		Name:       "once",
		Interval:   time.Hour, // never fires again during the test
		RunOnStart: true,
		Run: func(context.Context) error {
			runs.Add(1)
			return nil
		},
	})

	start(t, s)
	waitFor(t, func() bool { return runs.Load() == 1 })

	// Give it a moment to prove it does not run twice.
	time.Sleep(20 * time.Millisecond)
	if got := runs.Load(); got != 1 {
		t.Fatalf("ran %d times, want 1", got)
	}
}

func TestJobFailureKeepsTheLoopAlive(t *testing.T) {
	var runs atomic.Int32
	s := New(Config{})
	s.Add(Job{Name: "flaky", Interval: 5 * time.Millisecond, Run: func(context.Context) error {
		runs.Add(1)
		return errors.New("boom")
	}})

	start(t, s)
	waitFor(t, func() bool { return runs.Load() >= 3 })
}

func TestTimeoutBoundsARun(t *testing.T) {
	expired := make(chan struct{})
	var once atomic.Bool
	s := New(Config{Timeout: 10 * time.Millisecond})
	s.Add(Job{Name: "slow", Interval: time.Hour, RunOnStart: true, Run: func(ctx context.Context) error {
		<-ctx.Done()
		if once.CompareAndSwap(false, true) {
			close(expired)
		}
		return ctx.Err()
	}})

	start(t, s)
	select {
	case <-expired:
	case <-time.After(2 * time.Second):
		t.Fatal("the run was not bounded by Config.Timeout")
	}
}

// TestOnlyOneInstanceRunsAnInterval is the whole point of the claim: two
// replicas ticking on offset clocks must still run the job once per interval.
func TestOnlyOneInstanceRunsAnInterval(t *testing.T) {
	rc := redistest.Start(t)
	ctx := context.Background()
	var runs atomic.Int32

	// A one-minute interval keeps the miniredis claim alive for the whole
	// test, so the second call faces a held key.
	job := Job{Name: "shared", Interval: time.Minute, Run: func(context.Context) error {
		runs.Add(1)
		return nil
	}}

	instanceA, instanceB := New(Config{Redis: rc}), New(Config{Redis: rc})
	instanceA.Add(job)
	instanceB.Add(job)

	instanceA.runOnce(ctx, job, job.Interval)
	instanceB.runOnce(ctx, job, job.Interval)

	if got := runs.Load(); got != 1 {
		t.Fatalf("the job ran %d times in one interval, want exactly 1", got)
	}
}

// TestClaimSkippedWhenRedisIsDown: an unreachable redis means every instance
// would run the job, so the scheduler does nothing instead of stampeding.
func TestClaimSkippedWhenRedisIsDown(t *testing.T) {
	down := redis.New(goredis.NewClient(&goredis.Options{
		Addr:        "127.0.0.1:1", // nothing listens here
		DialTimeout: 100 * time.Millisecond,
	}))
	var runs atomic.Int32
	job := Job{Name: "j", Interval: time.Minute, Run: func(context.Context) error {
		runs.Add(1)
		return nil
	}}

	instance := New(Config{Redis: down})
	instance.Add(job)
	instance.runOnce(context.Background(), job, job.Interval)

	if got := runs.Load(); got != 0 {
		t.Fatalf("ran %d times without a leader claim", got)
	}
}

func TestNoRedisRunsUnconditionally(t *testing.T) {
	var runs atomic.Int32
	job := Job{Name: "j", Interval: time.Minute, Run: func(context.Context) error {
		runs.Add(1)
		return nil
	}}

	instance := New(Config{})
	instance.Add(job)
	instance.runOnce(context.Background(), job, job.Interval)
	instance.runOnce(context.Background(), job, job.Interval)

	// Without redis there is no leader election by design: one replica is
	// assumed.
	if got := runs.Load(); got != 2 {
		t.Fatalf("ran %d times, want 2", got)
	}
}

func TestAddRejectsInvalidJobs(t *testing.T) {
	run := func(context.Context) error { return nil }
	cases := []struct {
		name string
		job  Job
	}{
		{"no name", Job{Interval: time.Second, Run: run}},
		{"no schedule at all", Job{Name: "x", Run: run}},
		{"zero interval", Job{Name: "x", Interval: 0, Run: run}},
		{"no run", Job{Name: "x", Interval: time.Second}},
		{"interval and schedule", Job{Name: "x", Interval: time.Second, Schedule: "@daily", Run: run}},
		{"at and delay", Job{Name: "x", At: time.Now(), Delay: time.Second, Run: run}},
		{"bad cron expression", Job{Name: "x", Schedule: "not a cron", Run: run}},
		{"run on start on a cron job", Job{Name: "x", Schedule: "@daily", RunOnStart: true, Run: run}},
		{"run on start on a one-shot", Job{Name: "x", Delay: time.Second, RunOnStart: true, Run: run}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected a panic: a bad job must fail at startup")
				}
			}()
			New(Config{}).Add(c.job)
		})
	}
}

func TestConfigDefaults(t *testing.T) {
	if got := New(Config{}).cfg.Prefix; got != "cron:" {
		t.Fatalf("prefix = %q, want cron:", got)
	}
	// A caller-supplied prefix survives.
	if got := New(Config{Prefix: "jobs:"}).cfg.Prefix; got != "jobs:" {
		t.Fatalf("prefix = %q", got)
	}
}

func TestJobsAreListed(t *testing.T) {
	s := New(Config{})
	s.Add(
		Job{Name: "a", Interval: time.Second, Run: func(context.Context) error { return nil }},
		Job{Name: "b", Interval: time.Second, Run: func(context.Context) error { return nil }},
	)
	if got := len(s.Jobs()); got != 2 {
		t.Fatalf("Jobs() returned %d", got)
	}
}

func TestRunBlocksUntilCancelled(t *testing.T) {
	s := New(Config{})
	s.Add(Job{Name: "a", Interval: time.Hour, Run: func(context.Context) error { return nil }})

	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		_ = s.Run(ctx)
	}()

	select {
	case <-returned:
		t.Fatal("Run returned before its context was cancelled")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

// stepSchedule fires every d — a fake clock for the cron loop. robfig's real
// schedules are minute-granular (its "@every" rounds up to a second), which
// would make this test crawl; robfig's Next is already tested upstream, so
// here we exercise our own loop against a schedule that ticks fast.
type stepSchedule struct{ d time.Duration }

func (s stepSchedule) Next(t time.Time) time.Time { return t.Add(s.d) }

// TestCronJobRunsOnItsSchedule drives the cron loop: the job fires at each
// scheduled time, and the slot claim (hold) is derived from the schedule.
func TestCronJobRunsOnItsSchedule(t *testing.T) {
	var runs atomic.Int32
	s := New(Config{})
	s.Add(Job{Name: "report", Schedule: "@hourly", Run: func(context.Context) error {
		runs.Add(1)
		return nil
	}})
	// In-package test: swap in a fast clock without touching Add's validation
	// (which still sees a valid, parsed Schedule).
	s.jobs[0].schedule = stepSchedule{5 * time.Millisecond}

	start(t, s)
	waitFor(t, func() bool { return runs.Load() >= 3 })
}

// TestOneShotRunsAtItsDelay: a Delay job fires once, then stays quiet.
func TestOneShotRunsAtItsDelay(t *testing.T) {
	var runs atomic.Int32
	s := New(Config{})
	s.Add(Job{Name: "notify", Delay: 5 * time.Millisecond, Run: func(context.Context) error {
		runs.Add(1)
		return nil
	}})

	start(t, s)
	waitFor(t, func() bool { return runs.Load() == 1 })
	time.Sleep(20 * time.Millisecond)
	if got := runs.Load(); got != 1 {
		t.Fatalf("ran %d times, want exactly 1", got)
	}
}

// TestOneShotAtInThePastRunsImmediately: a one-shot whose time has passed is
// due at start.
func TestOneShotAtInThePastRunsImmediately(t *testing.T) {
	var runs atomic.Int32
	s := New(Config{})
	s.Add(Job{Name: "late", At: time.Now().Add(-time.Minute), Run: func(context.Context) error {
		runs.Add(1)
		return nil
	}})

	start(t, s)
	waitFor(t, func() bool { return runs.Load() == 1 })
}

// TestOneShotRunsOnceAcrossInstances pins the at-most-once contract: the
// winner's claim never expires (hold 0), so the loser — waking later, like a
// replica with an offset start — finds the slot permanently held.
func TestOneShotRunsOnceAcrossInstances(t *testing.T) {
	rc := redistest.Start(t)
	ctx := context.Background()
	var runs atomic.Int32
	job := Job{Name: "shared-once", Delay: 5 * time.Millisecond, Run: func(context.Context) error {
		runs.Add(1)
		return nil
	}}

	instanceA, instanceB := New(Config{Redis: rc}), New(Config{Redis: rc})
	instanceA.Add(job)
	instanceB.Add(job)

	instanceA.runOnce(ctx, job, 0)
	time.Sleep(10 * time.Millisecond)
	instanceB.runOnce(ctx, job, 0)

	if got := runs.Load(); got != 1 {
		t.Fatalf("the one-shot ran %d times across two replicas, want 1", got)
	}
}
