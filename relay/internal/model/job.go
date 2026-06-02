package model

// JobStatus represents the terminal state of a relay job.
type JobStatus string

const (
	JobStatusCompleted JobStatus = "completed"
	JobStatusFailed    JobStatus = "failed"
)

// Job holds the fields the relay needs to process a job.
// Unmarshaled from the gateway's JSON blob at job:{id}.
type Job struct {
	ID           string            `json:"id"`
	ServiceType  string            `json:"service_type"`
	Model        string            `json:"model"`
	InputRef     string            `json:"input_ref"`
	InferenceURL string            `json:"inference_url,omitempty"`
	Params       map[string]string `json:"params,omitempty"`
}
