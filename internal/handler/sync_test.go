package handler_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kevent/gateway/internal/config"
	"kevent/gateway/internal/handler"
	"kevent/gateway/internal/model"
	"kevent/gateway/internal/service"
	"kevent/gateway/internal/storage"
)

// ── Mocks ────────────────────────────────────────────────────────────────────

type mockS3 struct {
	uploadErr error
	getResult []byte
	getErr    error
	uploaded  bool
}

func (m *mockS3) Upload(_ context.Context, _ string, _ io.Reader, _ int64, _ string) error {
	m.uploaded = true
	return m.uploadErr
}
func (m *mockS3) GetObject(_ context.Context, _ string) ([]byte, error) {
	return m.getResult, m.getErr
}
func (m *mockS3) DeleteObject(_ context.Context, _ string) error { return nil }

type mockSub struct{ ch chan struct{} }

func (s *mockSub) Wait(ctx context.Context) error {
	select {
	case <-s.ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *mockSub) Close() {}

type mockRedis struct {
	saveErr error
	job     *model.Job
	getErr  error
	sub     *mockSub
	notified bool
}

func (m *mockRedis) SaveJob(_ context.Context, _ *model.Job) error { return m.saveErr }
func (m *mockRedis) GetJob(_ context.Context, _ string) (*model.Job, error) {
	return m.job, m.getErr
}
func (m *mockRedis) DeleteJob(_ context.Context, _ string) error { return nil }
func (m *mockRedis) SubscribeJobDone(_ context.Context, _ string) storage.JobDoneSubscription {
	return m.sub
}

type mockProducer struct {
	publishErr error
	published  bool
	// When set, signals the mock subscription on publish so Wait unblocks.
	sub *mockSub
}

func (m *mockProducer) PublishInputEvent(_ context.Context, _ string, _ *model.InputEvent) error {
	m.published = true
	if m.sub != nil {
		// Signal immediately so the handler's Wait unblocks in tests.
		select {
		case m.sub.ch <- struct{}{}:
		default:
		}
	}
	return m.publishErr
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func buildRegistry(syncTopic string) *service.Registry {
	cfgs := []config.ServiceConfig{
		{
			Type:  "transcription",
			Model: "whisper-large-v3",
			Operations: map[string][]string{
				"transcription": {"/v1/audio/transcriptions"},
			},
			InferenceURL:  "http://inference.example.com",
			InputTopic:    "jobs.whisper-large-v3.input",
			ResultTopic:   "jobs.whisper-large-v3.results",
			SyncTopic:     syncTopic,
			AcceptedExts:  []string{".mp3", ".wav"},
			MaxFileSizeMB: 100,
		},
	}
	return service.NewRegistry(cfgs)
}

func multipartRequest(t *testing.T, path, modelName string, fileContent []byte) *http.Request {
	t.Helper()
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	_ = mw.WriteField("model", modelName)
	fw, _ := mw.CreateFormFile("file", "audio.wav")
	_, _ = fw.Write(fileContent)
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, path, body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestSyncHandler_JSONAlwaysUsesDirectProxy verifies that JSON requests always go
// through the direct proxy, even when sync_topic is configured.
func TestSyncHandler_JSONAlwaysUsesDirectProxy(t *testing.T) {
	upstreamCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()

	cfgs := []config.ServiceConfig{{
		Type:  "ocr",
		Model: "llava",
		Operations: map[string][]string{
			"chat": {"/v1/chat/completions"},
		},
		InferenceURL: upstream.URL,
		SyncTopic:    "jobs.llava.sync", // set, but JSON → direct proxy
		InputTopic:   "jobs.llava.input",
		ResultTopic:  "jobs.llava.results",
	}}
	reg := service.NewRegistry(cfgs)

	h := handler.NewSyncHandler(reg, &mockS3{}, &mockRedis{}, &mockProducer{}, "", nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"llava","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if !upstreamCalled {
		t.Error("JSON request should have been proxied to upstream, but upstream was not called")
	}
}

// TestSyncHandler_MultipartNoSyncTopic_UsesDirectProxy verifies that a multipart
// request for a service without sync_topic is proxied directly.
func TestSyncHandler_MultipartNoSyncTopic_UsesDirectProxy(t *testing.T) {
	upstreamCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		_, _ = w.Write([]byte(`{"text":"hello"}`))
	}))
	defer upstream.Close()

	cfgs := []config.ServiceConfig{{
		Type:  "transcription",
		Model: "whisper-large-v3",
		Operations: map[string][]string{
			"transcription": {"/v1/audio/transcriptions"},
		},
		InferenceURL: upstream.URL,
		SyncTopic:    "", // empty → direct proxy
		InputTopic:   "jobs.whisper-large-v3.input",
		ResultTopic:  "jobs.whisper-large-v3.results",
		AcceptedExts: []string{".mp3", ".wav"},
	}}
	reg := service.NewRegistry(cfgs)

	h := handler.NewSyncHandler(reg, &mockS3{}, &mockRedis{}, &mockProducer{}, "", nil, nil)
	req := multipartRequest(t, "/v1/audio/transcriptions", "whisper-large-v3", []byte("fake audio"))
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if !upstreamCalled {
		t.Error("multipart with no sync_topic should proxy to upstream, but upstream was not called")
	}
}

