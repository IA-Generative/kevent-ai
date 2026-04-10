package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"kevent/gateway/internal/config"
	"kevent/gateway/internal/metrics"
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
	start := time.Now()
	data, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("marshaling job %q: %w", job.ID, err)
	}
	err = r.client.Set(ctx, jobKey(job.ID), data, r.jobTTL).Err()
	metrics.RedisOperationDuration.WithLabelValues("save_job").Observe(time.Since(start).Seconds())
	if err != nil {
		metrics.RedisErrorsTotal.WithLabelValues("save_job").Inc()
		return fmt.Errorf("saving job %q: %w", job.ID, err)
	}
	return nil
}

// GetJob retrieves a job from Redis. Returns a descriptive error when not found.
func (r *RedisClient) GetJob(ctx context.Context, id string) (*model.Job, error) {
	start := time.Now()
	data, err := r.client.Get(ctx, jobKey(id)).Bytes()
	metrics.RedisOperationDuration.WithLabelValues("get_job").Observe(time.Since(start).Seconds())
	if err == redis.Nil {
		return nil, fmt.Errorf("job %q not found", id)
	}
	if err != nil {
		metrics.RedisErrorsTotal.WithLabelValues("get_job").Inc()
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
	start := time.Now()
	err := r.client.Del(ctx, jobKey(id)).Err()
	metrics.RedisOperationDuration.WithLabelValues("delete_job").Observe(time.Since(start).Seconds())
	if err != nil {
		metrics.RedisErrorsTotal.WithLabelValues("delete_job").Inc()
		return fmt.Errorf("deleting job %q: %w", id, err)
	}
	return nil
}

// updateJobScript atomically reads a job JSON, patches status/result_ref/error/updated_at,
// and re-writes it with the same TTL — avoiding the read-modify-write race of GetJob+SaveJob.
var updateJobScript = redis.NewScript(`
local data = redis.call('GET', KEYS[1])
if not data then
    return redis.error_reply('job not found: ' .. KEYS[1])
end
local job = cjson.decode(data)
job['status']     = ARGV[1]
job['result_ref'] = ARGV[2]
job['error']      = ARGV[3]
job['updated_at'] = ARGV[4]
redis.call('SET', KEYS[1], cjson.encode(job), 'EX', tonumber(ARGV[5]))
return redis.status_reply('OK')
`)

// UpdateJobResult atomically updates a job's status, result reference, and error
// message after the inference worker publishes its result event.
// Uses a Lua script so the read-modify-write is executed without a race window.
func (r *RedisClient) UpdateJobResult(ctx context.Context, jobID string, status model.JobStatus, resultRef, errMsg string) error {
	updatedAt := time.Now().UTC().Format(time.RFC3339Nano)
	ttlSecs := int64(r.jobTTL.Seconds())
	if ttlSecs <= 0 {
		ttlSecs = 3600
	}
	start := time.Now()
	err := updateJobScript.Run(ctx, r.client,
		[]string{jobKey(jobID)},
		string(status), resultRef, errMsg, updatedAt, ttlSecs,
	).Err()
	metrics.RedisOperationDuration.WithLabelValues("update_job").Observe(time.Since(start).Seconds())
	if err != nil {
		metrics.RedisErrorsTotal.WithLabelValues("update_job").Inc()
		return fmt.Errorf("updating job %q in redis: %w", jobID, err)
	}
	return nil
}

// JobDoneSubscription is the interface returned by SubscribeJobDone.
// Callers block on Wait until the job completes, then call Close.
type JobDoneSubscription interface {
	Wait(ctx context.Context) error
	Close()
}

// jobDoneSub is the live Redis pub/sub implementation of JobDoneSubscription.
type jobDoneSub struct {
	ch     <-chan *redis.Message
	pubsub *redis.PubSub
}

func (s *jobDoneSub) Wait(ctx context.Context) error {
	select {
	case <-s.ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *jobDoneSub) Close() { _ = s.pubsub.Close() }

// SubscribeJobDone creates a subscription for the given job's completion channel.
// Must be called BEFORE publishing the job to Kafka to avoid missing the notification.
func (r *RedisClient) SubscribeJobDone(ctx context.Context, jobID string) JobDoneSubscription {
	ps := r.client.Subscribe(ctx, "job:"+jobID+":done")
	return &jobDoneSub{ch: ps.Channel(), pubsub: ps}
}

// NotifyJobDone publishes a signal on the job's completion channel to wake up
// any sync handler waiting via SubscribeJobDone.
func (r *RedisClient) NotifyJobDone(ctx context.Context, jobID string) {
	if err := r.client.Publish(ctx, "job:"+jobID+":done", "1").Err(); err != nil {
		slog.ErrorContext(ctx, "failed to notify job done", "job_id", jobID, "error", err)
	}
}
