package kafka

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	kafkago "github.com/segmentio/kafka-go"

	"kevent/gateway/internal/config"
	"kevent/gateway/internal/model"
	"kevent/gateway/internal/service"
	"kevent/gateway/internal/storage"
)

const (
	webhookMaxRetries     = 3
	webhookInitialBackoff = 2 * time.Second
)

// ConsumerManager starts one consumer goroutine per registered service result topic.
// It updates job state in Redis and optionally sends webhook notifications.
// Use Reconcile to dynamically add/remove consumers at runtime without restart.
// Call Wait() after cancelling the context to drain all in-flight goroutines.
type ConsumerManager struct {
	cfg        config.KafkaConfig
	dialer     *kafkago.Dialer
	redis      *storage.RedisClient
	s3         *storage.S3Client
	logger     *slog.Logger
	httpClient *http.Client

	mu        sync.Mutex
	parentCtx context.Context
	cancels   map[string]context.CancelFunc // keyed by resultTopic
	wg        sync.WaitGroup               // tracks consumer goroutines + in-flight webhook goroutines
}

func NewConsumerManager(
	cfg config.KafkaConfig,
	redis *storage.RedisClient,
	s3 *storage.S3Client,
	logger *slog.Logger,
) (*ConsumerManager, error) {
	dialer, err := buildDialer(cfg)
	if err != nil {
		return nil, fmt.Errorf("building kafka dialer: %w", err)
	}
	return &ConsumerManager{
		cfg:    cfg,
		dialer: dialer,
		redis:  redis,
		s3:     s3,
		logger: logger,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		cancels: make(map[string]context.CancelFunc),
	}, nil
}

// Start launches one goroutine per service result topic in the initial registry.
// Services without a result topic (sync-direct only) are skipped.
// All goroutines stop when ctx is cancelled; call Wait to drain them.
func (cm *ConsumerManager) Start(ctx context.Context, reg *service.Registry) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.parentCtx = ctx
	for _, def := range reg.KafkaServices() {
		cm.startLocked(def.ResultTopic, def.Type)
	}
}

// Reconcile updates the set of active consumers to match newRegistry.
// Consumers for removed topics are stopped gracefully; consumers for added
// topics are started immediately. Unchanged topics continue uninterrupted.
func (cm *ConsumerManager) Reconcile(newReg *service.Registry) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	newServices := make(map[string]string) // resultTopic → serviceType
	for _, def := range newReg.KafkaServices() {
		newServices[def.ResultTopic] = def.Type
	}

	// Stop consumers for removed topics.
	for topic, cancel := range cm.cancels {
		if _, ok := newServices[topic]; !ok {
			cm.logger.Info("stopping result consumer", "topic", topic)
			cancel()
			delete(cm.cancels, topic)
		}
	}

	// Start consumers for added topics.
	for topic, serviceType := range newServices {
		if _, ok := cm.cancels[topic]; !ok {
			cm.logger.Info("starting result consumer", "topic", topic, "service", serviceType)
			cm.startLocked(topic, serviceType)
		}
	}
}

// startLocked starts a consumer goroutine for topic. Must be called with cm.mu held.
func (cm *ConsumerManager) startLocked(topic, serviceType string) {
	ctx, cancel := context.WithCancel(cm.parentCtx)
	cm.cancels[topic] = cancel
	cm.wg.Add(1)
	go func() {
		defer cm.wg.Done()
		cm.consume(ctx, topic, serviceType)
	}()
}

// Wait blocks until all consumer goroutines and in-flight webhook goroutines have exited.
// Call after cancelling the context passed to Start.
func (cm *ConsumerManager) Wait() {
	cm.wg.Wait()
}

