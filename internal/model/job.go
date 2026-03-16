package model

import "time"

type JobStatus string

const (
	JobStatusPending    JobStatus = "pending"
	JobStatusProcessing JobStatus = "processing"
	JobStatusCompleted  JobStatus = "completed"
	JobStatusFailed     JobStatus = "failed"
)

// Job is the full record stored in Redis. CallbackURL is kept internal
// (never exposed in API responses) but persisted so the consumer can
// trigger the webhook when the result arrives minutes or hours later.
type Job struct {
	ID          string    `json:"id"`
	ServiceType string    `json:"service_type"`
	Model       string    `json:"model"`
	Status      JobStatus `json:"status"`
	InputRef    string    `json:"input_ref"`
	ResultRef   string    `json:"result_ref,omitempty"`
	CallbackURL string    `json:"callback_url,omitempty"`
	Error       string    `json:"error,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// InputEvent is published to the model-specific input Kafka topic.
// KServe (or an inference worker) consumes this to trigger processing.
type InputEvent struct {
	JobID         string    `json:"job_id"`
	ServiceType   string    `json:"service_type"`
	Model         string    `json:"model"`
	InputRef      string    `json:"input_ref"`      // S3 object key: "{job_id}/input.ext"
	InferenceURL  string    `json:"inference_url"`  // OpenAI path the relay must append to its local base URL
	CreatedAt     time.Time `json:"created_at"`
}

// ResultEvent is consumed from the service-specific result Kafka topic.
// The inference worker publishes this when processing completes (or fails).
type ResultEvent struct {
	JobID       string    `json:"job_id"`
	ServiceType string    `json:"service_type"`
	Status      JobStatus `json:"status"`      // completed | failed
	ResultRef   string    `json:"result_ref,omitempty"` // MinIO object key for the output
	Error       string    `json:"error,omitempty"`
	CompletedAt time.Time `json:"completed_at"`
}
