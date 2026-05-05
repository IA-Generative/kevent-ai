package handler_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"kevent/gateway/internal/config"
	"kevent/gateway/internal/handler"
	"kevent/gateway/internal/model"
	"kevent/gateway/internal/service"
)

// ── Mocks ────────────────────────────────────────────────────────────────────

type mockJobS3 struct {
	uploadErr   error
	uploaded    bool
	mu          sync.Mutex
	deletedKeys []string
}

func (m *mockJobS3) Upload(_ context.Context, _ string, _ io.Reader, _ int64, _ string) error {
	m.uploaded = true
	return m.uploadErr
}
func (m *mockJobS3) GetObject(_ context.Context, _ string) ([]byte, error) { return nil, nil }
func (m *mockJobS3) DeleteObject(_ context.Context, key string) error {
	m.mu.Lock()
	m.deletedKeys = append(m.deletedKeys, key)
	m.mu.Unlock()
	return nil
}

type mockAsyncStore struct {
	saveErr      error
	saved        bool
	updateCalled bool
}

func (m *mockAsyncStore) SaveJob(_ context.Context, _ *model.Job) error {
	m.saved = true
	return m.saveErr
}
func (m *mockAsyncStore) GetJob(_ context.Context, _ string) (*model.Job, error) {
	return nil, nil
}
func (m *mockAsyncStore) DeleteJob(_ context.Context, _ string) error { return nil }
func (m *mockAsyncStore) UpdateJobResult(_ context.Context, _ string, _ model.JobStatus, _, _ string) error {
	m.updateCalled = true
	return nil
}
func (m *mockAsyncStore) ListJobsByConsumer(_ context.Context, _ string, _, _ int64) ([]*model.Job, int64, error) {
	return nil, 0, nil
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// multiOpRegistry builds a registry with one model that has two operations
// (transcription + translation), reproducing the exact bug scenario.
func multiOpRegistry() *service.Registry {
	return service.NewRegistry([]config.ServiceConfig{{
		Type:  "transcription",
		Model: "faster-whisper",
		Operations: map[string][]string{
			"transcription": {"/v1/audio/transcriptions"},
			"translation":   {"/v1/audio/translations"},
		},
		InputTopic:    "jobs.faster-whisper.input",
		ResultTopic:   "jobs.faster-whisper.results",
		AcceptedExts:  []string{".mp3", ".wav"},
		MaxFileSizeMB: 100,
	}})
}

// singleOpRegistry builds a registry with one model and one operation.
func singleOpRegistry() *service.Registry {
	return service.NewRegistry([]config.ServiceConfig{{
		Type:  "transcription",
		Model: "faster-whisper",
		Operations: map[string][]string{
			"transcription": {"/v1/audio/transcriptions"},
		},
		InputTopic:    "jobs.faster-whisper.input",
		ResultTopic:   "jobs.faster-whisper.results",
		AcceptedExts:  []string{".mp3", ".wav"},
		MaxFileSizeMB: 100,
	}})
}

// submitReq builds a multipart POST /jobs/{serviceType} request and injects
// the chi route context so chi.URLParam works outside a real router.
func submitReq(t *testing.T, serviceType, modelName, operation, filename string, body []byte) *http.Request {
	t.Helper()
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	if modelName != "" {
		_ = mw.WriteField("model", modelName)
	}
	if operation != "" {
		_ = mw.WriteField("operation", operation)
	}
	fw, _ := mw.CreateFormFile("file", filename)
	_, _ = fw.Write(body)
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/jobs/"+serviceType, buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("service_type", serviceType)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func newAsyncHandler(reg *service.Registry, s3 *mockJobS3, store *mockAsyncStore, prod *mockProducer) *handler.JobHandler {
	return handler.NewJobHandler(reg, s3, store, prod, "", "", nil)
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestSubmit_InvalidOperation_NoSideEffects is the regression test for the bug
// where S3 and Redis were written before the operation was validated.
func TestSubmit_InvalidOperation_NoSideEffects(t *testing.T) {
	s3 := &mockJobS3{}
	store := &mockAsyncStore{}
	prod := &mockProducer{}

	req := submitReq(t, "transcription", "faster-whisper", "translations", "audio.wav", []byte("data"))
	w := httptest.NewRecorder()
	newAsyncHandler(multiOpRegistry(), s3, store, prod).Submit(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if s3.uploaded {
		t.Error("S3 upload must not be called when operation is invalid")
	}
	if store.saved {
		t.Error("Redis save must not be called when operation is invalid")
	}
	if prod.published {
		t.Error("Kafka publish must not be called when operation is invalid")
	}
}

// TestSubmit_MultipleOperations_MissingField verifies that omitting the operation
// field when a model has multiple operations returns 400 without side effects.
func TestSubmit_MultipleOperations_MissingField(t *testing.T) {
	s3 := &mockJobS3{}
	store := &mockAsyncStore{}

	req := submitReq(t, "transcription", "faster-whisper", "", "audio.wav", []byte("data"))
	w := httptest.NewRecorder()
	newAsyncHandler(multiOpRegistry(), s3, store, &mockProducer{}).Submit(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if s3.uploaded {
		t.Error("S3 upload must not be called when operation field is missing")
	}
	if store.saved {
		t.Error("Redis save must not be called when operation field is missing")
	}
}

// TestSubmit_NominalPath verifies the full success path: S3 → Redis → Kafka → 202.
func TestSubmit_NominalPath(t *testing.T) {
	s3 := &mockJobS3{}
	store := &mockAsyncStore{}
	prod := &mockProducer{}

	req := submitReq(t, "transcription", "faster-whisper", "", "audio.wav", []byte("data"))
	w := httptest.NewRecorder()
	newAsyncHandler(singleOpRegistry(), s3, store, prod).Submit(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
	if !s3.uploaded {
		t.Error("S3 upload should have been called")
	}
	if !store.saved {
		t.Error("Redis save should have been called")
	}
	if !prod.published {
		t.Error("Kafka publish should have been called")
	}
}

// TestSubmit_SingleOperation_AutoSelect verifies that omitting the operation
// field auto-selects the only configured operation.
func TestSubmit_SingleOperation_AutoSelect(t *testing.T) {
	req := submitReq(t, "transcription", "faster-whisper", "", "audio.wav", []byte("data"))
	w := httptest.NewRecorder()
	newAsyncHandler(singleOpRegistry(), &mockJobS3{}, &mockAsyncStore{}, &mockProducer{}).Submit(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("expected 202 when operation auto-selected, got %d: %s", w.Code, w.Body.String())
	}
}

// TestSubmit_UnknownServiceType verifies that an unknown service type returns
// 404 before any side effects.
func TestSubmit_UnknownServiceType(t *testing.T) {
	s3 := &mockJobS3{}
	store := &mockAsyncStore{}

	req := submitReq(t, "unknown-type", "faster-whisper", "", "audio.wav", []byte("data"))
	w := httptest.NewRecorder()
	newAsyncHandler(singleOpRegistry(), s3, store, &mockProducer{}).Submit(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
	if s3.uploaded {
		t.Error("S3 upload must not be called for unknown service type")
	}
	if store.saved {
		t.Error("Redis save must not be called for unknown service type")
	}
}

// TestSubmit_MissingFile verifies that a missing file field returns 400.
func TestSubmit_MissingFile(t *testing.T) {
	s3 := &mockJobS3{}
	store := &mockAsyncStore{}

	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	_ = mw.WriteField("model", "faster-whisper")
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/jobs/transcription", buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("service_type", "transcription")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	w := httptest.NewRecorder()
	newAsyncHandler(singleOpRegistry(), s3, store, &mockProducer{}).Submit(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing file, got %d", w.Code)
	}
	if s3.uploaded {
		t.Error("S3 upload must not be called when file is missing")
	}
}

// TestSubmit_InvalidExtension verifies that a rejected file extension returns
// 400 before any side effects.
func TestSubmit_InvalidExtension(t *testing.T) {
	s3 := &mockJobS3{}
	store := &mockAsyncStore{}

	req := submitReq(t, "transcription", "faster-whisper", "", "document.pdf", []byte("data"))
	w := httptest.NewRecorder()
	newAsyncHandler(singleOpRegistry(), s3, store, &mockProducer{}).Submit(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid extension, got %d: %s", w.Code, w.Body.String())
	}
	if s3.uploaded {
		t.Error("S3 upload must not be called for an invalid file extension")
	}
	if store.saved {
		t.Error("Redis save must not be called for an invalid file extension")
	}
}

// TestSubmit_S3Failure verifies that an S3 upload error returns 500 without
// writing to Redis or publishing to Kafka.
func TestSubmit_S3Failure(t *testing.T) {
	s3 := &mockJobS3{uploadErr: fmt.Errorf("s3 unavailable")}
	store := &mockAsyncStore{}
	prod := &mockProducer{}

	req := submitReq(t, "transcription", "faster-whisper", "", "audio.wav", []byte("data"))
	w := httptest.NewRecorder()
	newAsyncHandler(singleOpRegistry(), s3, store, prod).Submit(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on S3 failure, got %d", w.Code)
	}
	if store.saved {
		t.Error("Redis save must not be called after S3 failure")
	}
	if prod.published {
		t.Error("Kafka publish must not be called after S3 failure")
	}
}

// TestSubmit_KafkaFailure verifies that a Kafka publish error returns 500,
// marks the job as failed in Redis, and cleans up the orphaned S3 input file.
func TestSubmit_KafkaFailure(t *testing.T) {
	s3 := &mockJobS3{}
	store := &mockAsyncStore{}
	prod := &mockProducer{publishErr: fmt.Errorf("kafka unavailable")}

	req := submitReq(t, "transcription", "faster-whisper", "", "audio.wav", []byte("data"))
	w := httptest.NewRecorder()
	newAsyncHandler(singleOpRegistry(), s3, store, prod).Submit(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on Kafka failure, got %d", w.Code)
	}
	if !store.saved {
		t.Error("Redis save should have been called before Kafka publish")
	}
	if !store.updateCalled {
		t.Error("job should be marked failed in Redis after Kafka failure")
	}
	// DeleteObject runs in a goroutine — give it a moment.
	time.Sleep(20 * time.Millisecond)
	s3.mu.Lock()
	deleted := len(s3.deletedKeys) > 0
	s3.mu.Unlock()
	if !deleted {
		t.Error("orphaned S3 input file should be deleted after Kafka failure")
	}
}
