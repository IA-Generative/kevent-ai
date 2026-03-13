package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"kevent/gateway/internal/model"
	"kevent/gateway/internal/service"
	"kevent/gateway/internal/storage"
)

// s3Store is the subset of storage.S3Client used by SyncHandler.
type s3Store interface {
	Upload(ctx context.Context, key string, body io.Reader, size int64, contentType string) error
	GetObject(ctx context.Context, key string) ([]byte, error)
	DeleteObject(ctx context.Context, key string) error
}

// jobStore is the subset of storage.RedisClient used by SyncHandler.
type jobStore interface {
	SaveJob(ctx context.Context, job *model.Job) error
	GetJob(ctx context.Context, id string) (*model.Job, error)
	DeleteJob(ctx context.Context, id string) error
	SubscribeJobDone(ctx context.Context, jobID string) storage.JobDoneSubscription
}

// eventProducer is the subset of kafka.Producer used by SyncHandler.
type eventProducer interface {
	PublishInputEvent(ctx context.Context, topic string, event *model.InputEvent) error
}

// SyncHandler handles OpenAI-compatible POST /v1/* requests.
//
// Routing strategy:
//   - multipart/form-data + service has sync_topic configured → sync-over-Kafka:
//     upload file to S3, publish to the priority sync topic, keep the connection
//     open and wait for the result, then return it directly in the HTTP response.
//   - application/json or no sync_topic configured → direct proxy to the inference
//     backend (original behaviour).
type SyncHandler struct {
	registry   *service.Registry
	s3         s3Store
	redis      jobStore
	producer   eventProducer
	httpClient *http.Client
}

func NewSyncHandler(
	registry *service.Registry,
	s3 s3Store,
	redis jobStore,
	producer eventProducer,
) *SyncHandler {
	return &SyncHandler{
		registry: registry,
		s3:       s3,
		redis:    redis,
		producer: producer,
		// Generous timeout for direct-proxy path; Knative timeoutSeconds is the
		// real ceiling for the sync-over-Kafka path (controlled by context).
		httpClient: &http.Client{Timeout: 15 * time.Minute},
	}
}

func (h *SyncHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))

	switch {
	case strings.HasPrefix(ct, "multipart/form-data"):
		h.handleMultipart(w, r)

	case ct == "application/json":
		h.handleJSON(w, r)

	default:
		writeError(w, http.StatusUnsupportedMediaType,
			"Content-Type must be multipart/form-data or application/json")
	}
}

func (h *SyncHandler) handleMultipart(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid multipart form: "+err.Error())
		return
	}

	// model is optional when the path pattern embeds the model name
	// (e.g. /v2/models/{model}/infer). RouteSync extracts it from the URL.
	modelName := r.FormValue("model")

	def, err := h.registry.RouteSync(r.URL.Path, modelName)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if def.SyncTopic != "" {
		h.handleMultipartViaKafka(w, r, def)
	} else {
		body, contentType, err := reconstructMultipart(r)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to rebuild request: "+err.Error())
			return
		}
		h.proxyToInference(w, r, def, body, contentType)
	}
}

func (h *SyncHandler) handleJSON(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read body: "+err.Error())
		return
	}

	var payload struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(raw, &payload)
	// model may be empty for path-pattern routes (e.g. /v2/models/{model}/infer).
	def, err := h.registry.RouteSync(r.URL.Path, payload.Model)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// JSON requests always use direct proxy (no file to route through Kafka).
	h.proxyToInference(w, r, def,
		io.NopCloser(bytes.NewReader(raw)),
		r.Header.Get("Content-Type"),
	)
}

