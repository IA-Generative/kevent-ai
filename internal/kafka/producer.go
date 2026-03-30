package kafka

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	kafkago "github.com/segmentio/kafka-go"

	"kevent/gateway/internal/config"
	"kevent/gateway/internal/metrics"
	"kevent/gateway/internal/model"
)

// Producer manages one kafka-go Writer per topic, created lazily on first use.
// Writers are thread-safe; the internal map is protected by a mutex.
type Producer struct {
	brokers   []string
	transport *kafkago.Transport
	mu        sync.Mutex
	writers   map[string]*kafkago.Writer
}

func NewProducer(cfg config.KafkaConfig) (*Producer, error) {
	transport, err := buildTransport(cfg)
	if err != nil {
		return nil, fmt.Errorf("building kafka transport: %w", err)
	}
	return &Producer{
		brokers:   cfg.Brokers,
		transport: transport,
		writers:   make(map[string]*kafkago.Writer),
	}, nil
}

// writerFor returns an existing writer or creates a new one for the topic.
func (p *Producer) writerFor(topic string) *kafkago.Writer {
	p.mu.Lock()
	defer p.mu.Unlock()

	if w, ok := p.writers[topic]; ok {
		return w
	}

	w := &kafkago.Writer{
		Addr:  kafkago.TCP(p.brokers...),
		Topic: topic,
		// Hash balancer ensures messages with the same job_id land on the same
		// partition, which preserves ordering per job if needed.
		Balancer:               &kafkago.Hash{},
		RequiredAcks:           kafkago.RequireOne,
		AllowAutoTopicCreation: false, // topics must be pre-created by the operator
	}
	if p.transport != nil {
		w.Transport = p.transport
	}
	p.writers[topic] = w
	return w
}

// PublishInputEvent serialises the event and writes it to the service's input topic.
// The job_id is used as the Kafka message key.
func (p *Producer) PublishInputEvent(ctx context.Context, topic string, event *model.InputEvent) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshaling input event: %w", err)
	}

	start := time.Now()
	err = p.writerFor(topic).WriteMessages(ctx, kafkago.Message{
		Key:   []byte(event.JobID),
		Value: payload,
	})
	metrics.KafkaPublishDuration.WithLabelValues(topic).Observe(time.Since(start).Seconds())
	if err != nil {
		metrics.KafkaPublishErrorsTotal.WithLabelValues(topic).Inc()
		return fmt.Errorf("writing to topic %q: %w", topic, err)
	}
	return nil
}

// Close flushes and closes all active writers.
func (p *Producer) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for topic, w := range p.writers {
		_ = w.Close()
		delete(p.writers, topic)
	}
}
