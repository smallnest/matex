// Package kafka wraps franz-go with a producer (sync/async) and an
// at-least-once consumer loop. Business code uses this package instead
// of importing franz-go directly (depguard enforces the choke point).
package kafka

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/smallnest/matex/pkg/core/obs"
)

// Config configures clients. Leave Brokers empty to disable kafka.
// Brokers accepts a comma-separated string (from env expansion) or a
// YAML list.
type Config struct {
	Brokers  []string `json:"brokers" optional:""`
	ClientID string   `json:"client_id" default:"matex"`
}

// Producer sends records. Create one per service and reuse it.
type Producer struct {
	cl *kgo.Client
}

// NewProducer creates a producer with durable defaults (acks=all).
func NewProducer(cfg Config) (*Producer, error) {
	if len(cfg.Brokers) == 0 {
		return nil, fmt.Errorf("kafka: brokers are required")
	}
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(cfg.Brokers...),
		kgo.ClientID(cfg.ClientID),
		kgo.RequiredAcks(kgo.AllISRAcks()),
	)
	if err != nil {
		return nil, fmt.Errorf("kafka: new producer: %w", err)
	}
	return &Producer{cl: cl}, nil
}

// Close flushes buffered records and disconnects.
func (p *Producer) Close() { p.cl.Close() }

// SendSync blocks until the record is acknowledged.
func (p *Producer) SendSync(ctx context.Context, topic string, key, value []byte) error {
	err := p.cl.ProduceSync(ctx, &kgo.Record{Topic: topic, Key: key, Value: value}).FirstErr()
	if err != nil {
		return fmt.Errorf("kafka: produce %s: %w", topic, err)
	}
	return nil
}

// Send is fire-and-forget: failures are logged, never surfaced to the
// caller. Use SendSync when loss is unacceptable.
func (p *Producer) Send(ctx context.Context, topic string, key, value []byte) {
	p.cl.Produce(ctx, &kgo.Record{Topic: topic, Key: key, Value: value},
		func(_ *kgo.Record, err error) {
			if err != nil {
				obs.Error(ctx, "kafka: produce failed", "topic", topic, "err", err)
			}
		})
}

// SendJSON marshals v and sends it synchronously.
func (p *Producer) SendJSON(ctx context.Context, topic string, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("kafka: marshal %s: %w", topic, err)
	}
	return p.SendSync(ctx, topic, []byte(key), b)
}

// Message is the normalized record handed to consumers.
type Message struct {
	Topic     string
	Partition int32
	Offset    int64
	Key       []byte
	Value     []byte
	Time      time.Time
}

// Handler processes one message. Return an error to log-and-continue:
// records are committed after the handler returns regardless (at-least
// -once); retry semantics belong to the handler.
type Handler func(ctx context.Context, m *Message) error

// RunConsumer consumes group/topics until ctx is cancelled. It commits
// synchronously after each poll batch (commits survive shutdown by
// using a detached context).
func RunConsumer(ctx context.Context, cfg Config, group string, topics []string, h Handler) error {
	if len(cfg.Brokers) == 0 {
		return fmt.Errorf("kafka: brokers are required")
	}
	if group == "" {
		return fmt.Errorf("kafka: consumer group is required")
	}
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(cfg.Brokers...),
		kgo.ClientID(cfg.ClientID),
		kgo.ConsumerGroup(group),
		kgo.ConsumeTopics(topics...),
		kgo.DisableAutoCommit(),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
	)
	if err != nil {
		return fmt.Errorf("kafka: new consumer: %w", err)
	}
	defer cl.Close()

	const maxPoll = 100
	for ctx.Err() == nil {
		fetch := cl.PollRecords(ctx, maxPoll)
		records := fetch.Records()
		for _, e := range fetch.Errors() {
			obs.Warn(ctx, "kafka: fetch error", "topic", e.Topic, "err", e.Err)
		}
		for _, r := range records {
			m := &Message{
				Topic:     r.Topic,
				Partition: r.Partition,
				Offset:    r.Offset,
				Key:       r.Key,
				Value:     r.Value,
				Time:      r.Timestamp,
			}
			if err := h(ctx, m); err != nil {
				obs.Error(ctx, "kafka: handle failed",
					"topic", r.Topic, "partition", r.Partition, "offset", r.Offset, "err", err)
			}
		}
		if len(records) > 0 {
			// Commit even while shutting down: otherwise the batch is
			// redelivered to the next member.
			commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			err := cl.CommitRecords(commitCtx, records...)
			cancel()
			if err != nil {
				obs.Error(ctx, "kafka: commit failed", "err", err)
			}
		}
	}
	return nil
}
