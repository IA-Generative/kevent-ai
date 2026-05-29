package config

import (
	"strings"
	"testing"
	"time"
)

func TestValidate_MissingInputTopic(t *testing.T) {
	cfg := &Config{
		Service: ServiceConfig{ResultTopic: "jobs.x.results"},
		Kafka: KafkaConfig{
			Brokers:       []string{"kafka:9092"},
			ConsumerGroup: "g",
		},
		S3: S3Config{
			Endpoint: "https://s3.example.com",
			Region:   "fr-par",
			Bucket:   "bucket",
		},
		Inference: InferenceConfig{BaseURL: "http://127.0.0.1:9000"},
	}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "input_topic") {
		t.Errorf("expected input_topic validation error, got %v", err)
	}
}

func TestValidate_MissingConsumerGroup(t *testing.T) {
	cfg := &Config{
		Service: ServiceConfig{ResultTopic: "jobs.x.results"},
		Kafka: KafkaConfig{
			Brokers:    []string{"kafka:9092"},
			InputTopic: "jobs.x.input",
		},
		S3: S3Config{
			Endpoint: "https://s3.example.com",
			Region:   "fr-par",
			Bucket:   "bucket",
		},
		Inference: InferenceConfig{BaseURL: "http://127.0.0.1:9000"},
	}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "consumer_group") {
		t.Errorf("expected consumer_group validation error, got %v", err)
	}
}

func TestTimeoutDuration(t *testing.T) {
	cases := []struct {
		input string
		want  time.Duration
	}{
		{"300s", 300 * time.Second},
		{"5m", 5 * time.Minute},
		{"0s", 0},
		{"0", 0},
		{"", 300 * time.Second},
		{"invalid", 300 * time.Second},
		{"-1s", 0},
	}

	for _, tc := range cases {
		got := (InferenceConfig{Timeout: tc.input}).TimeoutDuration()
		if got != tc.want {
			t.Errorf("TimeoutDuration(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}
