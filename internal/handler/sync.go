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

	"kevent/gateway/internal/metrics"
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
	start := time.Now()
	metrics.SyncJobsInFlight.Inc()
	defer func() {
		metrics.SyncJobsInFlight.Dec()
		metrics.RequestDuration.WithLabelValues("sync", def.Type, def.Model).Observe(time.Since(start).Seconds())
	}()

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

	// Collect extra form fields to forward to the inference API.
	reserved := map[string]bool{"model": true, "file": true}
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
	// even if the relay processes the job before we start waiting.
	sub := h.redis.SubscribeJobDone(r.Context(), jobID)
	defer sub.Close()

	event := &model.InputEvent{
		JobID:        jobID,
		ServiceType:  def.Type,
		Model:        def.Model,
		InputRef:     inputRef,
		InferenceURL: r.URL.Path, // exact path the client called (e.g. /v1/audio/transcriptions)
		Params:       params,
		CreatedAt:    now,
	}
	if err := h.producer.PublishInputEvent(r.Context(), def.SyncTopic, event); err != nil {
		slog.ErrorContext(r.Context(), "kafka publish failed", "job_id", jobID, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to enqueue job")
		return
	}

	slog.InfoContext(r.Context(), "sync job enqueued",
		"job_id", jobID, "model", def.Model, "topic", def.SyncTopic)

	// committed tracks whether we have already sent the 200 + headers to the
	// client. Once committed, WriteHeader calls are no-ops in HTTP/1.1, so errors
	// can only be reported in the body. For fast inferences (< 20 s) we stay
	// uncommitted and return proper status codes. For longer ones, the first
	// keepalive tick commits the stream so proxies (APISix/nginx) don't drop the
	// idle connection ("upstream prematurely closed connection while reading
	// response header from upstream").
	flusher, canFlush := w.(http.Flusher)
	committed := false
	commit := func() {
		if !committed {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			committed = true
		}
	}

	// Wait for the relay to publish the result.
	// Send a JSON-whitespace newline keepalive every 20 s to prevent proxy
	// idle-connection timeouts during long inferences (OCR, large audio).
	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()
	waitDone := make(chan error, 1)
	waitStart := time.Now()
	go func() { waitDone <- sub.Wait(r.Context()) }()

waitLoop:
	for {
		select {
		case err := <-waitDone:
			metrics.SyncWaitDuration.WithLabelValues(def.Type, def.Model).Observe(time.Since(waitStart).Seconds())
			if err != nil {
				metrics.RequestsTotal.WithLabelValues("sync", def.Type, def.Model, "504").Inc()
				writeError(w, http.StatusGatewayTimeout, "timed out waiting for result")
				return
			}
			break waitLoop
		case <-keepalive.C:
			commit()
			_, _ = w.Write([]byte("\n"))
			if canFlush {
				flusher.Flush()
			}
		}
	}

	job, err := h.redis.GetJob(r.Context(), jobID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to retrieve job")
		return
	}

	// Cleanup lancé en goroutine quel que soit le chemin de sortie (succès,
	// échec métier, erreur S3). Sans defer, les early-returns laissaient
	// result.json et l'entrée Redis orphelins.
	defer func() {
		go func() {
			ctx := context.Background()
			if job.ResultRef != "" {
				if err := h.s3.DeleteObject(ctx, job.ResultRef); err != nil {
					slog.Error("failed to delete sync result", "job_id", jobID, "error", err)
				}
			}
			if err := h.redis.DeleteJob(ctx, jobID); err != nil {
				slog.Error("failed to delete sync job record", "job_id", jobID, "error", err)
			}
		}()
	}()

	if job.Status == model.JobStatusFailed {
		metrics.RequestsTotal.WithLabelValues("sync", def.Type, def.Model, "422").Inc()
		writeError(w, http.StatusUnprocessableEntity, job.Error)
		return
	}

	result, err := h.s3.GetObject(r.Context(), job.ResultRef)
	if err != nil {
		metrics.RequestsTotal.WithLabelValues("sync", def.Type, def.Model, "500").Inc()
		writeError(w, http.StatusInternalServerError, "failed to retrieve result")
		return
	}

	metrics.RequestsTotal.WithLabelValues("sync", def.Type, def.Model, "200").Inc()
	commit()
	_, _ = w.Write(result)
}

// proxyToInference forwards the request body directly to the inference backend.
func (h *SyncHandler) proxyToInference(w http.ResponseWriter, r *http.Request, def *service.Def, body io.ReadCloser, contentType string) {
	start := time.Now()
	defer func() {
		metrics.RequestDuration.WithLabelValues("sync-direct", def.Type, def.Model).Observe(time.Since(start).Seconds())
	}()
	defer body.Close()

	target, err := url.Parse(def.InferenceURL)
	if err != nil {
		metrics.RequestsTotal.WithLabelValues("sync-direct", def.Type, def.Model, "500").Inc()
		writeError(w, http.StatusInternalServerError, "invalid inference_url configuration")
		return
	}
	target.Path = r.URL.Path
	target.RawQuery = r.URL.RawQuery

	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target.String(), body)
	if err != nil {
		metrics.RequestsTotal.WithLabelValues("sync-direct", def.Type, def.Model, "500").Inc()
		writeError(w, http.StatusInternalServerError, "failed to build upstream request")
		return
	}
	upstreamReq.Header.Set("Content-Type", contentType)
	if auth := r.Header.Get("Authorization"); auth != "" {
		upstreamReq.Header.Set("Authorization", auth)
	}

	resp, err := h.httpClient.Do(upstreamReq)
	if err != nil {
		metrics.RequestsTotal.WithLabelValues("sync-direct", def.Type, def.Model, "502").Inc()
		writeError(w, http.StatusBadGateway, "upstream error: "+err.Error())
		return
	}
	defer resp.Body.Close()

	metrics.RequestsTotal.WithLabelValues("sync-direct", def.Type, def.Model, fmt.Sprintf("%d", resp.StatusCode)).Inc()
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
