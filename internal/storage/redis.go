package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"kevent/gateway/internal/config"
	"kevent/gateway/internal/model"
)

// RedisClient wraps go-redis with job-specific persistence helpers.
type RedisClient struct {
	client *redis.Client
	jobTTL time.Duration
}

func NewRedis(cfg config.RedisConfig) (*RedisClient, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("connecting to redis at %q: %w", cfg.Addr, err)
	}

	return &RedisClient{
		client: rdb,
		jobTTL: time.Duration(cfg.JobTTLH) * time.Hour,
	}, nil
}

func (r *RedisClient) Close() error {
	return r.client.Close()
}

func jobKey(id string) string { return "job:" + id }

// SaveJob persists the full job struct as a JSON blob with the configured TTL.
func (r *RedisClient) SaveJob(ctx context.Context, job *model.Job) error {
	data, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("marshaling job %q: %w", job.ID, err)
	}
	if err := r.client.Set(ctx, jobKey(job.ID), data, r.jobTTL).Err(); err != nil {
		return fmt.Errorf("saving job %q: %w", job.ID, err)
	}
	return nil
}

// GetJob retrieves a job from Redis. Returns a descriptive error when not found.
func (r *RedisClient) GetJob(ctx context.Context, id string) (*model.Job, error) {
	data, err := r.client.Get(ctx, jobKey(id)).Bytes()
	if err == redis.Nil {
		return nil, fmt.Errorf("job %q not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("getting job %q: %w", id, err)
	}

	var job model.Job
	if err := json.Unmarshal(data, &job); err != nil {
		return nil, fmt.Errorf("unmarshaling job %q: %w", id, err)
	}
	return &job, nil
}

// DeleteJob removes a job record from Redis.
func (r *RedisClient) DeleteJob(ctx context.Context, id string) error {
	if err := r.client.Del(ctx, jobKey(id)).Err(); err != nil {
		return fmt.Errorf("deleting job %q: %w", id, err)
	}
	return nil
}

// UpdateJobResult updates a job's status, result reference, and error message
// after the inference worker publishes its result event.
func (r *RedisClient) UpdateJobResult(ctx context.Context, jobID string, status model.JobStatus, resultRef, errMsg string) error {
	job, err := r.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	job.Status = status
	job.ResultRef = resultRef
	job.Error = errMsg
	job.UpdatedAt = time.Now().UTC()
	return r.SaveJob(ctx, job)
}

// JobDoneSub is a single-use subscription that unblocks when a job completes.
// Create it with SubscribeJobDone BEFORE publishing the job to Kafka, then
// call Wait once the job has been enqueued.
type JobDoneSub struct {
	ch     <-chan *redis.Message
	pubsub *redis.PubSub
}

// Wait blocks until the job-done notification arrives or ctx is cancelled.
func (s *JobDoneSub) Wait(ctx context.Context) error {
	select {
	case <-s.ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close releases the underlying pub/sub connection.
func (s *JobDoneSub) Close() { _ = s.pubsub.Close() }

// SubscribeJobDone creates a subscription for the given job's completion channel.
// Must be called BEFORE publishing the job to Kafka to avoid missing the notification.
func (r *RedisClient) SubscribeJobDone(ctx context.Context, jobID string) *JobDoneSub {
	ps := r.client.Subscribe(ctx, "job:"+jobID+":done")
	return &JobDoneSub{ch: ps.Channel(), pubsub: ps}
}

// NotifyJobDone publishes a signal on the job's completion channel to wake up
// any sync handler waiting via SubscribeJobDone.
func (r *RedisClient) NotifyJobDone(ctx context.Context, jobID string) {
	_ = r.client.Publish(ctx, "job:"+jobID+":done", "1").Err()
}
