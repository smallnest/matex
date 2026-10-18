// Package cron runs scheduled jobs in a deployment with more than one
// replica, without running each job once per replica.
//
// Every instance ticks. Before a job runs, the instance tries to take a
// redis lock named after the job: the one that gets it runs the job, the
// others skip that slot. With no redis configured (a single replica, a test)
// jobs simply run — which is the right behaviour for one process and the
// wrong one for a fleet.
//
// A job is scheduled exactly one of three ways:
//
//   - Interval: a fixed cadence, e.g. every 5 minutes.
//
//   - Schedule: a cron expression, e.g. "0 9 * * *" — the standard 5-field
//     syntax plus descriptors such as "@every 5m" or "@daily". Times follow
//     the scheduler's local timezone, like host cron.
//
//   - At or Delay: a one-shot run, at an absolute time or a delay after the
//     scheduler starts. A one-shot runs at most once: its claim never
//     expires, so a replica waking late cannot re-run it.
//
//     rc, _ := env.Redis()
//     sched := cron.New(cron.Config{Redis: rc, Timeout: time.Minute})
//     sched.Add(
//     cron.Job{Name: "expire-orders", Interval: 5 * time.Minute, Run: s.expireOrders},
//     cron.Job{Name: "daily-report", Schedule: "0 9 * * *", Run: s.dailyReport},
//     cron.Job{Name: "notify", Delay: 10 * time.Minute, Run: s.notify},
//     )
//     env.Block("cron", sched.Run)   // verticle drives it and stops it on shutdown
package cron

import (
	"context"
	"strconv"
	"sync"
	"time"

	robfig "github.com/robfig/cron/v3"

	"github.com/smallnest/matex/pkg/core/obs"
	"github.com/smallnest/matex/pkg/core/redis"
)

// Job is one scheduled task. Exactly one of Interval, Schedule, At or Delay
// must be set; Add panics otherwise.
type Job struct {
	// Name identifies the job: it is the lock key and the log field, so keep
	// it stable and unique across the service.
	Name string
	// Interval schedules a recurring run at a fixed cadence. It doubles as
	// the claim TTL, so a run must finish well within it — a job that
	// overruns its interval can be started by another instance while it is
	// still going.
	Interval time.Duration
	// Schedule schedules a recurring run on a cron expression ("0 9 * * *").
	// It accepts the standard 5-field syntax plus the @every/@daily family of
	// descriptors. Parsed at Add, so an invalid expression fails startup.
	// Missed fires are not back-filled: after a long run the next run is the
	// next fire strictly after now, like host cron.
	Schedule string
	// At schedules a one-shot run at an absolute time. A one-shot whose time
	// has already passed runs immediately at start.
	At time.Time
	// Delay schedules a one-shot run that long after the scheduler starts.
	// Mutually exclusive with At.
	Delay time.Duration
	// RunOnStart runs the job immediately at startup instead of waiting out
	// the first interval. It applies to Interval jobs only.
	RunOnStart bool
	// Run does the work. ctx is cancelled when the scheduler shuts down.
	Run func(ctx context.Context) error

	// schedule is the parsed form of Schedule, filled in by Add. It stays
	// unexported so robfig/cron is an internal detail of this package.
	schedule nextCron
}

// nextCron returns the next fire time strictly after t.
type nextCron interface{ Next(time.Time) time.Time }

// Config configures a Scheduler.
type Config struct {
	// Redis enables leader election across instances. nil runs every job in
	// this process unconditionally.
	Redis *redis.Client
	// Prefix namespaces the lock keys.
	Prefix string
	// Timeout bounds a single run. Zero means the run is bounded only by the
	// scheduler's context.
	Timeout time.Duration
}

func (c Config) withDefaults() Config {
	if c.Prefix == "" {
		c.Prefix = "cron:"
	}
	return c
}

// Scheduler runs a set of jobs.
type Scheduler struct {
	cfg  Config
	jobs []Job
}

// New creates a scheduler.
func New(cfg Config) *Scheduler {
	return &Scheduler{cfg: cfg.withDefaults()}
}

