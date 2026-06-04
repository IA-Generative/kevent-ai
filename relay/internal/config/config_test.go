package config

import (
	"strings"
	"testing"
	"time"
)

func TestValidate_MissingRedisAddr(t *testing.T) {
	cfg := &Config{
		Model: "whisper-large-v3",
		S3: S3Config{
			Endpoint: "https://s3.example.com",
			Region:   "fr-par",
			Bucket:   "bucket",
		},
		Inference: InferenceConfig{BaseURL: "http://127.0.0.1:9000"},
	}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "redis.addr") {
		t.Errorf("expected redis.addr validation error, got %v", err)
	}
}

func TestValidate_MissingModel(t *testing.T) {
	cfg := &Config{
		Redis: RedisConfig{Addr: "redis:6379"},
		S3: S3Config{
			Endpoint: "https://s3.example.com",
			Region:   "fr-par",
			Bucket:   "bucket",
		},
		Inference: InferenceConfig{BaseURL: "http://127.0.0.1:9000"},
	}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "model") {
		t.Errorf("expected model validation error, got %v", err)
	}
}

func TestValidate_MissingS3Endpoint(t *testing.T) {
	cfg := &Config{
		Model: "whisper-large-v3",
		Redis: RedisConfig{Addr: "redis:6379"},
		S3: S3Config{
			Region: "fr-par",
			Bucket: "bucket",
		},
		Inference: InferenceConfig{BaseURL: "http://127.0.0.1:9000"},
	}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "s3.endpoint") {
		t.Errorf("expected s3.endpoint validation error, got %v", err)
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
