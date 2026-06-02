package consumer_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"kevent/gateway/internal/consumer"
)

func TestSubscriberNotifiesOnCompletion(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	notified := make(chan string, 1)
	sub := consumer.NewSubscriber(rdb, func(ctx context.Context, jobID string) {
		notified <- jobID
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sub.Subscribe(ctx, "whisper-diarization")
	time.Sleep(50 * time.Millisecond)

	rdb.Publish(context.Background(), "jobs:whisper-diarization:completed", "job-42")

	select {
	case id := <-notified:
		assert.Equal(t, "job-42", id)
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: job completion not received")
	}
}
