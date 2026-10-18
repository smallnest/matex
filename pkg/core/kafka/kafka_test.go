package kafka_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kfake"

	"github.com/smallnest/matex/pkg/core/kafka"
)

// kfake is an in-process broker, so the consumer loop and the producer
// are tested without Docker. packages_fake_file lets only this test file
// import it (see .golangci.yml), mirroring the examples exemption.

func newCluster(t *testing.T, topics ...string) *kfake.Cluster {
	t.Helper()
	c, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, topics...))
	if err != nil {
		t.Fatalf("kfake: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestNewProducerRequiresBrokers(t *testing.T) {
	if _, err := kafka.NewProducer(kafka.Config{}); err == nil {
		t.Fatal("NewProducer without brokers must fail")
	}
}

func TestSendSyncDelivers(t *testing.T) {
	const topic = "t.deliver"
	cl := newCluster(t, topic)

	p, err := kafka.NewProducer(kafka.Config{Brokers: cl.ListenAddrs()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)

	if err := p.SendSync(t.Context(), topic, []byte("k"), []byte("v")); err != nil {
		t.Fatalf("SendSync: %v", err)
	}
}

func TestSendJSONDeliversEncodedValue(t *testing.T) {
	const topic = "t.json"
	cl := newCluster(t, topic)

	p, err := kafka.NewProducer(kafka.Config{Brokers: cl.ListenAddrs()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	got := consumeOne(ctx, t, kafka.Config{Brokers: cl.ListenAddrs()}, "g-json", topic)

	if err := p.SendJSON(ctx, topic, "key-1", map[string]int{"n": 7}); err != nil {
		t.Fatalf("SendJSON: %v", err)
	}
	m := <-got
	if string(m.Key) != "key-1" || string(m.Value) != `{"n":7}` {
		t.Fatalf("got key=%q value=%q", m.Key, m.Value)
	}
	if m.Topic != topic || m.Time.IsZero() {
		t.Fatalf("message metadata not filled: %+v", m)
	}
}

func TestSendJSONRejectsUnencodableValue(t *testing.T) {
	cl := newCluster(t, "t.bad")
	p, err := kafka.NewProducer(kafka.Config{Brokers: cl.ListenAddrs()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)

	if err := p.SendJSON(t.Context(), "t.bad", "", make(chan int)); err == nil {
		t.Fatal("SendJSON must fail on a value json cannot encode")
	}
}

func TestRunConsumerRejectsMissingConfig(t *testing.T) {
	h := func(context.Context, *kafka.Message) error { return nil }
	if err := kafka.RunConsumer(t.Context(), kafka.Config{}, "g", []string{"t"}, h); err == nil {
		t.Fatal("RunConsumer without brokers must fail")
	}
	cfg := kafka.Config{Brokers: []string{"127.0.0.1:1"}}
	if err := kafka.RunConsumer(t.Context(), cfg, "", []string{"t"}, h); err == nil {
		t.Fatal("RunConsumer without a group must fail")
	}
}

func TestHandlerErrorDoesNotStopTheConsumer(t *testing.T) {
	const topic = "t.err"
	cl := newCluster(t, topic)
	cfg := kafka.Config{Brokers: cl.ListenAddrs()}

	done := make(chan []string, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	go func() {
		var got []string
		var once bool
		_ = kafka.RunConsumer(ctx, cfg, "g-err", []string{topic},
			func(context.Context, *kafka.Message) error {
				got = append(got, "seen")
				if !once {
					once = true
					return context.DeadlineExceeded
				}
				done <- got
				return nil
			})
	}()

	p, err := kafka.NewProducer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	for _, v := range []string{"a", "b"} {
		if err := p.SendSync(ctx, topic, nil, []byte(v)); err != nil {
			t.Fatalf("produce %s: %v", v, err)
		}
	}

	select {
	case got := <-done:
		if len(got) != 2 {
			t.Fatalf("handler saw %d messages, want 2", len(got))
		}
	case <-ctx.Done():
		t.Fatal("consumer stopped after a handler error (at-least-once log-and-continue)")
	}
}

func TestRunConsumerStopsOnCancel(t *testing.T) {
	const topic = "t.stop"
	cl := newCluster(t, topic)
	cfg := kafka.Config{Brokers: cl.ListenAddrs()}

	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() {
		errCh <- kafka.RunConsumer(ctx, cfg, "g-stop", []string{topic},
			func(context.Context, *kafka.Message) error { return nil })
	}()
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("RunConsumer after cancel: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunConsumer did not return after ctx cancellation")
	}
}

// consumeOne starts a consumer and hands back the first message it sees.
func consumeOne(ctx context.Context, t *testing.T, cfg kafka.Config, group, topic string) <-chan *kafka.Message {
	t.Helper()
	got := make(chan *kafka.Message, 1)
	go func() {
		_ = kafka.RunConsumer(ctx, cfg, group, []string{topic},
			func(_ context.Context, m *kafka.Message) error {
				select {
				case got <- m:
				default:
				}
				return nil
			})
	}()
	return got
}

func TestHandlerPanicDoesNotStopTheConsumer(t *testing.T) {
	const topic = "t.panic"
	cl := newCluster(t, topic)
	cfg := kafka.Config{Brokers: cl.ListenAddrs()}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	second := make(chan struct{}, 1)
	go func() {
		calls := 0
		_ = kafka.RunConsumer(ctx, cfg, "g-panic", []string{topic},
			func(context.Context, *kafka.Message) error {
				calls++
				if calls == 1 {
					panic("handler bug")
				}
				second <- struct{}{}
				return nil
			})
	}()

	p, err := kafka.NewProducer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	for _, v := range []string{"a", "b"} {
		if err := p.SendSync(ctx, topic, nil, []byte(v)); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-second:
	case <-ctx.Done():
		t.Fatal("consumer died after a handler panic; later messages were never handled")
	}
}

// TestRunConsumerDLQPacksFailedMessages: a handler error sends the record to
// the dead letter topic with its provenance, while good records still flow.
func TestRunConsumerDLQPacksFailedMessages(t *testing.T) {
	const topic, dlqTopic = "t.dlq", "t.dlq.dead"
	cl := newCluster(t, topic, dlqTopic)
	cfg := kafka.Config{Brokers: cl.ListenAddrs(), DLQTopic: dlqTopic}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	dlq, err := kafka.NewProducer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dlq.Close)

	seen := make(chan string, 1)
	go func() {
		var ok int
		_ = kafka.RunConsumerDLQ(ctx, cfg, "g-dlq", []string{topic},
			func(_ context.Context, m *kafka.Message) error {
				if string(m.Value) == "bad" {
					return errors.New("cannot parse")
				}
				ok++
				if ok == 2 {
					seen <- "both good"
				}
				return nil
			}, dlq)
	}()
	dead := consumeOne(ctx, t, cfg, "g-dlq-observer", dlqTopic)

	p, err := kafka.NewProducer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	for _, v := range []string{"bad", "good-1", "good-2"} {
		if err := p.SendSync(ctx, topic, []byte("k-"+v), []byte(v)); err != nil {
			t.Fatal(err)
		}
	}

	select {
	case m := <-dead:
		if string(m.Key) != "k-bad" {
			t.Fatalf("DLQ key = %q, want the source key", m.Key)
		}
		var rec kafka.DLQRecord
		if err := json.Unmarshal(m.Value, &rec); err != nil {
			t.Fatalf("DLQ value is not a DLQRecord: %v (%s)", err, m.Value)
		}
		if rec.SourceTopic != topic || string(rec.Value) != "bad" || rec.Error != "cannot parse" {
			t.Fatalf("DLQ record = %+v", rec)
		}
	case <-ctx.Done():
		t.Fatal("the failed record never reached the DLQ")
	}
	select {
	case <-seen:
	case <-ctx.Done():
		t.Fatal("the consumer stopped or the good records were lost")
	}
}

// TestRunConsumerDLQWithoutTopicBehavesLikeRunConsumer: an empty DLQTopic
// keeps the log-and-continue contract, so wiring DLQ in accidentally cannot
// change a service's behavior.
func TestRunConsumerDLQWithoutTopicBehavesLikeRunConsumer(t *testing.T) {
	const topic = "t.dlq.off"
	cl := newCluster(t, topic)
	cfg := kafka.Config{Brokers: cl.ListenAddrs()}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	second := make(chan struct{}, 1)
	go func() {
		calls := 0
		_ = kafka.RunConsumerDLQ(ctx, cfg, "g-dlq-off", []string{topic},
			func(context.Context, *kafka.Message) error {
				calls++
				if calls == 1 {
					return errors.New("first fails")
				}
				second <- struct{}{}
				return nil
			}, nil)
	}()

	p, err := kafka.NewProducer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	for _, v := range []string{"a", "b"} {
		if err := p.SendSync(ctx, topic, nil, []byte(v)); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-second:
	case <-ctx.Done():
		t.Fatal("a handler error stopped the consumer")
	}
}
