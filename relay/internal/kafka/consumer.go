package kafka

import (
	"context"
	"time"

	kafkago "github.com/segmentio/kafka-go"

	"kevent/relay/internal/config"
)

// Consumer wraps a kafka-go Reader for manual-commit message consumption.
// Each pod holds one Consumer; Kafka's consumer group protocol distributes
// partitions across pods automatically on scale-up/down.
type Consumer struct {
	reader *kafkago.Reader
}

// NewConsumer creates a Consumer for the topic and group in cfg.
func NewConsumer(cfg config.KafkaConfig) (*Consumer, error) {
	dialer, err := buildDialer(cfg)
	if err != nil {
		return nil, err
	}
	r := kafkago.NewReader(kafkago.ReaderConfig{
		Brokers:  cfg.Brokers,
		GroupID:  cfg.ConsumerGroup,
		Topic:    cfg.InputTopic,
		Dialer:   dialer,
		MinBytes: 1,
		MaxBytes: 10 << 20, // 10 MB
		MaxWait:  5 * time.Second,
	})
	return &Consumer{reader: r}, nil
}

// FetchMessage blocks until a message is available or ctx is cancelled.
// The message is NOT committed until CommitMessages is called.
func (c *Consumer) FetchMessage(ctx context.Context) (kafkago.Message, error) {
	return c.reader.FetchMessage(ctx)
}

// CommitMessages marks msg as processed in the consumer group.
// Call only after successful processing.
func (c *Consumer) CommitMessages(ctx context.Context, msg kafkago.Message) error {
	return c.reader.CommitMessages(ctx, msg)
}

// Close closes the underlying reader and leaves the consumer group.
func (c *Consumer) Close() error {
	return c.reader.Close()
}
