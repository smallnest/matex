// Package kafka wraps franz-go with a producer (sync/async) and an
// at-least-once consumer loop. Business code uses this package instead
// of importing franz-go directly (depguard enforces the choke point).
package kafka

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime/debug"
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
	// DLQTopic is where RunConsumerDLQ parks messages the handler could not
	// process. Empty disables the dead letter queue.
	DLQTopic string `json:"dlq_topic" optional:""`
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
//
// RunConsumerDLQ intercepts the error instead: the message goes to the dead
// letter queue before the batch is committed.
type Handler func(ctx context.Context, m *Message) error

// handle runs the handler, converting a panic into an error: a bug in one
// message must not take the consumer (and every group member) down.
func handle(ctx context.Context, h Handler, m *Message) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v\n%s", p, debug.Stack())
		}
	}()
	return h(ctx, m)
}

// RunConsumer consumes group/topics until ctx is cancelled. It commits
// synchronously after each poll batch (commits survive shutdown by
// using a detached context).
//
// A handler error is logged and the record is still committed: a poison
// message must not wedge its partition. Use RunConsumerDLQ to keep such
// messages instead of dropping them.
func RunConsumer(ctx context.Context, cfg Config, group string, topics []string, h Handler) error {
	return consume(ctx, cfg, group, topics, h, nil)
}

// DLQRecord is what RunConsumerDLQ writes to the dead letter topic: the
// untouched message plus enough provenance to replay it.
type DLQRecord struct {
	SourceTopic     string    `json:"source_topic"`
	SourcePartition int32     `json:"source_partition"`
	SourceOffset    int64     `json:"source_offset"`
	SourceTime      time.Time `json:"source_time"`
	Key             []byte    `json:"key,omitempty"`
	Value           []byte    `json:"value"`
	Error           string    `json:"error"`
}

// RunConsumerDLQ is RunConsumer with a dead letter queue: when the handler
// fails, the message is published to cfg.DLQTopic through dlq — with its
// key, value and source coordinates — and only then is the batch
// committed. An empty DLQTopic degrades to RunConsumer's log-and-continue;
// dlq is then ignored and may be nil.
//
// If publishing to the DLQ itself fails, the message is logged and the
// batch is still committed: blocking the partition forever on a broken DLQ
// is worse than losing one record that was already logged.
func RunConsumerDLQ(ctx context.Context, cfg Config, group string, topics []string, h Handler, dlq *Producer) error {
	var deadLetter func(context.Context, *Message, error)
	if cfg.DLQTopic != "" && dlq != nil {
		deadLetter = func(c context.Context, m *Message, err error) {
			deadLetterMessage(c, dlq, cfg.DLQTopic, m, err)
		}
	}
	return consume(ctx, cfg, group, topics, h, deadLetter)
}

// deadLetterMessage publishes m to topic. It keeps the original key, so a
// DLQ consumer preserving the partition order sees the same sequence.
func deadLetterMessage(ctx context.Context, dlq *Producer, topic string, m *Message, cause error) {
	rec := DLQRecord{
		SourceTopic:     m.Topic,
		SourcePartition: m.Partition,
		SourceOffset:    m.Offset,
		SourceTime:      m.Time,
		Key:             m.Key,
		Value:           m.Value,
		Error:           cause.Error(),
	}
	b, err := json.Marshal(rec)
	if err != nil {
		obs.Error(ctx, "kafka: marshal dead letter", "topic", m.Topic, "offset", m.Offset, "err", err)
		return
	}
	if err := dlq.SendSync(ctx, topic, m.Key, b); err != nil {
		obs.Error(ctx, "kafka: dead letter publish failed; record is dropped",
			"topic", m.Topic, "offset", m.Offset, "dlq", topic, "err", err)
		return
	}
	obs.Warn(ctx, "kafka: message moved to the dead letter queue",
		"dlq", topic, "topic", m.Topic, "partition", m.Partition, "offset", m.Offset)
}

// consume is the shared loop. deadLetter, when set, is called with every
// handler error before the batch is committed.
func consume(ctx context.Context, cfg Config, group string, topics []string, h Handler, deadLetter func(context.Context, *Message, error)) error {
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
			if err := handle(ctx, h, m); err != nil {
				obs.Error(ctx, "kafka: handle failed",
					"topic", r.Topic, "partition", r.Partition, "offset", r.Offset, "err", err)
				if deadLetter != nil {
					deadLetter(ctx, m, err)
				}
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
