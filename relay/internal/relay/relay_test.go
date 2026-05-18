package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"kevent/relay/internal/adapter"
	"kevent/relay/internal/model"
)

// ── test doubles ─────────────────────────────────────────────────────────────

type mockS3 struct {
	getErr    error
	getBody   string
	putErr    error
	deleteErr error
	deleted   []string
}

func (m *mockS3) GetObject(_ context.Context, _ string) (io.ReadCloser, int64, string, error) {
	if m.getErr != nil {
		return nil, 0, "", m.getErr
	}
	return io.NopCloser(strings.NewReader(m.getBody)), int64(len(m.getBody)), "application/octet-stream", nil
}

func (m *mockS3) PutObject(_ context.Context, _ string, _ io.Reader, _ int64, _ string) error {
	return m.putErr
}

func (m *mockS3) DeleteObject(_ context.Context, key string) error {
	m.deleted = append(m.deleted, key)
	return m.deleteErr
}

type mockAdapter struct {
	result []byte
	err    error
}

func (a *mockAdapter) Call(_ context.Context, _ adapter.CallInput) ([]byte, error) {
	return a.result, a.err
}

type mockPublisher struct {
	published []*model.ResultEvent
	err       error
}

func (p *mockPublisher) PublishResultEvent(_ context.Context, _ string, event *model.ResultEvent) error {
	p.published = append(p.published, event)
	return p.err
}

func newTestDispatcher(s3 objectStore, adp adapter.Adapter, pub eventPublisher) *Dispatcher {
	return &Dispatcher{
		adapter:     adp,
		s3:          s3,
		publisher:   pub,
		resultTopic: "results",
	}
}

func testEvent() *model.InputEvent {
	return &model.InputEvent{
		JobID:        "job-1",
		ServiceType:  "transcription",
		Model:        "whisper-large-v3",
		InputRef:     "job-1/input.wav",
		InferenceURL: "/v1/audio/transcriptions",
	}
}

// ── decodeInputEvent tests ────────────────────────────────────────────────────

// TestDecodeInputEvent_BinaryMode verifies that a plain JSON body (KafkaSource
// binary mode, the default) is decoded correctly into an InputEvent.
func TestDecodeInputEvent_BinaryMode(t *testing.T) {
	body := `{"job_id":"abc-123","service_type":"transcription","model":"whisper-large-v3","input_ref":"abc-123/input.wav","created_at":"2026-03-13T13:00:00Z"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	event, err := decodeInputEvent(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.JobID != "abc-123" {
		t.Errorf("expected job_id abc-123, got %q", event.JobID)
	}
}

// TestDecodeInputEvent_StructuredCloudEvent verifies that a structured CloudEvent
// body (Content-Type: application/cloudevents+json) is unwrapped and the
// InputEvent is extracted from the "data" field.
func TestDecodeInputEvent_StructuredCloudEvent(t *testing.T) {
	body := `{
		"specversion": "1.0",
		"id": "550e8400-e29b-41d4-a716-446655440000",
		"type": "dev.knative.kafka.event",
		"source": "/apis/v1/namespaces/default/kafkasources/kevent-transcription-sync",
		"time": "2026-03-13T13:00:00Z",
		"datacontenttype": "application/json",
		"data": {"job_id":"abc-123","service_type":"transcription","model":"whisper-large-v3","input_ref":"abc-123/input.wav","created_at":"2026-03-13T13:00:00Z"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/sync", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/cloudevents+json")

	event, err := decodeInputEvent(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.JobID != "abc-123" {
		t.Errorf("expected job_id abc-123, got %q", event.JobID)
	}
	if event.Model != "whisper-large-v3" {
		t.Errorf("expected model whisper-large-v3, got %q", event.Model)
	}
}

// TestServeHTTP_Returns503WhenSyncActive verifies that the async handler defers
// jobs with 503 while a sync job is in progress on the same pod.
func TestServeHTTP_Returns503WhenSyncActive(t *testing.T) {
	d := &Dispatcher{}
	d.syncPriority.Store(1)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"job_id":"async-1"}`))
	w := httptest.NewRecorder()
	d.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 Service Unavailable, got %d", w.Code)
	}
}

// TestServeHTTP_ProcessesWhenIdle verifies that the async handler proceeds
// normally (no 503) when no sync job is active.
func TestServeHTTP_ProcessesWhenIdle(t *testing.T) {
	d := &Dispatcher{} // syncPriority is 0 by default

	// Empty job_id → 400 Bad Request, which proves the handler entered processing
	// rather than returning 503.
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	d.ServeHTTP(w, req)

	if w.Code == http.StatusServiceUnavailable {
		t.Errorf("unexpected 503: async handler should not defer when sync is idle")
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 (empty job_id), got %d", w.Code)
	}
}

// TestServeHTTP_MethodNotAllowed verifies that non-POST requests are rejected.
func TestServeHTTP_MethodNotAllowed(t *testing.T) {
	d := &Dispatcher{}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	d.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

// TestServeHTTPSync_UnsetsFlagAfterReturn verifies that the syncPriority flag is
// always cleared after ServeHTTPSync returns, even when the job fails.
func TestServeHTTPSync_UnsetsFlagAfterReturn(t *testing.T) {
	d := &Dispatcher{}

	if d.syncPriority.Load() != 0 {
		t.Fatal("syncPriority should be 0 initially")
	}

	// Invalid event (empty job_id) → serveHTTP returns 400, but the deferred
	// Store(0) must still execute.
	req := httptest.NewRequest(http.MethodPost, "/sync", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	d.ServeHTTPSync(w, req)

	if d.syncPriority.Load() != 0 {
		t.Error("syncPriority should be reset to 0 after ServeHTTPSync returns")
	}
}

// TestServeHTTPSync_BlocksAsyncConcurrently verifies that async jobs receive 503
// while a sync job holds the priority flag, and succeed once it is released.
func TestServeHTTPSync_BlocksAsyncConcurrently(t *testing.T) {
	d := &Dispatcher{}

	// Manually set the flag to simulate a sync job in progress.
	d.syncPriority.Store(1)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"job_id":"async-1"}`))
	w := httptest.NewRecorder()
	d.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("async should be deferred (503) while sync is active, got %d", w.Code)
	}

	// Release the flag — subsequent async requests must no longer get 503.
	d.syncPriority.Store(0)

	req2 := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	w2 := httptest.NewRecorder()
	d.ServeHTTP(w2, req2)

	if w2.Code == http.StatusServiceUnavailable {
		t.Errorf("async should not be deferred after sync is done, got 503")
	}
}