// TestSyncHandler_MultipartWithSyncTopic_UsesKafka verifies that a multipart
// request for a service with sync_topic goes through S3 + Kafka, not direct proxy.
func TestSyncHandler_MultipartWithSyncTopic_UsesKafka(t *testing.T) {
	upstreamCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
	}))
	defer upstream.Close()

	sub := &mockSub{ch: make(chan struct{}, 1)}
	s3 := &mockS3{getResult: []byte(`{"text":"transcribed"}`)}
	redis := &mockRedis{
		sub: sub,
		job: &model.Job{
			ID:        "test-job",
			Status:    model.JobStatusCompleted,
			ResultRef: "test-job/result.json",
		},
	}
	prod := &mockProducer{sub: sub} // signals sub.ch on publish

	reg := buildRegistry("jobs.whisper-large-v3.sync")
	h := handler.NewSyncHandler(reg, s3, redis, prod, "", nil, nil)

	req := multipartRequest(t, "/v1/audio/transcriptions", "whisper-large-v3", []byte("fake audio"))
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if upstreamCalled {
		t.Error("multipart with sync_topic should NOT proxy directly to upstream")
	}
	if !s3.uploaded {
		t.Error("file should have been uploaded to S3")
	}
	if !prod.published {
		t.Error("InputEvent should have been published to Kafka sync topic")
	}
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "transcribed") {
		t.Errorf("expected result in response body, got: %s", w.Body.String())
	}
}

// TestSyncHandler_S3UploadFailure verifies that an S3 upload error returns 500.
func TestSyncHandler_S3UploadFailure(t *testing.T) {
	sub := &mockSub{ch: make(chan struct{}, 1)}
	s3 := &mockS3{uploadErr: fmt.Errorf("s3 unavailable")}
	redis := &mockRedis{sub: sub}
	prod := &mockProducer{}

	reg := buildRegistry("jobs.whisper-large-v3.sync")
	h := handler.NewSyncHandler(reg, s3, redis, prod, "", nil, nil)

	req := multipartRequest(t, "/v1/audio/transcriptions", "whisper-large-v3", []byte("audio"))
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on S3 failure, got %d", w.Code)
	}
	if prod.published {
		t.Error("Kafka publish should not happen after S3 upload failure")
	}
}

// TestSyncHandler_KafkaPublishFailure verifies that a Kafka publish error returns 500.
func TestSyncHandler_KafkaPublishFailure(t *testing.T) {
	sub := &mockSub{ch: make(chan struct{}, 1)}
	s3 := &mockS3{}
	redis := &mockRedis{sub: sub}
	prod := &mockProducer{publishErr: fmt.Errorf("kafka unavailable")}

	reg := buildRegistry("jobs.whisper-large-v3.sync")
	h := handler.NewSyncHandler(reg, s3, redis, prod, "", nil, nil)

	req := multipartRequest(t, "/v1/audio/transcriptions", "whisper-large-v3", []byte("audio"))
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on Kafka failure, got %d", w.Code)
	}
}

