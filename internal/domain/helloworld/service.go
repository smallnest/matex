package helloworld

import (
	"context"
	"time"

	"github.com/smallnest/matex/pkg/core/db"
	"github.com/smallnest/matex/pkg/core/kafka"
	"github.com/smallnest/matex/pkg/core/memcache"
	"github.com/smallnest/matex/pkg/core/obs"
	"github.com/smallnest/matex/pkg/core/redis"
)

// TopicGreetings is the event topic the example publishes to.
const TopicGreetings = "matex.greetings"

// Deps injects the dependencies a Service needs. Nil means "not
// configured"; Service methods defensively no-op on nil.
type Deps struct {
	DB       *db.DB
	Redis    *redis.Client
	Memcache *memcache.Client
	Producer *kafka.Producer

	Greeting string // from service config
	Now      func() time.Time
}

// Service is the business layer: pure logic over Deps, no HTTP or
// framing concerns (those live in Handler).
type Service struct {
	deps Deps
	dao  *DAO
}

// New builds the Service (dao is wired when DB is present).
func New(d Deps) *Service {
	if d.Now == nil {
		d.Now = time.Now
	}
	s := &Service{deps: d}
	if d.DB != nil {
		s.dao = NewDAO(d.DB)
	}
	return s
}

// GreetResponse is the API payload.
type GreetResponse struct {
	Greeting string `json:"greeting"`
	Cached   bool   `json:"cached"`
	Count    int64  `json:"count"`
}

// Greet greets name. Demonstrates the cache-aside pattern: read cache,
// fall back to computing, write cache, record a db counter, publish an
// event — each piece no-ops when its dependency is nil.
func (s *Service) Greet(ctx context.Context, name string) (*GreetResponse, error) {
	greeting := s.deps.Greeting + name + "!"

	// cache-aside (redis path)
	cached := false
	if s.deps.Redis != nil {
		if v, ok, err := redis.GetJSON[string](ctx, s.deps.Redis, greetKey(name)); err == nil && ok {
			greeting, cached = v, true
		}
	}

	// record the visit (db, optional)
	var count int64
	if s.dao != nil {
		if c, err := s.dao.IncrementGreet(ctx, name); err == nil {
			count = c
		} else {
			obs.Warn(ctx, "greet counter failed", "name", name, "err", err)
		}
	}

	if !cached && s.deps.Redis != nil {
		_ = s.deps.Redis.SetJSON(ctx, greetKey(name), greeting, time.Minute)
	}

	// fire-and-forget event
	if s.deps.Producer != nil {
		s.deps.Producer.Send(ctx, TopicGreetings, []byte(name), []byte(greeting))
	}

	return &GreetResponse{Greeting: greeting, Cached: cached, Count: count}, nil
}

// Stats returns the visit counter (requires DB).
func (s *Service) Stats(ctx context.Context, name string) (*GreetStat, error) {
	if s.dao == nil {
		return nil, errNoDB
	}
	return s.dao.Stat(ctx, name)
}

// HandleGreetingEvent consumes the example topic (wired when the
// consumer is enabled in config).
func (s *Service) HandleGreetingEvent(ctx context.Context, m *kafka.Message) error {
	obs.Info(ctx, "greeting event",
		"topic", m.Topic, "partition", m.Partition, "offset", m.Offset,
		"key", string(m.Key), "value", string(m.Value))
	return nil
}

func greetKey(name string) string { return "matex:greet:" + name }
