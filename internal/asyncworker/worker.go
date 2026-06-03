// Package asyncworker provides a per-model worker pool that processes async
// inference jobs. Each worker pops one job at a time from the Redis queue,
// calls the inference backend, and updates the job record in Redis.
package asyncworker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"kevent/gateway/internal/consumer"
	"kevent/gateway/internal/model"
	"kevent/gateway/internal/service"
	"kevent/gateway/internal/storage"
)

// redisStore is the subset of storage.RedisClient used by the worker.
type redisStore interface {
	PopJob(ctx context.Context, modelName string) (string, error)
	DoneJob(ctx context.Context, jobID, modelName string) error
	GetJob(ctx context.Context, id string) (*model.Job, error)
	UpdateJobResult(ctx context.Context, jobID string, status model.JobStatus, resultRef, errMsg string) error
	NotifyJobDone(ctx context.Context, jobID string)
}

// s3Store is the subset of storage.S3Client used by the worker.
type s3Store interface {
	GetObject(ctx context.Context, key string) ([]byte, error)
	Upload(ctx context.Context, key string, r interface{ Read([]byte) (int, error) }, size int64, contentType string) error
	DeleteObject(ctx context.Context, key string) error
}

// Pool manages async worker goroutines for a single model.
type Pool struct {
	def           *service.Def
	redis         redisStore
	s3            *storage.S3Client
	webhookSender *consumer.WebhookSender
	persistResult bool
	httpClient    *http.Client
	wg            sync.WaitGroup
}

// Manager holds all per-model worker pools and coordinates startup/shutdown.
type Manager struct {
	pools []*Pool
}

// New creates a Manager that starts worker goroutines for every async-enabled
// model in the registry.
func New(
	reg *service.Registry,
	redis *storage.RedisClient,
	s3 *storage.S3Client,
	persistResult bool,
) *Manager {
	webhookSender := consumer.NewWebhookSender(redis, s3, persistResult)
	m := &Manager{}
	for _, def := range reg.All() {
		if !def.SupportsAsync || def.AsyncWorkers <= 0 {
			continue
		}
		p := &Pool{
			def:           def,
			redis:         redis,
			s3:            s3,
			webhookSender: webhookSender,
			persistResult: persistResult,
			httpClient:    &http.Client{Timeout: 30 * time.Minute},
		}
		m.pools = append(m.pools, p)
	}
	return m
}

// Start launches all worker goroutines. They run until ctx is cancelled.
func (m *Manager) Start(ctx context.Context) {
	for _, p := range m.pools {
		p.start(ctx)
	}
}

// UpdatePersistsResult updates the S3 result retention policy at runtime.
func (m *Manager) UpdatePersistsResult(v bool) {
	for _, p := range m.pools {
		p.persistResult = v
		p.webhookSender.UpdatePersistsResult(v)
	}
}

// Wait blocks until all in-flight jobs have completed (used during graceful shutdown).
func (m *Manager) Wait() {
	for _, p := range m.pools {
		p.wg.Wait()
	}
}

func (p *Pool) start(ctx context.Context) {
	for i := 0; i < p.def.AsyncWorkers; i++ {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.loop(ctx)
		}()
	}
	slog.Info("async workers started", "model", p.def.Model, "workers", p.def.AsyncWorkers)
}

func (p *Pool) loop(ctx context.Context) {
	for {
		jobID, err := p.redis.PopJob(ctx, p.def.Model)
		if errors.Is(err, context.Canceled) {
			return
		}
		if err != nil {
			slog.Error("async worker: pop error", "model", p.def.Model, "error", err)
			// brief backoff before retrying to avoid tight error loops
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}

		p.process(ctx, jobID)
	}
}

func (p *Pool) process(ctx context.Context, jobID string) {
	job, err := p.redis.GetJob(ctx, jobID)
	if err != nil {
		slog.Error("async worker: get job failed", "job_id", jobID, "error", err)
		_ = p.redis.DoneJob(context.Background(), jobID, p.def.Model)
		return
	}

	if job.Status == model.JobStatusCancelled {
		slog.Info("async worker: job already cancelled, skipping", "job_id", jobID)
		_ = p.redis.DoneJob(context.Background(), jobID, p.def.Model)
		return
	}

	slog.Info("async worker: processing job", "job_id", jobID, "model", job.Model)

	resultRef, inferErr := p.runJob(ctx, job)
	if inferErr != nil {
		slog.Error("async worker: job failed", "job_id", jobID, "error", inferErr)
		_ = p.redis.UpdateJobResult(context.Background(), jobID, model.JobStatusFailed, "", fmt.Sprintf("inference: %v", inferErr))
		p.redis.NotifyJobDone(context.Background(), jobID)
		if j, err := p.redis.GetJob(context.Background(), jobID); err == nil && j.CallbackURL != "" {
			go p.webhookSender.Send(j)
		}
		_ = p.redis.DoneJob(context.Background(), jobID, p.def.Model)
		return
	}

	_ = p.redis.UpdateJobResult(context.Background(), jobID, model.JobStatusCompleted, resultRef, "")
	p.redis.NotifyJobDone(context.Background(), jobID)
	if j, err := p.redis.GetJob(context.Background(), jobID); err == nil && j.CallbackURL != "" {
		go p.webhookSender.Send(j)
	}
	_ = p.redis.DoneJob(context.Background(), jobID, p.def.Model)

	if !p.persistResult {
		if err := p.s3.DeleteObject(context.Background(), job.InputRef); err != nil {
			slog.Warn("async worker: failed to delete input", "job_id", jobID, "error", err)
		}
	}

	slog.Info("async worker: job completed", "job_id", jobID, "result_ref", resultRef)
}

func (p *Pool) runJob(ctx context.Context, job *model.Job) (string, error) {
	data, err := p.s3.GetObject(ctx, job.InputRef)
	if err != nil {
		return "", fmt.Errorf("s3 get: %w", err)
	}

	// Resolve inference URL from job or fall back to service def.
	inferURL := job.InferenceURL
	if inferURL == "" {
		if len(p.def.Backends) > 0 {
			inferURL = p.def.Backends[0].URL
		} else {
			inferURL = p.def.InferenceURL
		}
	} else {
		// job.InferenceURL is a path; prepend the backend base URL.
		base := p.def.InferenceURL
		if len(p.def.Backends) > 0 {
			base = p.def.Backends[0].URL
		}
		inferURL = base + inferURL
	}

	result, err := callInference(ctx, p.httpClient, callInput{
		url:         inferURL,
		filename:    filepath.Base(job.InputRef),
		body:        data,
		contentType: "application/octet-stream",
		model:       job.Model,
		params:      job.Params,
		extraFields: p.def.InferenceExtraFields,
	})
	if err != nil {
		return "", err
	}

	resultKey := job.ID + "/result.json"
	if err := p.s3.Upload(ctx, resultKey, bytes.NewReader(result), int64(len(result)), "application/json"); err != nil {
		return "", fmt.Errorf("s3 upload result: %w", err)
	}
	return resultKey, nil
}
