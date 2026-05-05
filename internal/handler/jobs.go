package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"kevent/gateway/internal/metrics"
	"kevent/gateway/internal/model"
	"kevent/gateway/internal/ratelimit"
	"kevent/gateway/internal/service"
)

// asyncJobStore is the subset of storage.RedisClient used by JobHandler.
type asyncJobStore interface {
	SaveJob(ctx context.Context, job *model.Job) error
	GetJob(ctx context.Context, id string) (*model.Job, error)
	DeleteJob(ctx context.Context, id string) error
	UpdateJobResult(ctx context.Context, jobID string, status model.JobStatus, resultRef, errMsg string) error
	ListJobsByConsumer(ctx context.Context, consumer string, limit, offset int64) ([]*model.Job, int64, error)
}

// reservedJobFields are multipart form fields consumed by the gateway
// and excluded from the params map forwarded to the inference API.
var reservedJobFields = map[string]bool{
	"model": true, "file": true, "callback_url": true, "operation": true,
}

// JobHandler handles job submission and status queries.
type JobHandler struct {
	registry       *service.Registry
	store          s3Store       // reuses the interface defined in sync.go
	redis          asyncJobStore
	producer       eventProducer // reuses the interface defined in sync.go
	priorityHeader string        // HTTP header that triggers high-priority routing (e.g. "X-Priority")
	consumerHeader string        // HTTP header identifying the API consumer (e.g. "X-Consumer-Username")
	rateLimiter    ratelimit.Checker // nil = no rate limiting
}

func NewJobHandler(
	registry *service.Registry,
	store s3Store,
	redis asyncJobStore,
	producer eventProducer,
	priorityHeader string,
	consumerHeader string,
	rateLimiter ratelimit.Checker,
) *JobHandler {
	return &JobHandler{registry, store, redis, producer, priorityHeader, consumerHeader, rateLimiter}
}

// submitResponse is the 202 body returned after a successful job submission.
type submitResponse struct {
	JobID       string `json:"job_id"`
	ServiceType string `json:"service_type"`
	Model       string `json:"model"`
	Status      string `json:"status"`
}

