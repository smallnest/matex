// Command kafka demonstrates pkg/core/kafka: a producer (sync + async) and
// an at-least-once consumer loop, both driven by the verticle lifecycle.
//
// Kafka must be running (make dev). The producer is built by
// verticle.Run from the `kafka:` section and handed over as env.Kafka; the
// consumer is registered as a background block in Setup, so it starts with
// the app and stops on shutdown (committing the last batch).
//
// Run:
//
//	make dev
//	go run ./examples/kafka
//
//	curl -i -X POST localhost:8080/api/v1/events       -d '{"key":"u1","value":"hello"}'
//	curl -i -X POST localhost:8080/api/v1/events/async -d '{"key":"u2","value":"fire-and-forget"}'
//	curl -i localhost:8080/api/v1/stats
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sync/atomic"

	"github.com/smallnest/matex/pkg/core/errs"
	"github.com/smallnest/matex/pkg/core/httpx"
	"github.com/smallnest/matex/pkg/core/kafka"
	"github.com/smallnest/matex/pkg/core/obs"
	"github.com/smallnest/matex/pkg/core/verticle"
)

type kafkaConfig struct {
	Topic   string `json:"topic" default:"matex.example.events"`
	Group   string `json:"group" default:"kafkademo-consumers"`
	Consume bool   `json:"consume" default:"true"`
}

type kafkaService struct {
	cfg kafkaConfig
	p   *kafka.Producer

	produced atomic.Int64
	consumed atomic.Int64
}

func (s *kafkaService) Name() string { return "kafkademo" }

func (s *kafkaService) Setup(ctx context.Context, env *verticle.Env) error {
	if err := env.DecodeService(&s.cfg); err != nil {
		return err
	}
	if env.Kafka == nil {
		return errors.New("kafkademo: the `kafka:` section is required (start Kafka first: make dev)")
	}
	s.p = env.Kafka

	if s.cfg.Consume {
		// The same brokers/client id the producer used.
		cfg, topic, group := env.KafkaCfg, s.cfg.Topic, s.cfg.Group
		env.Block("consumer", func(ctx context.Context) error {
			obs.Info(ctx, "consumer started", "topic", topic, "group", group)
			return kafka.RunConsumer(ctx, cfg, group, []string{topic}, s.handle)
		})
	}
	return nil
}

func (s *kafkaService) handle(ctx context.Context, m *kafka.Message) error {
	s.consumed.Add(1)
	// Returning an error here only logs it; the batch is committed anyway
	// (at-least-once). Retry/backoff belongs in the handler.
	obs.Info(ctx, "consumed",
		"topic", m.Topic, "partition", m.Partition, "offset", m.Offset,
		"key", string(m.Key), "value", string(m.Value))
	return nil
}

type event struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// publishSync blocks until the broker acknowledges; use it when losing the
// event is unacceptable.
func (s *kafkaService) publishSync(ctx context.Context, r *http.Request) (any, error) {
	e, err := decodeEvent(r)
	if err != nil {
		return nil, err
	}
	if err := s.p.SendSync(ctx, s.cfg.Topic, []byte(e.Key), []byte(e.Value)); err != nil {
		return nil, err // kafka wrapper already wrapped it
	}
	s.produced.Add(1)
	return map[string]any{"topic": s.cfg.Topic, "key": e.Key, "mode": "sync"}, nil
}

// publishAsync returns immediately; failures are only logged.
func (s *kafkaService) publishAsync(ctx context.Context, r *http.Request) (any, error) {
	e, err := decodeEvent(r)
	if err != nil {
		return nil, err
	}
	s.p.Send(ctx, s.cfg.Topic, []byte(e.Key), []byte(e.Value))
	s.produced.Add(1)
	return map[string]any{"topic": s.cfg.Topic, "key": e.Key, "mode": "async"}, nil
}

func (s *kafkaService) stats(context.Context, *http.Request) (any, error) {
	return map[string]any{
		"topic":    s.cfg.Topic,
		"group":    s.cfg.Group,
		"produced": s.produced.Load(),
		"consumed": s.consumed.Load(),
	}, nil
}

func (s *kafkaService) BuildRouter(srv *httpx.Server) error {
	srv.Handle("POST", "/api/v1/events", s.publishSync)
	srv.Handle("POST", "/api/v1/events/async", s.publishAsync)
	srv.Handle("GET", "/api/v1/stats", s.stats)
	return nil
}

func decodeEvent(r *http.Request) (event, error) {
	var e event
	if err := httpx.ReadJSON(r, &e); err != nil {
		return e, errs.Invalid(40001, "bad request body: %v", err)
	}
	if e.Value == "" {
		return e, errs.Invalid(40002, "value is required")
	}
	return e, nil
}

func main() {
	conf := flag.String("conf", "examples/kafka/config.yaml", "config file")
	flag.Parse()

	if err := verticle.Run(context.Background(), &kafkaService{}, verticle.WithConf(*conf)); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}
