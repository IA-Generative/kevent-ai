package adapter

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"kevent/relay/internal/config"
)

func newTestAdapter(backendURL string, clientTimeout time.Duration) *multipartAdapter {
	return &multipartAdapter{
		inf:    config.InferenceConfig{BaseURL: backendURL},
		client: &http.Client{Timeout: clientTimeout},
	}
}

func callInput() CallInput {
	return CallInput{
		JobID:        "job-1",
		Filename:     "audio.wav",
		ContentType:  "audio/wav",
		Size:         4,
		Body:         strings.NewReader("data"),
		InferenceURL: "/v1/infer",
	}
}

// TestCall_ParentContextCancelled_DoesNotAbortInference verifies that cancelling
// the Knative request context (simulating timeoutSeconds) does NOT abort an
// in-flight inference call. Only http.Client.Timeout governs the deadline.
func TestCall_ParentContextCancelled_DoesNotAbortInference(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Consume the body so the client's pipe-writer goroutine can finish.
		_, _ = io.ReadAll(r.Body)
		wg.Done()      // signal that inference has been reached
		time.Sleep(50 * time.Millisecond) // simulate slow inference
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"result":"ok"}`)
	}))
	defer backend.Close()

	a := newTestAdapter(backend.URL, 5*time.Second) // generous client timeout

	// Simulate Knative cancelling the request context (e.g. timeoutSeconds=300).
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		wg.Wait()   // wait until backend has received the request
		cancel()    // then cancel the parent context — as Knative would
	}()

	_, err := a.Call(ctx, callInput())
	if err != nil {
		t.Errorf("inference should succeed despite parent context cancellation, got: %v", err)
	}
}

// TestCall_ClientTimeout_Enforced verifies that http.Client.Timeout still applies
// when the parent context has no deadline.
func TestCall_ClientTimeout_Enforced(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		time.Sleep(200 * time.Millisecond) // longer than the 50ms client timeout
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer backend.Close()

	a := newTestAdapter(backend.URL, 50*time.Millisecond) // short timeout

	_, err := a.Call(context.Background(), callInput())
	if err == nil {
		t.Error("expected a timeout error, got nil")
	}
}

// TestCall_Success_ReturnsBody verifies the happy path: body is returned on 200.
func TestCall_Success_ReturnsBody(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"text":"hello"}`)
	}))
	defer backend.Close()

	a := newTestAdapter(backend.URL, 5*time.Second)
	got, err := a.Call(context.Background(), callInput())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != `{"text":"hello"}` {
		t.Errorf("unexpected body: %s", got)
	}
}

// TestCall_NonSuccessStatus_ReturnsError verifies that a 4xx/5xx from the
// inference endpoint is surfaced as an error.
func TestCall_NonSuccessStatus_ReturnsError(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"error":"bad file"}`)
	}))
	defer backend.Close()

	a := newTestAdapter(backend.URL, 5*time.Second)
	_, err := a.Call(context.Background(), callInput())
	if err == nil {
		t.Fatal("expected error for non-2xx status, got nil")
	}
	if !strings.Contains(err.Error(), "422") {
		t.Errorf("expected status code in error, got: %v", err)
	}
}
