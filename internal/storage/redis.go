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

// Raw exposes the underlying go-redis client for subsystems that need
// generic Redis access (e.g. the LLM response cache).
func (r *RedisClient) Raw() *redis.Client { return r.client }

func jobKey(id string) string        { return "job:" + id }
func consumerKey(name string) string { return "consumer:" + name + ":jobs" }

// SaveJob persists the full job struct as a JSON blob with the configured TTL.
// If the job has a ConsumerName, the job ID is also added to the consumer's
// sorted set (score = Unix timestamp) so it can be listed via ListJobsByConsumer.
func (r *RedisClient) SaveJob(ctx context.Context, job *model.Job) error {
	start := time.Now()
	data, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("marshaling job %q: %w", job.ID, err)
	}

	var pipeErr error
	if job.ConsumerName != "" {
		_, pipeErr = r.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.Set(ctx, jobKey(job.ID), data, r.jobTTL)
			pipe.ZAdd(ctx, consumerKey(job.ConsumerName), redis.Z{
				Score:  float64(job.CreatedAt.Unix()),
				Member: job.ID,
			})
			pipe.Expire(ctx, consumerKey(job.ConsumerName), r.jobTTL)
			return nil
		})
	} else {
		pipeErr = r.client.Set(ctx, jobKey(job.ID), data, r.jobTTL).Err()
	}

	metrics.RedisOperationDuration.WithLabelValues("save_job").Observe(time.Since(start).Seconds())
	if pipeErr != nil {
		metrics.RedisErrorsTotal.WithLabelValues("save_job").Inc()
		return fmt.Errorf("saving job %q: %w", job.ID, pipeErr)
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

// deleteJobScript atomically removes a job and cleans up the consumer index.
// It reads the job JSON, extracts consumer_name (if any), removes the job ID
// from the consumer sorted set, then deletes the job key — all in one step.
var deleteJobScript = redis.NewScript(`
local data = redis.call('GET', KEYS[1])
if data then
    local ok, job = pcall(cjson.decode, data)
    if ok and job['consumer_name'] and job['consumer_name'] ~= '' then
        redis.call('ZREM', 'consumer:' .. job['consumer_name'] .. ':jobs', ARGV[1])
    end
end
return redis.call('DEL', KEYS[1])
`)

// DeleteJob removes a job record from Redis and cleans up the consumer index.
func (r *RedisClient) DeleteJob(ctx context.Context, id string) error {
	start := time.Now()
	err := deleteJobScript.Run(ctx, r.client, []string{jobKey(id)}, id).Err()
	metrics.RedisOperationDuration.WithLabelValues("delete_job").Observe(time.Since(start).Seconds())
	if err != nil {
		metrics.RedisErrorsTotal.WithLabelValues("delete_job").Inc()
		return fmt.Errorf("deleting job %q: %w", id, err)
	}
	return nil
}

// ListJobsByConsumer returns up to limit jobs for the given consumer, ordered
// by most-recent-first (ZREVRANGE on the score = creation Unix timestamp).
// total is the full count before pagination.
func (r *RedisClient) ListJobsByConsumer(ctx context.Context, consumer string, limit, offset int64) ([]*model.Job, int64, error) {
	key := consumerKey(consumer)

	// Pipeline: ZCARD + ZREVRANGE in one round-trip.
	var total int64
	var ids []string
	_, err := r.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		cardCmd := pipe.ZCard(ctx, key)
		rangeCmd := pipe.ZRevRange(ctx, key, offset, offset+limit-1)
		_ = cardCmd
		_ = rangeCmd
		return nil
	})
	if err != nil && err != redis.Nil {
		return nil, 0, fmt.Errorf("listing jobs for consumer %q: %w", consumer, err)
	}
	// Re-run individually to get typed results (TxPipelined results are in cmds).
	pipe := r.client.Pipeline()
	cardCmd := pipe.ZCard(ctx, key)
	rangeCmd := pipe.ZRevRange(ctx, key, offset, offset+limit-1)
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, 0, fmt.Errorf("listing jobs for consumer %q: %w", consumer, err)
	}
	total = cardCmd.Val()
	ids = rangeCmd.Val()

	if len(ids) == 0 {
		return nil, total, nil
	}

	// Batch-fetch all job records.
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = jobKey(id)
	}
	vals, err := r.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, 0, fmt.Errorf("fetching jobs for consumer %q: %w", consumer, err)
	}

	jobs := make([]*model.Job, 0, len(vals))
	for i, v := range vals {
		if v == nil {
			// Job TTL expired but consumer index not yet cleaned — skip stale entry.
			continue
		}
		var job model.Job
		if err := json.Unmarshal([]byte(v.(string)), &job); err != nil {
			slog.Warn("skipping malformed job record", "id", ids[i], "error", err)
			continue
		}
		jobs = append(jobs, &job)
	}
	return jobs, total, nil
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
