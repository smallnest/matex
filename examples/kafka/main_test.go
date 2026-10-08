package main

import (
	"context"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kfake"

	"github.com/smallnest/matex/pkg/core/kafka"
)

// kfake is a broker that runs in-process, so Kafka behaviour can be tested
// with no Docker and no network. It is the one place an example is allowed
// to import a driver-level package (see .golangci.yml).
func TestProduceAndConsume(t *testing.T) {
	const (
		topic = "matex.test.events"
		group = "kafkademo-test"
	)

	cluster, err := kfake.NewCluster(
		kfake.NumBrokers(1),
		kfake.SeedTopics(1, topic),
	)
	if err != nil {
		t.Fatalf("kfake: %v", err)
	}
	t.Cleanup(cluster.Close)

	cfg := kafka.Config{Brokers: cluster.ListenAddrs(), ClientID: "kafkademo-test"}

	p, err := kafka.NewProducer(cfg)
	if err != nil {
		t.Fatalf("producer: %v", err)
	}
	t.Cleanup(p.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// Collect the message value.
	got := make(chan string, 1)
	go func() {
		_ = kafka.RunConsumer(ctx, cfg, group, []string{topic},
			func(_ context.Context, m *kafka.Message) error {
				select {
				case got <- string(m.Value):
				default:
				}
				return nil
			})
	}()

	if err := p.SendSync(ctx, topic, []byte("k"), []byte("v")); err != nil {
		t.Fatalf("produce: %v", err)
	}

	select {
	case v := <-got:
		if v != "v" {
			t.Fatalf("consumed %q, want %q", v, "v")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no record consumed within 20s")
	}
}