// TestSyncHandler_ClientDisconnect verifies that a context cancellation (client
// disconnect) while waiting for the result returns an appropriate error.
func TestSyncHandler_ClientDisconnect(t *testing.T) {
	sub := &mockSub{ch: make(chan struct{})} // channel never signalled
	s3 := &mockS3{}
	redis := &mockRedis{sub: sub}
	prod := &mockProducer{} // does not signal sub

	reg := buildRegistry("jobs.whisper-large-v3.sync")
	h := handler.NewSyncHandler(reg, s3, redis, prod, "", nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	req := multipartRequest(t, "/v1/audio/transcriptions", "whisper-large-v3", []byte("audio")).
		WithContext(ctx)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusGatewayTimeout {
		t.Errorf("expected 504 on client timeout, got %d", w.Code)
	}
}

// TestSyncHandler_InferenceFailure verifies that a failed job (status=failed) returns 422.
func TestSyncHandler_InferenceFailure(t *testing.T) {
	sub := &mockSub{ch: make(chan struct{}, 1)}
	s3 := &mockS3{}
	redis := &mockRedis{
		sub: sub,
		job: &model.Job{
			ID:     "test-job",
			Status: model.JobStatusFailed,
			Error:  "inference error: GPU OOM",
		},
	}
	prod := &mockProducer{sub: sub}

	reg := buildRegistry("jobs.whisper-large-v3.sync")
	h := handler.NewSyncHandler(reg, s3, redis, prod, "", nil, nil)

	req := multipartRequest(t, "/v1/audio/transcriptions", "whisper-large-v3", []byte("audio"))
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 for failed job, got %d", w.Code)
	}
}

// TestSyncHandler_MissingModelField_SingleModel verifies that when only one model
// is registered for a path, omitting the "model" field auto-selects that model.
func TestSyncHandler_MissingModelField_SingleModel(t *testing.T) {
	sub := &mockSub{ch: make(chan struct{}, 1)}
	s3 := &mockS3{getResult: []byte(`{"text":"ok"}`)}
	redis := &mockRedis{
		sub: sub,
		job: &model.Job{
			ID:        "test-job",
			Status:    model.JobStatusCompleted,
			ResultRef: "test-job/result.json",
		},
	}
	prod := &mockProducer{sub: sub}

	reg := buildRegistry("jobs.whisper-large-v3.sync")
	h := handler.NewSyncHandler(reg, s3, redis, prod, "", nil, nil)

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	fw, _ := mw.CreateFormFile("file", "audio.wav")
	_, _ = fw.Write([]byte("audio"))
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	// Single registered model → auto-selected, request succeeds.
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 when single model is auto-selected, got %d: %s", w.Code, w.Body.String())
	}
}

// TestSyncHandler_MissingModelField_MultipleModels verifies that when multiple
// models are registered for a path, omitting "model" returns 400.
func TestSyncHandler_MissingModelField_MultipleModels(t *testing.T) {
	cfgs := []config.ServiceConfig{
		{
			Type:  "transcription",
			Model: "whisper-large-v3",
			Operations: map[string][]string{
				"transcription": {"/v1/audio/transcriptions"},
			},
			InferenceURL: "http://inference.example.com",
			InputTopic:   "jobs.whisper-large-v3.input",
			ResultTopic:  "jobs.whisper-large-v3.results",
			SyncTopic:    "jobs.whisper-large-v3.sync",
		},
		{
			Type:  "transcription",
			Model: "whisper-turbo",
			Operations: map[string][]string{
				"transcription": {"/v1/audio/transcriptions"},
			},
			InferenceURL: "http://inference-turbo.example.com",
			InputTopic:   "jobs.whisper-turbo.input",
			ResultTopic:  "jobs.whisper-turbo.results",
			SyncTopic:    "jobs.whisper-turbo.sync",
		},
	}
	reg := service.NewRegistry(cfgs)
	h := handler.NewSyncHandler(reg, &mockS3{}, &mockRedis{}, &mockProducer{}, "", nil, nil)

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	fw, _ := mw.CreateFormFile("file", "audio.wav")
	_, _ = fw.Write([]byte("audio"))
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 when multiple models and no model field, got %d", w.Code)
	}
}

// TestSyncHandler_UnknownModel verifies that an unknown model returns 400.
func TestSyncHandler_UnknownModel(t *testing.T) {
	reg := buildRegistry("jobs.whisper-large-v3.sync")
	h := handler.NewSyncHandler(reg, &mockS3{}, &mockRedis{}, &mockProducer{}, "", nil, nil)

	req := multipartRequest(t, "/v1/audio/transcriptions", "unknown-model", []byte("audio"))
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for unknown model, got %d", w.Code)
	}
}

// TestSyncHandler_UnsupportedContentType verifies that unsupported content types
// return 415.
func TestSyncHandler_UnsupportedContentType(t *testing.T) {
	reg := buildRegistry("")
	h := handler.NewSyncHandler(reg, &mockS3{}, &mockRedis{}, &mockProducer{}, "", nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions",
		strings.NewReader("plain text body"))
	req.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("expected 415, got %d", w.Code)
	}
}

// ── Multi-backend tests ────────────────────────────────────────────────────────