// statusResponse is the body returned on GET /jobs/{service_type}/{id}.
// CallbackURL and ConsumerName are intentionally excluded from the response.
type statusResponse struct {
	JobID       string          `json:"job_id"`
	ServiceType string          `json:"service_type"`
	Model       string          `json:"model"`
	Status      model.JobStatus `json:"status"`
	Result      json.RawMessage `json:"result,omitempty"` // inline result payload when completed
	Error       string          `json:"error,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// listJobsResponse is the body returned on GET /jobs.
type listJobsResponse struct {
	Consumer string           `json:"consumer"`
	Total    int64            `json:"total"`
	Limit    int64            `json:"limit"`
	Offset   int64            `json:"offset"`
	Jobs     []*jobSummary    `json:"jobs"`
}

type jobSummary struct {
	JobID       string          `json:"job_id"`
	ServiceType string          `json:"service_type"`
	Model       string          `json:"model"`
	Status      model.JobStatus `json:"status"`
	Error       string          `json:"error,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// Submit handles POST /jobs/{service_type}.
//
// Form fields:
//
//	model        (optional if only one model for the type) – e.g. "whisper-large-v3"
//	file         (required) – the binary file to process
//	callback_url (optional) – webhook URL notified when the job completes
func (h *JobHandler) Submit(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	serviceType := chi.URLParam(r, "service_type")

	// Set the body size limit using the maximum across all models for this service
	// type, before ParseMultipartForm. The model field inside the form is not yet
	// readable at this point.
	maxSize, err := h.registry.MaxFileSizeForType(serviceType)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	if h.rateLimiter != nil {
		allowed, retryAfter, err := h.rateLimiter.Check(r.Context(), r, serviceType)
		if err != nil {
			slog.ErrorContext(r.Context(), "rate limit check failed", "error", err)
			// fail open — don't block requests on rate limiter errors
		} else if !allowed {
			w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxSize<<20)

	// Buffer up to 32 MB in memory; the rest spills to a temp file on disk.
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid multipart form: "+err.Error())
		return
	}

	// Resolve the specific model def now that the form is parsed.
	def, err := h.registry.RouteAsync(serviceType, r.FormValue("model"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Sync-direct services have no Kafka topics and cannot be used asynchronously.
	if def.InputTopic == "" {
		writeError(w, http.StatusMethodNotAllowed, fmt.Sprintf("service %q only supports sync requests (POST /v1/*)", def.Model))
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "field 'file' is required")
		return
	}
	defer file.Close()

	if err := h.registry.ValidateFileDef(def, header.Filename); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Validate the operation before touching S3 or Redis.
	operation := r.FormValue("operation")
	inferenceURL, err := def.OperationPath(operation)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	callbackURL := r.FormValue("callback_url")
	consumerName := ""
	if h.consumerHeader != "" {
		consumerName = r.Header.Get(h.consumerHeader)
	}

	// Collect extra form fields to forward to the inference API.
	// Reserved gateway fields are excluded.
	var params map[string]string
	for k, values := range r.MultipartForm.Value {
		if reservedJobFields[k] || len(values) == 0 {
			continue
		}
		if params == nil {
			params = make(map[string]string)
		}
		params[k] = values[0]
	}

	jobID := uuid.New().String()
	ext := filepath.Ext(header.Filename)
	inputRef := fmt.Sprintf("%s/input%s", jobID, ext) // e.g. "abc123/input.wav"

	// Step 1 — store the file in S3.
	if err := h.store.Upload(r.Context(), inputRef, file, header.Size, header.Header.Get("Content-Type")); err != nil {
		slog.ErrorContext(r.Context(), "s3 upload failed", "job_id", jobID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to store file")
		return
	}

	now := time.Now().UTC()
	job := &model.Job{
		ID:           jobID,
		ServiceType:  serviceType,
		Model:        def.Model,
		Status:       model.JobStatusPending,
		InputRef:     inputRef,
		CallbackURL:  callbackURL,
		ConsumerName: consumerName,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	// Step 2 — persist the job record in Redis.
	if err := h.redis.SaveJob(r.Context(), job); err != nil {
		slog.ErrorContext(r.Context(), "redis save failed", "job_id", jobID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to save job")
		return
	}

	// Step 3 — publish the input event to the model's Kafka topic.
	// When the priority header is present and the service has a priority_topic,
	// route to that topic so the relay processes it with elevated priority
	// (defers normal async jobs via the syncPriority flag, same as sync-over-Kafka).

	topic := def.InputTopic
	mode := "async"
	if h.priorityHeader != "" && r.Header.Get(h.priorityHeader) != "" && def.PriorityTopic != "" {
		topic = def.PriorityTopic
		mode = "async-priority"
	}

	event := &model.InputEvent{
		JobID:        jobID,
		ServiceType:  serviceType,
		Model:        def.Model,
		InputRef:     inputRef,
		InferenceURL: inferenceURL,
		Params:       params,
		CreatedAt:    now,
	}
	if err := h.producer.PublishInputEvent(r.Context(), topic, event); err != nil {
		slog.ErrorContext(r.Context(), "kafka publish failed", "job_id", jobID, "error", err)
		// Mark the job as failed so the client can react instead of polling forever.
		_ = h.redis.UpdateJobResult(r.Context(), jobID, model.JobStatusFailed, "", "failed to enqueue")
		// Clean up the orphaned S3 input file — it will never be consumed.
		go func() {
			if derr := h.store.DeleteObject(context.Background(), inputRef); derr != nil {
				slog.Error("failed to delete orphaned input after publish failure", "job_id", jobID, "error", derr)
			}
		}()
		writeError(w, http.StatusInternalServerError, "failed to enqueue job, please retry")
		return
	}

	slog.InfoContext(r.Context(), "job submitted",
		"job_id", jobID,
		"service_type", serviceType,
		"model", def.Model,
		"file", header.Filename,
		"mode", mode,
		"topic", topic,
	)

	metrics.RequestsTotal.WithLabelValues(mode, serviceType, def.Model, "202").Inc()
	metrics.RequestDuration.WithLabelValues(mode, serviceType, def.Model).Observe(time.Since(start).Seconds())
	if consumerName != "" {
		metrics.JobsByConsumerTotal.WithLabelValues(mode, serviceType, def.Model, consumerName).Inc()
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(submitResponse{
		JobID:       jobID,
		ServiceType: serviceType,
		Model:       def.Model,
		Status:      string(model.JobStatusPending),
	})
}

// GetStatus handles GET /jobs/{service_type}/{id}.
// When the job is complete the result is inlined in the response body.
//
// Consumer isolation: if consumer_header is configured and the request carries
// that header, the job's consumer_name must match — otherwise 404 is returned.
// This prevents a consumer from fetching another consumer's job even if they
// somehow obtained its UUID. Deployments without consumer_header skip this check.
func (h *JobHandler) GetStatus(w http.ResponseWriter, r *http.Request) {
	serviceType := chi.URLParam(r, "service_type")
	id := chi.URLParam(r, "id")

	job, err := h.redis.GetJob(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("job %q not found", id))
		return
	}

	// Validate that the job belongs to the requested service type.
	if job.ServiceType != serviceType {
		writeError(w, http.StatusNotFound, fmt.Sprintf("job %q not found", id))
		return
	}

	// Consumer ownership check (opt-in): only enforced when consumer_header is
	// configured AND the header is present in this request.
	// - Header absent → skip check (admin/internal calls, auth-less deployments).
	// - Header present + mismatch → 404 (don't reveal the job exists for another consumer).
	if h.consumerHeader != "" {
		if requester := r.Header.Get(h.consumerHeader); requester != "" {
			if job.ConsumerName != requester {
				writeError(w, http.StatusNotFound, fmt.Sprintf("job %q not found", id))
				return
			}
		}
	}

	resp := statusResponse{
		JobID:       job.ID,
		ServiceType: job.ServiceType,
		Model:       job.Model,
		Status:      job.Status,
		Error:       job.Error,
		CreatedAt:   job.CreatedAt,
		UpdatedAt:   job.UpdatedAt,
	}

	// Fetch and inline the result payload when the job is completed.
	if job.Status == model.JobStatusCompleted && job.ResultRef != "" {
		data, err := h.store.GetObject(r.Context(), job.ResultRef)
		if err != nil {
			slog.ErrorContext(r.Context(), "result fetch failed", "job_id", id, "error", err)
		} else {
			resp.Result = json.RawMessage(data)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(resp)

	// Clean up after delivering the result: delete the S3 file and Redis record.
	// Uses a background context — the request context is already done by the time
	// the goroutine runs.
	if job.Status == model.JobStatusCompleted && job.ResultRef != "" {
		go func(resultRef, jobID string) {
			ctx := context.Background()
			if err := h.store.DeleteObject(ctx, resultRef); err != nil {
				slog.Error("failed to delete result file", "job_id", jobID, "result_ref", resultRef, "error", err)
			}
			if err := h.redis.DeleteJob(ctx, jobID); err != nil {
				slog.Error("failed to delete job record", "job_id", jobID, "error", err)
			}
		}(job.ResultRef, job.ID)
	}
}

// ListJobs handles GET /jobs.
// Requires consumer_header to be configured. Returns the list of jobs for
// the consumer identified by that header, ordered most-recent-first.
//
// Query params:
//
//	limit  (default 20, max 100)
//	offset (default 0)
func (h *JobHandler) ListJobs(w http.ResponseWriter, r *http.Request) {
	if h.consumerHeader == "" {
		writeError(w, http.StatusNotImplemented, "GET /jobs requires consumer_header to be configured")
		return
	}

	consumer := r.Header.Get(h.consumerHeader)
	if consumer == "" {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("header %q is required", h.consumerHeader))
		return
	}

	limit := int64(20)
	offset := int64(0)
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			if n > 100 {
				n = 100
			}
			limit = n
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			offset = n
		}
	}

	jobs, total, err := h.redis.ListJobsByConsumer(r.Context(), consumer, limit, offset)
	if err != nil {
		slog.ErrorContext(r.Context(), "list jobs failed", "consumer", consumer, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list jobs")
		return
	}

	summaries := make([]*jobSummary, len(jobs))
	for i, j := range jobs {
		summaries[i] = &jobSummary{
			JobID:       j.ID,
			ServiceType: j.ServiceType,
			Model:       j.Model,
			Status:      j.Status,
			Error:       j.Error,
			CreatedAt:   j.CreatedAt,
			UpdatedAt:   j.UpdatedAt,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(listJobsResponse{
		Consumer: consumer,
		Total:    total,
		Limit:    limit,
		Offset:   offset,
		Jobs:     summaries,
	})
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(map[string]string{"error": msg})
}
