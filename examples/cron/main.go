// Command cron demonstrates pkg/core/cron: periodic jobs that run once per
// deployment rather than once per replica.
//
// Every instance ticks; the instance that wins a redis claim for the job's
// interval is the one that runs it. With no redis configured (a single
// replica) the jobs simply run — `make dev` + the redis section in
// config.yaml is what makes the leader election visible: start two copies
// and watch each job's counter advance once per interval, not twice.
//
// Run:
//
//	go run ./examples/cron
//
//	curl -s localhost:8084/api/v1/jobs          # per-job run counts
//	curl -s localhost:8084/version              # built-in build metadata
//
//	go test ./examples/cron
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/smallnest/matex/pkg/core/cron"
	"github.com/smallnest/matex/pkg/core/httpx"
	"github.com/smallnest/matex/pkg/core/obs"
	"github.com/smallnest/matex/pkg/core/redis"
	"github.com/smallnest/matex/pkg/core/verticle"
)

type cronConfig struct {
	HeartbeatInterval time.Duration `json:"heartbeat_interval" default:"500ms"`
	ReconcileInterval time.Duration `json:"reconcile_interval" default:"2s"`
	JobTimeout        time.Duration `json:"job_timeout" default:"5s"`
}

type cronService struct {
	cfg   cronConfig
	redis *redis.Client // nil when the redis section is absent
	sched *cron.Scheduler
	stats *jobStats
}

func (s *cronService) Name() string { return "cron" }

func (s *cronService) Setup(ctx context.Context, env *verticle.Env) error {
	if err := env.DecodeService(&s.cfg); err != nil {
		return err
	}
	s.stats = newJobStats()
	s.redis = env.Redis
	s.sched = cron.New(cron.Config{
		// nil when redis is not configured: one replica, no election needed.
		Redis:   env.Redis,
		Timeout: s.cfg.JobTimeout,
	})
	s.sched.Add(s.jobs(s.cfg.HeartbeatInterval, s.cfg.ReconcileInterval)...)
	obs.Info(ctx, "cron jobs registered",
		"jobs", len(s.sched.Jobs()), "leader_election", env.Redis != nil)

	// verticle starts this block and cancels it on shutdown, before the
	// infrastructure it uses is closed.
	env.Block("cron", s.sched.Run)
	return nil
}

// jobs is the job set this service runs. It is a method taking its intervals
// so tests can build the same jobs on a short clock instead of duplicating
// them.
func (s *cronService) jobs(heartbeat, reconcile time.Duration) []cron.Job {
	return []cron.Job{
		{
			Name:     "heartbeat",
			Interval: heartbeat,
			// Run once at startup so the first counter bump is immediate.
			RunOnStart: true,
			Run: func(ctx context.Context) error {
				s.stats.record("heartbeat")
				obs.Info(ctx, "heartbeat")
				return nil
			},
		},
		{
			Name:     "reconcile",
			Interval: reconcile,
			Run: func(context.Context) error {
				// Every third run fails, to show that one bad run does not
				// stop the loop — the next tick still happens.
				if n := s.stats.record("reconcile"); n%3 == 0 {
					return errors.New("reconcile: the batch was rejected")
				}
				return nil
			},
		},
	}
}

func (s *cronService) BuildRouter(srv *httpx.Server) error {
	srv.Handle("GET", "/api/v1/jobs", s.listJobs)
	return nil
}

func (s *cronService) listJobs(_ context.Context, _ *http.Request) (any, error) {
	return s.stats.snapshot(), nil
}

// jobStats counts runs per job and remembers the last one, so the effect of
// the scheduler is observable from outside.
type jobStats struct {
	mu     sync.Mutex
	runs   map[string]int
	lastAt map[string]time.Time
}

func newJobStats() *jobStats {
	return &jobStats{runs: map[string]int{}, lastAt: map[string]time.Time{}}
}

// record bumps the counter and returns the new value.
func (s *jobStats) record(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs[name]++
	s.lastAt[name] = time.Now()
	return s.runs[name]
}

func (s *jobStats) count(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runs[name]
}

func (s *jobStats) snapshot() any {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make(map[string]any, len(s.runs))
	for name, n := range s.runs {
		out[name] = map[string]any{
			"runs": n,
			"last": s.lastAt[name].UTC().Format(time.RFC3339Nano),
		}
	}
	return map[string]any{"jobs": out}
}

func main() {
	conf := flag.String("conf", "examples/cron/config.yaml", "config file")
	flag.Parse()

	if err := verticle.Run(context.Background(), &cronService{}, verticle.WithConf(*conf)); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}
