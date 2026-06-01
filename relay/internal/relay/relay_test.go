package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
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

// mockAdapterFunc lets tests control adapter behaviour per call.
type mockAdapterFunc struct {
	fn func(context.Context, adapter.CallInput) ([]byte, error)
}

func (a *mockAdapterFunc) Call(ctx context.Context, in adapter.CallInput) ([]byte, error) {
	return a.fn(ctx, in)
}

// mockS3CallCounter tracks GetObject call count and returns a configurable error.
type mockS3CallCounter struct {
	getErrFn  func() error
	putErr    error
	deleteErr error
	deleted   []string
}

func (m *mockS3CallCounter) GetObject(_ context.Context, _ string) (io.ReadCloser, int64, string, error) {
	return nil, 0, "", m.getErrFn()
}

func (m *mockS3CallCounter) PutObject(_ context.Context, _ string, _ io.Reader, _ int64, _ string) error {
	return m.putErr
}

func (m *mockS3CallCounter) DeleteObject(_ context.Context, key string) error {
	m.deleted = append(m.deleted, key)
	return m.deleteErr
}

// mockS3PutCounter lets tests control PutObject behaviour per call.
type mockS3PutCounter struct {
	getBody string
	putFn   func() error
	deleted []string
}

func (m *mockS3PutCounter) GetObject(_ context.Context, _ string) (io.ReadCloser, int64, string, error) {
	return io.NopCloser(strings.NewReader(m.getBody)), int64(len(m.getBody)), "application/octet-stream", nil
}

func (m *mockS3PutCounter) PutObject(_ context.Context, _ string, _ io.Reader, _ int64, _ string) error {
	return m.putFn()
}

func (m *mockS3PutCounter) DeleteObject(_ context.Context, key string) error {
	m.deleted = append(m.deleted, key)
	return nil
}

