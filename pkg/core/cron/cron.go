// Package cron runs periodic jobs in a deployment with more than one
// replica, without running each job once per replica.
//
// Every instance ticks. Before a job runs, the instance tries to take a
// redis lock named after the job: the one that gets it runs the job, the
// others skip that tick. With no redis configured (a single replica, a test)
// jobs simply run — which is the right behaviour for one process and the
// wrong one for a fleet.
//
//	sched := cron.New(cron.Config{Redis: env.Redis, Timeout: time.Minute})
//	sched.Add(cron.Job{
//		Name:     "expire-orders",
//		Interval: 5 * time.Minute,
//		Run:      s.expireOrders,
//	})
//	env.Block("cron", sched.Run)   // verticle drives it and stops it on shutdown
package cron

import (
	"context"
	"sync"
	"time"

	"github.com/smallnest/matex/pkg/core/obs"
	"github.com/smallnest/matex/pkg/core/redis"
)

// Job is one periodic task.
type Job struct {
	// Name identifies the job: it is the lock key and the log field, so keep
	// it stable and unique across the service.
	Name string
	// Interval is how often the job should run. It doubles as the lock TTL,
	// so a run must finish well within it — a job that overruns its interval
	// can be started by another instance while it is still going.
	Interval time.Duration
	// RunOnStart runs the job immediately at startup instead of waiting out
	// the first interval.
	RunOnStart bool
	// Run does the work. ctx is cancelled when the scheduler shuts down.
	Run func(ctx context.Context) error
}

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
		case j.Interval <= 0:
			panic("cron: job " + j.Name + " needs a positive interval")
		case j.Run == nil:
			panic("cron: job " + j.Name + " needs a Run function")
		}
		s.jobs = append(s.jobs, j)
	}
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

// loop ticks one job.
func (s *Scheduler) loop(ctx context.Context, j Job) {
	if j.RunOnStart {
		s.runOnce(ctx, j)
	}
	t := time.NewTicker(j.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.runOnce(ctx, j)
		}
	}
}

// runOnce claims the job's slot and, if it wins, runs it.
func (s *Scheduler) runOnce(ctx context.Context, j Job) {
	if s.cfg.Redis != nil {
		// The claim is held for a whole interval and then expires on its own.
		// Releasing it when the work finishes would defeat the point: two
		// instances tick on offset clocks, so a slot freed early is a slot
		// the other instance will happily take, and the job runs once per
		// replica after all.
		claimed, err := s.cfg.Redis.Claim(ctx, s.cfg.Prefix+j.Name, j.Interval)
		if err != nil {
			obs.Warn(ctx, "cron: leader claim unavailable, skipping this tick", "job", j.Name, "err", err)
			return
		}
		if !claimed {
			return // another instance owns this interval
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
