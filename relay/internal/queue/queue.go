// Package queue implements a Redis-backed BLMOVE job queue for the relay.
// Jobs are atomically moved from relay:{model}:pending to relay:{model}:processing
// on Pop, and removed from processing via Done after completion.
package queue

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrNoJob is returned by Pop when the queue stays empty for the full timeout.
// The pod was created by KEDA before the job was cancelled — caller should exit 0.
var ErrNoJob = errors.New("no job available")

// popTimeout is how long Pop waits before returning ErrNoJob.
// KEDA creates one pod per queue item; if nothing arrives within this window
// the item was cancelled before the pod started.
const popTimeout = 30 * time.Second

// Queue manages the relay-side Redis lists for one model.
type Queue struct {
	rdb     *redis.Client
	model   string
	pending string
	proc    string
}

// New creates a Queue for the given model.
func New(rdb *redis.Client, model string) *Queue {
	return &Queue{
		rdb:     rdb,
		model:   model,
		pending: "relay:" + model + ":pending",
		proc:    "relay:" + model + ":processing",
	}
}

// Pop waits up to popTimeout for a job ID in the pending list, then atomically
// moves it to the processing list using BLMOVE.
// Returns ErrNoJob if the queue stays empty for the full timeout (job was
// cancelled before the pod started). Returns context.Canceled on SIGTERM.
func (q *Queue) Pop(ctx context.Context) (string, error) {
	val, err := q.rdb.BLMove(ctx, q.pending, q.proc, "LEFT", "RIGHT", popTimeout).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return "", ErrNoJob
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("blmove %s → %s: %w", q.pending, q.proc, err)
	}
	return val, nil
}

// Done removes jobID from the processing list after successful or failed
// completion. Failures here are logged by the caller — they do not block
// job processing.
func (q *Queue) Done(ctx context.Context, jobID string) error {
	if err := q.rdb.LRem(ctx, q.proc, 1, jobID).Err(); err != nil {
		return fmt.Errorf("lrem %s %q: %w", q.proc, jobID, err)
	}
	return nil
}

// Publish notifies the gateway (and any subscriber) that jobID has completed.
// Channel: jobs:{model}:completed
func (q *Queue) Publish(ctx context.Context, jobID string) error {
	channel := "jobs:" + q.model + ":completed"
	if err := q.rdb.Publish(ctx, channel, jobID).Err(); err != nil {
		return fmt.Errorf("publish %s %q: %w", channel, jobID, err)
	}
	return nil
}

// Ping checks the Redis connection. Returns an error if unreachable.
func (q *Queue) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return q.rdb.Ping(ctx).Err()
}