// handleMultipartViaKafka uploads the file to S3, publishes to the priority sync
// topic, waits for the result, and returns it in the HTTP response — keeping the
// connection open throughout.
func (h *SyncHandler) handleMultipartViaKafka(w http.ResponseWriter, r *http.Request, def *service.Def) {
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

	jobID := uuid.New().String()
	ext := filepath.Ext(header.Filename)
	inputRef := fmt.Sprintf("%s/input%s", jobID, ext)

	if err := h.s3.Upload(r.Context(), inputRef, file, header.Size, header.Header.Get("Content-Type")); err != nil {
		slog.ErrorContext(r.Context(), "s3 upload failed", "job_id", jobID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to store file")
		return
	}

	now := time.Now().UTC()
	if err := h.redis.SaveJob(r.Context(), &model.Job{
		ID:          jobID,
		ServiceType: def.Type,
		Model:       def.Model,
		Status:      model.JobStatusPending,
		InputRef:    inputRef,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		slog.ErrorContext(r.Context(), "redis save failed", "job_id", jobID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to save job")
		return
	}

	// Subscribe BEFORE publishing to Kafka — ensures we never miss the notification
	// even if the dispatcher processes the job before we start waiting.
	sub := h.redis.SubscribeJobDone(r.Context(), jobID)
	defer sub.Close()

	event := &model.InputEvent{
		JobID:       jobID,
		ServiceType: def.Type,
		Model:       def.Model,
		InputRef:    inputRef,
		CreatedAt:   now,
	}
	if err := h.producer.PublishInputEvent(r.Context(), def.SyncTopic, event); err != nil {
		slog.ErrorContext(r.Context(), "kafka publish failed", "job_id", jobID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to enqueue job")
		return
	}

	slog.InfoContext(r.Context(), "sync job enqueued",
		"job_id", jobID, "model", def.Model, "topic", def.SyncTopic)

	// Block until the dispatcher publishes the result (or the client disconnects).
	if err := sub.Wait(r.Context()); err != nil {
		writeError(w, http.StatusGatewayTimeout, "timed out waiting for result")
		return
	}

	job, err := h.redis.GetJob(r.Context(), jobID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to retrieve job")
		return
	}

	if job.Status == model.JobStatusFailed {
		writeError(w, http.StatusUnprocessableEntity, job.Error)
		return
	}

	result, err := h.s3.GetObject(r.Context(), job.ResultRef)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to retrieve result")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(result)

	go func(resultRef, jobID string) {
		ctx := context.Background()
		if err := h.s3.DeleteObject(ctx, resultRef); err != nil {
			slog.Error("failed to delete sync result", "job_id", jobID, "error", err)
		}
		if err := h.redis.DeleteJob(ctx, jobID); err != nil {
			slog.Error("failed to delete sync job record", "job_id", jobID, "error", err)
		}
	}(job.ResultRef, jobID)
}

// proxyToInference forwards the request body directly to the inference backend.
func (h *SyncHandler) proxyToInference(w http.ResponseWriter, r *http.Request, def *service.Def, body io.ReadCloser, contentType string) {
	defer body.Close()

	target, err := url.Parse(def.InferenceURL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "invalid inference_url configuration")
		return
	}
	target.Path = r.URL.Path
	target.RawQuery = r.URL.RawQuery

	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target.String(), body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to build upstream request")
		return
	}
	upstreamReq.Header.Set("Content-Type", contentType)
	if auth := r.Header.Get("Authorization"); auth != "" {
		upstreamReq.Header.Set("Authorization", auth)
	}

	resp, err := h.httpClient.Do(upstreamReq)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream error: "+err.Error())
		return
	}
	defer resp.Body.Close()

	for key, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// reconstructMultipart rebuilds the multipart body from the already-parsed form,
// streaming file parts via an io.Pipe to avoid loading large files into memory.
func reconstructMultipart(r *http.Request) (io.ReadCloser, string, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	go func() {
		err := func() error {
			for key, values := range r.MultipartForm.Value {
				for _, value := range values {
					if err := mw.WriteField(key, value); err != nil {
						return err
					}
				}
			}
			for fieldName, fileHeaders := range r.MultipartForm.File {
				for _, fh := range fileHeaders {
					part, err := mw.CreateFormFile(fieldName, fh.Filename)
					if err != nil {
						return err
					}
					f, err := fh.Open()
					if err != nil {
						return err
					}
					_, err = io.Copy(part, f)
					f.Close()
					if err != nil {
						return err
					}
				}
			}
			return mw.Close()
		}()
		pw.CloseWithError(err)
	}()

	return pr, mw.FormDataContentType(), nil
}
