package relay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
