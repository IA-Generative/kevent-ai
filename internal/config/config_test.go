package config_test

import (
	"os"
	"testing"
	"time"

	"kevent/gateway/internal/config"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "config-*.yaml")
	if err != nil {
		t.Fatalf("creating temp config: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("writing temp config: %v", err)
	}
	f.Close()
	return f.Name()
}

// minimalValid returns a config that satisfies all validation rules with the
// minimal set of required fields.
const minimalValid = `
s3:
  endpoint: https://s3.example.com
  region: us-east-1
  bucket: my-bucket
redis:
  addr: "localhost:6379"
kafka:
  brokers:
    - "localhost:9092"
services:
  - type: transcription
    input_topic: jobs.input
    result_topic: jobs.results
`

// syncOnlyValid has a service without Kafka topics (direct proxy only).
const syncOnlyValid = `
s3:
  endpoint: https://s3.example.com
  region: us-east-1
  bucket: my-bucket
redis:
  addr: "localhost:6379"
services:
  - type: transcription
    model: whisper
    inference_url: "http://whisper.svc"
`

// ── Load errors ───────────────────────────────────────────────────────────────

func TestLoad_FileNotFound(t *testing.T) {
	_, err := config.Load("/nonexistent/path/config.yaml")
	if err == nil {
		t.Error("expected error for missing file, got nil")
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	path := writeConfig(t, "not: valid: yaml: {{{")
	_, err := config.Load(path)
	if err == nil {
		t.Error("expected error for invalid YAML, got nil")
	}
}

// ── Defaults ──────────────────────────────────────────────────────────────────

func TestLoad_Defaults(t *testing.T) {
	path := writeConfig(t, minimalValid)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Server.Addr != ":8080" {
		t.Errorf("Addr: expected :8080, got %q", cfg.Server.Addr)
	}
	if cfg.Server.ReadTimeout != 120*time.Second {
		t.Errorf("ReadTimeout: expected 120s, got %v", cfg.Server.ReadTimeout)
	}
	if cfg.Server.IdleTimeout != 120*time.Second {
		t.Errorf("IdleTimeout: expected 120s, got %v", cfg.Server.IdleTimeout)
	}
	if cfg.Kafka.ConsumerGroup != "kevent-gateway" {
		t.Errorf("ConsumerGroup: expected kevent-gateway, got %q", cfg.Kafka.ConsumerGroup)
	}
	if cfg.Redis.JobTTLH != 72 {
		t.Errorf("JobTTLH: expected 72, got %d", cfg.Redis.JobTTLH)
	}
	for i, svc := range cfg.Services {
		if svc.MaxFileSizeMB != 100 {
			t.Errorf("services[%d].MaxFileSizeMB: expected 100, got %d", i, svc.MaxFileSizeMB)
		}
	}
}

func TestLoad_DefaultsNotOverrideExplicit(t *testing.T) {
	path := writeConfig(t, `
s3:
  endpoint: https://s3.example.com
  region: us-east-1
  bucket: my-bucket
redis:
  addr: "localhost:6379"
  job_ttl_hours: 48
server:
  addr: ":9090"
  read_timeout: 30s
kafka:
  brokers: ["localhost:9092"]
  consumer_group: my-group
services:
  - type: transcription
    input_topic: jobs.input
    result_topic: jobs.results
    max_file_size_mb: 200
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.Addr != ":9090" {
		t.Errorf("explicit addr should not be overridden, got %q", cfg.Server.Addr)
	}
	if cfg.Server.ReadTimeout != 30*time.Second {
		t.Errorf("explicit read_timeout should not be overridden, got %v", cfg.Server.ReadTimeout)
	}
	if cfg.Kafka.ConsumerGroup != "my-group" {
		t.Errorf("explicit consumer_group should not be overridden, got %q", cfg.Kafka.ConsumerGroup)
	}
	if cfg.Redis.JobTTLH != 48 {
		t.Errorf("explicit job_ttl_hours should not be overridden, got %d", cfg.Redis.JobTTLH)
	}
	if cfg.Services[0].MaxFileSizeMB != 200 {
		t.Errorf("explicit max_file_size_mb should not be overridden, got %d", cfg.Services[0].MaxFileSizeMB)
	}
}

// ── Env expansion ─────────────────────────────────────────────────────────────

func TestLoad_EnvVar_Set(t *testing.T) {
	t.Setenv("TEST_BUCKET", "env-bucket")
	path := writeConfig(t, `
s3:
  endpoint: https://s3.example.com
  region: us-east-1
  bucket: ${TEST_BUCKET}
redis:
  addr: "localhost:6379"
kafka:
  brokers: ["localhost:9092"]
services:
  - type: t
    input_topic: j.in
    result_topic: j.out
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.S3.Bucket != "env-bucket" {
		t.Errorf("expected bucket from env, got %q", cfg.S3.Bucket)
	}
}

func TestLoad_EnvVar_DefaultUsed(t *testing.T) {
	os.Unsetenv("UNSET_KEVENT_VAR")
	path := writeConfig(t, `
s3:
  endpoint: https://s3.example.com
  region: us-east-1
  bucket: ${UNSET_KEVENT_VAR:-fallback-bucket}
redis:
  addr: "localhost:6379"
kafka:
  brokers: ["localhost:9092"]
services:
  - type: t
    input_topic: j.in
    result_topic: j.out
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.S3.Bucket != "fallback-bucket" {
		t.Errorf("expected fallback value, got %q", cfg.S3.Bucket)
	}
}

func TestLoad_EnvVar_DefaultNotUsedWhenSet(t *testing.T) {
	t.Setenv("KEVENT_TEST_VAR", "actual-value")
	path := writeConfig(t, `
s3:
  endpoint: https://s3.example.com
  region: us-east-1
  bucket: ${KEVENT_TEST_VAR:-fallback}
redis:
  addr: "localhost:6379"
kafka:
  brokers: ["localhost:9092"]
services:
  - type: t
    input_topic: j.in
    result_topic: j.out
`)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.S3.Bucket != "actual-value" {
		t.Errorf("expected env value to take precedence, got %q", cfg.S3.Bucket)
	}
}

// ── Validation ────────────────────────────────────────────────────────────────

func TestLoad_Validate_MissingS3Endpoint(t *testing.T) {
	path := writeConfig(t, `
s3:
  region: us-east-1
  bucket: my-bucket
redis:
  addr: "localhost:6379"
services:
  - type: t
`)
	_, err := config.Load(path)
	if err == nil {
		t.Error("expected error for missing s3.endpoint")
	}
}

func TestLoad_Validate_MissingS3Region(t *testing.T) {
	path := writeConfig(t, `
s3:
  endpoint: https://s3.example.com
  bucket: my-bucket
redis:
  addr: "localhost:6379"
services:
  - type: t
`)
	_, err := config.Load(path)
	if err == nil {
		t.Error("expected error for missing s3.region")
	}
}

func TestLoad_Validate_MissingS3Bucket(t *testing.T) {
	path := writeConfig(t, `
s3:
  endpoint: https://s3.example.com
  region: us-east-1
redis:
  addr: "localhost:6379"
services:
  - type: t
`)
	_, err := config.Load(path)
	if err == nil {
		t.Error("expected error for missing s3.bucket")
	}
}

func TestLoad_Validate_MissingRedisAddr(t *testing.T) {
	path := writeConfig(t, `
s3:
  endpoint: https://s3.example.com
  region: us-east-1
  bucket: my-bucket
services:
  - type: t
`)
	_, err := config.Load(path)
	if err == nil {
		t.Error("expected error for missing redis.addr")
	}
}

func TestLoad_Validate_NoServices(t *testing.T) {
	path := writeConfig(t, `
s3:
  endpoint: https://s3.example.com
  region: us-east-1
  bucket: my-bucket
redis:
  addr: "localhost:6379"
services: []
`)
	_, err := config.Load(path)
	if err == nil {
		t.Error("expected error for empty services list")
	}
}

func TestLoad_Validate_ServiceEmptyType(t *testing.T) {
	path := writeConfig(t, `
s3:
  endpoint: https://s3.example.com
  region: us-east-1
  bucket: my-bucket
redis:
  addr: "localhost:6379"
services:
  - model: whisper
    inference_url: "http://svc"
`)
	_, err := config.Load(path)
	if err == nil {
		t.Error("expected error for service with empty type")
	}
}

func TestLoad_Validate_InputTopicWithoutResultTopic(t *testing.T) {
	path := writeConfig(t, `
s3:
  endpoint: https://s3.example.com
  region: us-east-1
  bucket: my-bucket
redis:
  addr: "localhost:6379"
services:
  - type: transcription
    input_topic: jobs.input
`)
	_, err := config.Load(path)
	if err == nil {
		t.Error("expected error: input_topic set without result_topic")
	}
}

func TestLoad_Validate_ResultTopicWithoutInputTopic(t *testing.T) {
	path := writeConfig(t, `
s3:
  endpoint: https://s3.example.com
  region: us-east-1
  bucket: my-bucket
redis:
  addr: "localhost:6379"
services:
  - type: transcription
    result_topic: jobs.results
`)
	_, err := config.Load(path)
	if err == nil {
		t.Error("expected error: result_topic set without input_topic")
	}
}

func TestLoad_Validate_KafkaTopicsWithoutBrokers(t *testing.T) {
	path := writeConfig(t, `
s3:
  endpoint: https://s3.example.com
  region: us-east-1
  bucket: my-bucket
redis:
  addr: "localhost:6379"
services:
  - type: transcription
    input_topic: jobs.input
    result_topic: jobs.results
`)
	_, err := config.Load(path)
	if err == nil {
		t.Error("expected error: kafka topics configured without kafka.brokers")
	}
}

func TestLoad_Validate_SyncOnlyServiceNoBrokersOK(t *testing.T) {
	// A service with only inference_url (no Kafka topics) does not require brokers.
	path := writeConfig(t, syncOnlyValid)
	_, err := config.Load(path)
	if err != nil {
		t.Errorf("sync-only service (no Kafka topics) should not require brokers, got: %v", err)
	}
}