func (cm *ConsumerManager) consume(ctx context.Context, topic, serviceType string) {
	// Each result topic gets its own consumer group to avoid the heterogeneous
	// subscription issue in kafka-go: two Readers sharing the same GroupID but
	// subscribing to different topics cause the coordinator to assign 0 partitions
	// to both members during rebalance.
	groupID := cm.cfg.ConsumerGroup + "-" + serviceType
	r := kafkago.NewReader(kafkago.ReaderConfig{
		Brokers:  cm.cfg.Brokers,
		Topic:    topic,
		GroupID:  groupID,
		Dialer:   cm.dialer,
		MinBytes: 1,
		MaxBytes: 10e6, // 10 MB
		// LastOffset: for a new consumer group, start from the tail.
		// Existing groups resume from their committed offset automatically.
		StartOffset: kafkago.LastOffset,
	})
	defer r.Close()

	cm.logger.Info("result consumer started", "topic", topic, "service", serviceType, "group", groupID)

	for {
		// FetchMessage + CommitMessages gives us at-least-once delivery semantics:
		// the offset is only committed after successful processing.
		msg, err := r.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				cm.logger.Info("result consumer stopped", "topic", topic)
				return
			}
			cm.logger.Error("fetching message", "topic", topic, "error", err)
			continue
		}

		if err := cm.handleResult(ctx, msg.Value); err != nil {
			cm.logger.Error("handling result event", "topic", topic, "offset", msg.Offset, "error", err)
			// Do not commit: the message will be reprocessed after restart.
			continue
		}

		if err := r.CommitMessages(ctx, msg); err != nil {
			cm.logger.Error("committing offset", "topic", topic, "offset", msg.Offset, "error", err)
		}
	}
}

func (cm *ConsumerManager) handleResult(ctx context.Context, raw []byte) error {
	var event model.ResultEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		return fmt.Errorf("unmarshaling result event: %w", err)
	}

	if err := cm.redis.UpdateJobResult(ctx, event.JobID, event.Status, event.ResultRef, event.Error); err != nil {
		return fmt.Errorf("updating job %q in redis: %w", event.JobID, err)
	}

	cm.logger.Info("job result stored", "job_id", event.JobID, "status", event.Status)

	// Retrieve the updated job BEFORE notifying sync waiters — this ensures the
	// job record still exists in Redis when the sync handler wakes up and reads it.
	job, err := cm.redis.GetJob(ctx, event.JobID)
	if err != nil {
		return fmt.Errorf("fetching job %q for webhook: %w", event.JobID, err)
	}

	// Wake up any sync handler waiting on this job (no-op for async jobs).
	cm.redis.NotifyJobDone(ctx, event.JobID)

	if job.CallbackURL != "" {
		cm.wg.Add(1)
		go func(j *model.Job) {
			defer cm.wg.Done()
			cm.sendWebhook(j)
		}(job)
	}

	return nil
}

// webhookPayload is the body sent to the client's callback URL.
type webhookPayload struct {
	JobID       string          `json:"job_id"`
	ServiceType string          `json:"service_type"`
	Status      model.JobStatus `json:"status"`
	Result      json.RawMessage `json:"result,omitempty"` // inline result payload when completed
	Error       string          `json:"error,omitempty"`
	CompletedAt time.Time       `json:"completed_at"`
}

// sendWebhook delivers the job result notification to the client's callback URL.
// It retries up to webhookMaxRetries times with exponential backoff.
func (cm *ConsumerManager) sendWebhook(job *model.Job) {
	p := webhookPayload{
		JobID:       job.ID,
		ServiceType: job.ServiceType,
		Status:      job.Status,
		Error:       job.Error,
		CompletedAt: job.UpdatedAt,
	}

	if job.Status == model.JobStatusCompleted && job.ResultRef != "" {
		data, err := cm.s3.GetObject(context.Background(), job.ResultRef)
		if err != nil {
			cm.logger.Error("webhook: failed to fetch result", "job_id", job.ID, "error", err)
		} else {
			p.Result = json.RawMessage(data)
		}
	}

	payload, _ := json.Marshal(p)

	backoff := webhookInitialBackoff

	for attempt := 1; attempt <= webhookMaxRetries; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, job.CallbackURL, bytes.NewReader(payload))
		if err != nil {
			cancel()
			cm.logger.Error("building webhook request", "job_id", job.ID, "error", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Job-ID", job.ID)

		resp, err := cm.httpClient.Do(req)
		cancel()

		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				cm.logger.Info("webhook delivered", "job_id", job.ID, "status_code", resp.StatusCode, "attempt", attempt)
				return
			}
			cm.logger.Warn("webhook server error", "job_id", job.ID, "status_code", resp.StatusCode, "attempt", attempt)
		} else {
			cm.logger.Warn("webhook request failed", "job_id", job.ID, "attempt", attempt, "error", err)
		}

		if attempt < webhookMaxRetries {
			time.Sleep(backoff)
			backoff *= 2
		}
	}

	cm.logger.Error("webhook failed after all retries", "job_id", job.ID, "url", job.CallbackURL)
}
