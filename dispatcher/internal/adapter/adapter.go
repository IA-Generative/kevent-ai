package adapter

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"kevent/dispatcher/internal/config"
)

// CallInput contains the data passed to an adapter for inference.
type CallInput struct {
	JobID        string
	Filename     string
	ContentType  string
	Size         int64     // -1 if unknown
	Body         io.Reader // stream from S3; caller closes
	Model        string    // model name from InputEvent (e.g. "whisper-large-v3")
	InferenceURL string    // OpenAI path from InputEvent (e.g. "/v1/audio/transcriptions")
}

// Adapter sends an inference request to a KServe-compatible endpoint
// and returns the raw JSON response body.
type Adapter interface {
	Call(ctx context.Context, input CallInput) ([]byte, error)
}

// New returns the Adapter implementation matching cfg.Service.Type.
func New(cfg *config.Config) (Adapter, error) {
	inf := cfg.Inference
	switch cfg.Service.Type {
	case "transcription":
		return newTranscription(cfg.Transcription, inf), nil
	case "diarization":
		return newDiarization(cfg.Diarization, inf), nil
	case "ocr":
		return newOCR(cfg.OCR, inf), nil
	default:
		return nil, fmt.Errorf("unknown service type: %q", cfg.Service.Type)
	}
}

// buildURL constructs the full inference endpoint URL.
// If input.InferenceURL is set (path from the gateway event), it is appended
// to inf.BaseURL. Otherwise returns an empty string (adapter should handle fallback).
func buildURL(inf config.InferenceConfig, input CallInput) string {
	if inf.BaseURL != "" && input.InferenceURL != "" {
		return strings.TrimRight(inf.BaseURL, "/") + input.InferenceURL
	}
	return ""
}

// newHTTPClient creates an HTTP client with the given timeout, falling back to
// the InferenceConfig timeout if the per-type timeout is empty.
func newHTTPClient(perTypeTimeout string, inf config.InferenceConfig) *http.Client {
	d := inf.TimeoutDuration()
	if t, err := time.ParseDuration(perTypeTimeout); err == nil && t > 0 {
		d = t
	}
	return &http.Client{Timeout: d}
}