func buildMultiBackendRegistry(backends []config.BackendConfig) *service.Registry {
	cfgs := []config.ServiceConfig{{
		Type:  "ocr",
		Model: "llava",
		Operations: map[string][]string{
			"chat": {"/v1/chat/completions"},
		},
		Backends: backends,
	}}
	return service.NewRegistry(cfgs)
}

func jsonRequest(path string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path,
		strings.NewReader(`{"model":"llava","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestSyncHandler_MultiBackend_PrimaryFails_FallbackUsed(t *testing.T) {
	fallbackCalled := false
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalled = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer fallback.Close()

	reg := buildMultiBackendRegistry([]config.BackendConfig{
		{URL: primary.URL, Weight: 100},
		{URL: fallback.URL, Weight: 10},
	})
	h := handler.NewSyncHandler(reg, &mockS3{}, &mockRedis{}, &mockProducer{}, "", nil, nil)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, jsonRequest("/v1/chat/completions"))

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 from fallback, got %d", w.Code)
	}
	if !fallbackCalled {
		t.Error("fallback backend was not called")
	}
}

func TestSyncHandler_MultiBackend_NetworkError_FallbackUsed(t *testing.T) {
	fallbackCalled := false
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalled = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer fallback.Close()

	reg := buildMultiBackendRegistry([]config.BackendConfig{
		{URL: "http://127.0.0.1:1", Weight: 100}, // unreachable
		{URL: fallback.URL, Weight: 10},
	})
	h := handler.NewSyncHandler(reg, &mockS3{}, &mockRedis{}, &mockProducer{}, "", nil, nil)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, jsonRequest("/v1/chat/completions"))

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 from fallback, got %d", w.Code)
	}
	if !fallbackCalled {
		t.Error("fallback backend was not called")
	}
}

func TestSyncHandler_MultiBackend_AllFail_Returns502(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer primary.Close()
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer secondary.Close()

	reg := buildMultiBackendRegistry([]config.BackendConfig{
		{URL: primary.URL, Weight: 100},
		{URL: secondary.URL, Weight: 10},
	})
	h := handler.NewSyncHandler(reg, &mockS3{}, &mockRedis{}, &mockProducer{}, "", nil, nil)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, jsonRequest("/v1/chat/completions"))

	if w.Code != http.StatusBadGateway {
		t.Errorf("expected 502 when all backends fail, got %d", w.Code)
	}
}

func TestSyncHandler_MultiBackend_WeightZero_UsedAsFallback(t *testing.T) {
	fallbackCalled := false
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalled = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer fallback.Close()

	reg := buildMultiBackendRegistry([]config.BackendConfig{
		{URL: primary.URL, Weight: 100},
		{URL: fallback.URL, Weight: 0}, // fallback-only
	})
	h := handler.NewSyncHandler(reg, &mockS3{}, &mockRedis{}, &mockProducer{}, "", nil, nil)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, jsonRequest("/v1/chat/completions"))

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 from weight=0 fallback, got %d", w.Code)
	}
	if !fallbackCalled {
		t.Error("weight=0 fallback backend was not called after primary failure")
	}
}

func TestSyncHandler_MultiBackend_4xx_NotRetried(t *testing.T) {
	callCount := 0
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusOK)
	}))
	defer fallback.Close()

	reg := buildMultiBackendRegistry([]config.BackendConfig{
		{URL: primary.URL, Weight: 100},
		{URL: fallback.URL, Weight: 10},
	})
	h := handler.NewSyncHandler(reg, &mockS3{}, &mockRedis{}, &mockProducer{}, "", nil, nil)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, jsonRequest("/v1/chat/completions"))

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 forwarded as-is, got %d", w.Code)
	}
	if callCount != 1 {
		t.Errorf("4xx should not trigger retry; backend called %d time(s)", callCount)
	}
}

func TestSyncHandler_BackwardCompat_SingleInferenceURL(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	cfgs := []config.ServiceConfig{{
		Type:  "ocr",
		Model: "llava",
		Operations: map[string][]string{
			"chat": {"/v1/chat/completions"},
		},
		InferenceURL: upstream.URL, // legacy single-URL config
	}}
	reg := service.NewRegistry(cfgs)
	h := handler.NewSyncHandler(reg, &mockS3{}, &mockRedis{}, &mockProducer{}, "", nil, nil)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, jsonRequest("/v1/chat/completions"))

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 with legacy inference_url, got %d", w.Code)
	}
	if !called {
		t.Error("upstream was not called")
	}
}
