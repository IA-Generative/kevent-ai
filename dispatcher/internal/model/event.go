package model

import "time"

type JobStatus string

const (
	JobStatusCompleted JobStatus = "completed"
	JobStatusFailed    JobStatus = "failed"
)

// InputEvent is consumed from the model-specific input Kafka topic.
// Published by the gateway when a job is submitted.
type InputEvent struct {
	JobID        string    `json:"job_id"`
	ServiceType  string    `json:"service_type"`
	Model        string    `json:"model"`
	InputRef     string    `json:"input_ref"`     // S3 object key: "{job_id}/input.ext"
	InferenceURL string    `json:"inference_url"` // full URL the dispatcher must call
	CreatedAt    time.Time `json:"created_at"`
}

// ResultEvent is published to the service-specific result Kafka topic.
// Consumed by the gateway to update the job status.
type ResultEvent struct {
	JobID       string    `json:"job_id"`
	ServiceType string    `json:"service_type"`
	Status      JobStatus `json:"status"`               // completed | failed
	ResultRef   string    `json:"result_ref,omitempty"` // S3 object key for the output
	Error       string    `json:"error,omitempty"`
	CompletedAt time.Time `json:"completed_at"`
}