// Add registers jobs. It panics on an invalid job — a misconfigured
// scheduler should fail at startup, not silently skip work for weeks.
func (s *Scheduler) Add(jobs ...Job) {
	for _, j := range jobs {
		switch {
		case j.Name == "":
			panic("cron: job name is required")
		case j.Run == nil:
			panic("cron: job " + j.Name + " needs a Run function")
		}
		if n := j.schedules(); n != 1 {
			panic("cron: job " + j.Name + " needs exactly one of Interval, Schedule, At, Delay")
		}
		if j.RunOnStart && (j.Schedule != "" || !j.At.IsZero() || j.Delay > 0) {
			panic("cron: job " + j.Name + ": RunOnStart applies to Interval jobs only")
		}
		if j.Schedule != "" {
			parsed, err := robfig.ParseStandard(j.Schedule)
			if err != nil {
				panic("cron: job " + j.Name + ": bad schedule " + strconv.Quote(j.Schedule) + ": " + err.Error())
			}
			j.schedule = parsed
		}
		s.jobs = append(s.jobs, j)
	}
}

// schedules counts how many of the four schedule fields are set.
func (j Job) schedules() int {
	n := 0
	if j.Interval > 0 {
		n++
	}
	if j.Schedule != "" {
		n++
	}
	if !j.At.IsZero() {
		n++
	}
	if j.Delay > 0 {
		n++
	}
	return n
}

// Jobs returns the registered jobs.
func (s *Scheduler) Jobs() []Job { return s.jobs }

// Run starts every job and blocks until ctx is done, then waits for the
// in-flight runs to return. Pass it to verticle.Env.Block so shutdown stops
// the jobs before the resources they use are closed.
func (s *Scheduler) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for _, j := range s.jobs {
		wg.Add(1)
		go func(j Job) {
			defer wg.Done()
			s.loop(ctx, j)
		}(j)
	}
	<-ctx.Done()
	wg.Wait()
	return nil
}

// loop dispatches a job to the loop of its schedule kind.
func (s *Scheduler) loop(ctx context.Context, j Job) {
	switch {
	case j.Schedule != "":
		s.loopCron(ctx, j)
	case !j.At.IsZero() || j.Delay > 0:
		s.loopOnce(ctx, j)
	default:
		s.loopInterval(ctx, j)
	}
}

// loopInterval ticks one interval job.
func (s *Scheduler) loopInterval(ctx context.Context, j Job) {
	if j.RunOnStart {
		s.runOnce(ctx, j, j.Interval)
	}
	t := time.NewTicker(j.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.runOnce(ctx, j, j.Interval)
		}
	}
}

// loopCron fires one cron job at each scheduled time. The claim covers the
// whole slot (until the next fire), so offset replicas cannot run the same
// slot twice — the same contract as loopInterval.
func (s *Scheduler) loopCron(ctx context.Context, j Job) {
	for {
		fire := j.schedule.Next(time.Now())
		t := time.NewTimer(time.Until(fire))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
			s.runOnce(ctx, j, j.schedule.Next(fire).Sub(fire))
		}
	}
}

// loopOnce runs a one-shot job at its fire time and stops. The claim never
// expires (hold 0), so the job runs at most once across the fleet: a replica
// that wakes late — or a failed run — cannot re-trigger it.
func (s *Scheduler) loopOnce(ctx context.Context, j Job) {
	fire := j.At
	if j.Delay > 0 {
		fire = time.Now().Add(j.Delay)
	}
	t := time.NewTimer(time.Until(fire))
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
		s.runOnce(ctx, j, 0)
	}
}

// runOnce claims the job's slot and, if it wins, runs it. hold bounds the
// claim; zero means it never expires (used by one-shot jobs, which must run
// at most once).
func (s *Scheduler) runOnce(ctx context.Context, j Job, hold time.Duration) {
	if s.cfg.Redis != nil {
		// The claim is held for a whole slot and then expires on its own.
		// Releasing it when the work finishes would defeat the point: two
		// instances tick on offset clocks, so a slot freed early is a slot
		// the other instance will happily take, and the job runs once per
		// replica after all.
		claimed, err := s.cfg.Redis.Claim(ctx, s.cfg.Prefix+j.Name, hold)
		if err != nil {
			obs.Warn(ctx, "cron: leader claim unavailable, skipping this tick", "job", j.Name, "err", err)
			return
		}
		if !claimed {
			return // another instance owns this slot
		}
	}

	runCtx := ctx
	if s.cfg.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, s.cfg.Timeout)
		defer cancel()
	}

	start := time.Now()
	err := j.Run(runCtx)
	dur := time.Since(start).Milliseconds()
	if err != nil {
		obs.Error(ctx, "cron job failed", "job", j.Name, "dur_ms", dur, "err", err)
		return
	}
	obs.Info(ctx, "cron job finished", "job", j.Name, "dur_ms", dur)
}
