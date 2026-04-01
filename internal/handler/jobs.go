package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"kevent/gateway/internal/kafka"
	"kevent/gateway/internal/metrics"
	"kevent/gateway/internal/model"
	"kevent/gateway/internal/service"
	"kevent/gateway/internal/storage"
)



// JobHandler handles job submission and status queries.
type JobHandler struct {
	registry *service.Registry
	store    *storage.S3Client
	redis    *storage.RedisClient
	producer *kafka.Producer
}

func NewJobHandler(
	registry *service.Registry,
	store *storage.S3Client,
	redis *storage.RedisClient,
	producer *kafka.Producer,
) *JobHandler {
	return &JobHandler{registry, store, redis, producer}
}

// submitResponse is the 202 body returned after a successful job submission.
type submitResponse struct {
	JobID       string `json:"job_id"`
	ServiceType string `json:"service_type"`
	Model       string `json:"model"`
	Status      string `json:"status"`
}

// statusResponse is the body returned on GET /jobs/{service_type}/{id}.
// CallbackURL is intentionally excluded from the response.
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

	callbackURL := r.FormValue("callback_url")

	// Collect extra form fields to forward to the inference API.
	// Reserved gateway fields are excluded.
	reserved := map[string]bool{"model": true, "file": true, "callback_url": true, "operation": true}
	var params map[string]string
	for k, values := range r.MultipartForm.Value {
		if reserved[k] || len(values) == 0 {
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
		ID:          jobID,
		ServiceType: serviceType,
		Model:       def.Model,
		Status:      model.JobStatusPending,
		InputRef:    inputRef,
		CallbackURL: callbackURL,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	// Step 2 — persist the job record in Redis.
	if err := h.redis.SaveJob(r.Context(), job); err != nil {
		slog.ErrorContext(r.Context(), "redis save failed", "job_id", jobID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to save job")
		return
	}

	// Step 3 — publish the input event to the model's Kafka topic.
	operation := r.FormValue("operation")
	inferenceURL, err := def.OperationPath(operation)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
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
	if err := h.producer.PublishInputEvent(r.Context(), def.InputTopic, event); err != nil {
		slog.ErrorContext(r.Context(), "kafka publish failed", "job_id", jobID, "error", err)
		// Mark the job as failed so the client can react instead of polling forever.
		_ = h.redis.UpdateJobResult(r.Context(), jobID, model.JobStatusFailed, "", "failed to enqueue")
		writeError(w, http.StatusInternalServerError, "failed to enqueue job, please retry")
		return
	}

	slog.InfoContext(r.Context(), "job submitted",
		"job_id", jobID,
		"service_type", serviceType,
		"model", def.Model,
		"file", header.Filename,
	)

	metrics.RequestsTotal.WithLabelValues("async", serviceType, def.Model, "202").Inc()
	metrics.RequestDuration.WithLabelValues("async", serviceType, def.Model).Observe(time.Since(start).Seconds())

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

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(map[string]string{"error": msg})
}