// ── process() unit tests ──────────────────────────────────────────────────────

// TestProcess_S3NotFound_PublishesFailureAndReturnsNil verifies that a permanent
// S3 NoSuchKey error is treated as a permanent failure: a ResultEvent with status
// "failed" is published, nil is returned (no KafkaSource retry), and the input
// file is NOT re-queued.
func TestProcess_S3NotFound_PublishesFailureAndReturnsNil(t *testing.T) {
	noSuchKey := &s3types.NoSuchKey{}
	s3 := &mockS3{getErr: fmt.Errorf("getting S3 object %q: %w", "job-1/input.wav", noSuchKey)}
	pub := &mockPublisher{}

	d := newTestDispatcher(s3, &mockAdapter{}, pub)
	err := d.process(context.Background(), testEvent())
	if err != nil {
		t.Fatalf("expected nil (permanent failure, no retry), got: %v", err)
	}
	if len(pub.published) != 1 {
		t.Fatalf("expected 1 published event, got %d", len(pub.published))
	}
	if pub.published[0].Status != model.JobStatusFailed {
		t.Errorf("expected status failed, got %q", pub.published[0].Status)
	}
	if !strings.Contains(pub.published[0].Error, "input file not found") {
		t.Errorf("expected 'input file not found' in error, got %q", pub.published[0].Error)
	}
}

// TestProcess_S3TransientError_ReturnsError verifies that a non-404 S3 error is
// treated as transient: process() returns an error so KafkaSource retries.
func TestProcess_S3TransientError_ReturnsError(t *testing.T) {
	s3 := &mockS3{getErr: errors.New("connection refused")}
	pub := &mockPublisher{}

	d := newTestDispatcher(s3, &mockAdapter{}, pub)
	err := d.process(context.Background(), testEvent())
	if err == nil {
		t.Fatal("expected error for transient S3 failure, got nil")
	}
	if len(pub.published) != 0 {
		t.Errorf("expected no published events for transient error, got %d", len(pub.published))
	}
}

// TestProcess_InferenceFailure_PublishesFailureAndReturnsNil verifies that when
// the adapter returns an error (model/file invalid), a failure ResultEvent is
// published and nil is returned so KafkaSource does not retry.
func TestProcess_InferenceFailure_PublishesFailureAndReturnsNil(t *testing.T) {
	s3 := &mockS3{getBody: "audio data"}
	adp := &mockAdapter{err: errors.New("unsupported format")}
	pub := &mockPublisher{}

	d := newTestDispatcher(s3, adp, pub)
	err := d.process(context.Background(), testEvent())
	if err != nil {
		t.Fatalf("expected nil (business failure, no retry), got: %v", err)
	}
	if len(pub.published) != 1 {
		t.Fatalf("expected 1 published event, got %d", len(pub.published))
	}
	if pub.published[0].Status != model.JobStatusFailed {
		t.Errorf("expected status failed, got %q", pub.published[0].Status)
	}
}

// TestProcess_Success_PublishesCompletedEvent verifies the happy path: a completed
// ResultEvent is published with a non-empty result_ref.
func TestProcess_Success_PublishesCompletedEvent(t *testing.T) {
	s3 := &mockS3{getBody: "audio data"}
	adp := &mockAdapter{result: []byte(`{"text":"hello"}`)}
	pub := &mockPublisher{}

	d := newTestDispatcher(s3, adp, pub)
	err := d.process(context.Background(), testEvent())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pub.published) != 1 {
		t.Fatalf("expected 1 published event, got %d", len(pub.published))
	}
	if pub.published[0].Status != model.JobStatusCompleted {
		t.Errorf("expected status completed, got %q", pub.published[0].Status)
	}
	if pub.published[0].ResultRef == "" {
		t.Error("expected non-empty result_ref")
	}
}

// TestProcess_S3NotFound_PublishFails_ReturnsError verifies that if publishing
// the permanent-failure event itself fails (Kafka down), process() returns an
// error so KafkaSource keeps the message and retries later.
func TestProcess_S3NotFound_PublishFails_ReturnsError(t *testing.T) {
	noSuchKey := &s3types.NoSuchKey{}
	s3 := &mockS3{getErr: fmt.Errorf("getting S3 object: %w", noSuchKey)}
	pub := &mockPublisher{err: errors.New("kafka unavailable")}

	d := newTestDispatcher(s3, &mockAdapter{}, pub)
	err := d.process(context.Background(), testEvent())
	if err == nil {
		t.Fatal("expected error when publish of permanent failure fails, got nil")
	}
}