func newTestProcessor(s3 objectStore, adp adapter.Adapter, pub eventPublisher) *Processor {
	return &Processor{
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

// ── ParseInputEvent tests ─────────────────────────────────────────────────────

func TestParseInputEvent_Raw(t *testing.T) {
	data := []byte(`{"job_id":"abc","service_type":"transcription","model":"whisper-large-v3","input_ref":"abc/input.wav","created_at":"2026-01-01T00:00:00Z"}`)
	event, err := ParseInputEvent(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.JobID != "abc" {
		t.Errorf("expected job_id abc, got %q", event.JobID)
	}
}

func TestParseInputEvent_StructuredCloudEvent(t *testing.T) {
	data := []byte(`{"specversion":"1.0","type":"dev.knative.kafka.event","source":"/kafkasource","id":"123","data":{"job_id":"abc","service_type":"transcription","model":"whisper-large-v3","input_ref":"abc/input.wav","created_at":"2026-01-01T00:00:00Z"}}`)
	event, err := ParseInputEvent(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.JobID != "abc" {
		t.Errorf("expected job_id abc, got %q", event.JobID)
	}
}

// ── process() unit tests ──────────────────────────────────────────────────────

// TestProcess_S3NotFound_PublishesFailureAndReturnsNil verifies that a permanent
// S3 NoSuchKey error is treated as a permanent failure: a ResultEvent with status
// "failed" is published, nil is returned (no retry), and the input
// file is NOT re-queued.
func TestProcess_S3NotFound_PublishesFailureAndReturnsNil(t *testing.T) {
	noSuchKey := &s3types.NoSuchKey{}
	s3 := &mockS3{getErr: fmt.Errorf("getting S3 object %q: %w", "job-1/input.wav", noSuchKey)}
	pub := &mockPublisher{}

	p := newTestProcessor(s3, &mockAdapter{}, pub)
	err := p.process(context.Background(), testEvent())
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
// treated as transient: process() returns an error so the Job exits 1.
func TestProcess_S3TransientError_ReturnsError(t *testing.T) {
	s3 := &mockS3{getErr: errors.New("connection refused")}
	pub := &mockPublisher{}

	p := newTestProcessor(s3, &mockAdapter{}, pub)
	err := p.process(context.Background(), testEvent())
	if err == nil {
		t.Fatal("expected error for transient S3 failure, got nil")
	}
	if len(pub.published) != 0 {
		t.Errorf("expected no published events for transient error, got %d", len(pub.published))
	}
}

// TestProcess_InferenceFailure_PublishesFailureAndReturnsNil verifies that when
// the adapter returns an error (model/file invalid), a failure ResultEvent is
// published and nil is returned so the Job does not get retried.
func TestProcess_InferenceFailure_PublishesFailureAndReturnsNil(t *testing.T) {
	s3 := &mockS3{getBody: "audio data"}
	adp := &mockAdapter{err: errors.New("unsupported format")}
	pub := &mockPublisher{}

	p := newTestProcessor(s3, adp, pub)
	err := p.process(context.Background(), testEvent())
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
// ResultEvent is published with a non-empty result_ref and the input file is deleted.
func TestProcess_Success_PublishesCompletedEvent(t *testing.T) {
	s3 := &mockS3{getBody: "audio data"}
	adp := &mockAdapter{result: []byte(`{"text":"hello"}`)}
	pub := &mockPublisher{}

	p := newTestProcessor(s3, adp, pub)
	err := p.process(context.Background(), testEvent())
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
	if len(s3.deleted) != 1 || s3.deleted[0] != testEvent().InputRef {
		t.Errorf("expected input file %q to be deleted, got %v", testEvent().InputRef, s3.deleted)
	}
}

// TestProcess_S3NotFound_PublishFails_ReturnsError verifies that if publishing
// the permanent-failure event itself fails (Kafka down), process() returns an
// error so the Job is retried later.
func TestProcess_S3NotFound_PublishFails_ReturnsError(t *testing.T) {
	noSuchKey := &s3types.NoSuchKey{}
	s3 := &mockS3{getErr: fmt.Errorf("getting S3 object: %w", noSuchKey)}
	pub := &mockPublisher{err: errors.New("kafka unavailable")}

	p := newTestProcessor(s3, &mockAdapter{}, pub)
	err := p.process(context.Background(), testEvent())
	if err == nil {
		t.Fatal("expected error when publish of permanent failure fails, got nil")
	}
}

// TestProcess_InferenceTransientError_RetriesOnce verifies that a transient
// inference failure (e.g. network glitch, endpoint restart) triggers one
// immediate retry. The second attempt succeeds and a completed ResultEvent is
// published — no retry needed.
func TestProcess_InferenceTransientError_RetriesOnce(t *testing.T) {
	s3 := &mockS3{getBody: "audio data"}
	calls := 0
	adp := &mockAdapterFunc{fn: func(_ context.Context, _ adapter.CallInput) ([]byte, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("connection reset by peer")
		}
		return []byte(`{"text":"hello"}`), nil
	}}
	pub := &mockPublisher{}

	p := newTestProcessor(s3, adp, pub)
	err := p.process(context.Background(), testEvent())
	if err != nil {
		t.Fatalf("expected nil after successful retry, got: %v", err)
	}
	if calls != 2 {
		t.Errorf("expected 2 adapter calls (1 fail + 1 retry), got %d", calls)
	}
	if len(pub.published) != 1 || pub.published[0].Status != model.JobStatusCompleted {
		t.Errorf("expected one completed ResultEvent, got %v", pub.published)
	}
}

// TestProcess_InferencePermanentError_NoRetry verifies that a not-found S3
// error is NOT retried (the file is gone, retrying would be pointless).
func TestProcess_InferencePermanentError_NoRetry(t *testing.T) {
	noSuchKey := &s3types.NoSuchKey{}
	calls := 0
	s3 := &mockS3CallCounter{
		getErrFn: func() error {
			calls++
			return fmt.Errorf("getting S3 object: %w", noSuchKey)
		},
	}
	pub := &mockPublisher{}

	p := newTestProcessor(s3, &mockAdapter{}, pub)
	err := p.process(context.Background(), testEvent())
	if err != nil {
		t.Fatalf("expected nil for not-found (permanent failure), got: %v", err)
	}
	if calls != 1 {
		t.Errorf("expected exactly 1 GetObject call (no retry on not-found), got %d", calls)
	}
	if len(pub.published) != 1 || pub.published[0].Status != model.JobStatusFailed {
		t.Errorf("expected one failed ResultEvent, got %v", pub.published)
	}
}

// TestProcess_S3PutTransientError_RetriesOnce verifies that a transient S3
// PutObject failure triggers one immediate retry before returning an error.
func TestProcess_S3PutTransientError_RetriesOnce(t *testing.T) {
	putCalls := 0
	s3 := &mockS3PutCounter{
		getBody: "audio data",
		putFn: func() error {
			putCalls++
			if putCalls == 1 {
				return errors.New("connection reset")
			}
			return nil
		},
	}
	adp := &mockAdapter{result: []byte(`{"text":"hello"}`)}
	pub := &mockPublisher{}

	p := newTestProcessor(s3, adp, pub)
	err := p.process(context.Background(), testEvent())
	if err != nil {
		t.Fatalf("expected nil after successful s3 put retry, got: %v", err)
	}
	if putCalls != 2 {
		t.Errorf("expected 2 PutObject calls (1 fail + 1 retry), got %d", putCalls)
	}
	if len(pub.published) != 1 || pub.published[0].Status != model.JobStatusCompleted {
		t.Errorf("expected one completed ResultEvent, got %v", pub.published)
	}
}
