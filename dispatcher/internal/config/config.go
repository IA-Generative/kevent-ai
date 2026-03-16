package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Service       ServiceConfig       `yaml:"service"`
	Kafka         KafkaConfig         `yaml:"kafka"`
	S3            S3Config            `yaml:"s3"`
	Encryption    EncryptionConfig    `yaml:"encryption"`
	Inference     InferenceConfig     `yaml:"inference"`
	Transcription TranscriptionConfig `yaml:"transcription"`
	Diarization   DiarizationConfig   `yaml:"diarization"`
	OCR           OCRConfig           `yaml:"ocr"`
}

// InferenceConfig holds the shared local inference endpoint configuration.
// The base_url is combined with the OpenAI path supplied per-event (InputEvent.InferenceURL)
// to build the actual request URL: base_url + inference_url (e.g. "/v1/audio/transcriptions").
type InferenceConfig struct {
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`
	Timeout string `yaml:"timeout"`
}

func (c InferenceConfig) TimeoutDuration() time.Duration {
	if d, err := time.ParseDuration(c.Timeout); err == nil && d > 0 {
		return d
	}
	return 300 * time.Second
}

type EncryptionConfig struct {
	// Key is a hex-encoded 32-byte AES-256 key. Empty = encryption disabled.
	Key string `yaml:"key"`
}

type ServiceConfig struct {
	Type        string `yaml:"type"`
	ResultTopic string `yaml:"result_topic"`
	// La concurrence est gérée par containerConcurrency dans le Knative Service spec.
}

type KafkaConfig struct {
	Brokers []string   `yaml:"brokers"`
	SASL    SASLConfig `yaml:"sasl"`
	TLS     TLSConfig  `yaml:"tls"`
}

type SASLConfig struct {
	Mechanism string `yaml:"mechanism"` // PLAIN | SCRAM-SHA-256 | SCRAM-SHA-512
	Username  string `yaml:"username"`
	Password  string `yaml:"password"`
}

type TLSConfig struct {
	Enabled    bool   `yaml:"enabled"`
	CACertPath string `yaml:"ca_cert_path"`
}

// S3Config holds S3-compatible object storage credentials and settings.
type S3Config struct {
	Endpoint  string `yaml:"endpoint"`
	Region    string `yaml:"region"`
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`
	Bucket    string `yaml:"bucket"`
}

type TranscriptionConfig struct {
	Language       string `yaml:"language"`
	ResponseFormat string `yaml:"response_format"`
}

type DiarizationConfig struct {
	NumSpeakers int `yaml:"num_speakers"`
}

type OCRConfig struct {
	Prompt    string `yaml:"prompt"`
	MaxTokens int    `yaml:"max_tokens"`
}

// Load reads, env-expands, and validates the YAML config at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %q: %w", path, err)
	}

	expanded := []byte(os.Expand(string(data), expandWithDefault))

	var cfg Config
	if err := yaml.Unmarshal(expanded, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	cfg.applyDefaults()

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &cfg, nil
}

// expandWithDefault gère la syntaxe ${VAR:-default} que os.ExpandEnv ne supporte pas.
func expandWithDefault(key string) string {
	if i := strings.Index(key, ":-"); i >= 0 {
		varName, defaultVal := key[:i], key[i+2:]
		if v, ok := os.LookupEnv(varName); ok && v != "" {
			return v
		}
		return defaultVal
	}
	return os.Getenv(key)
}

func (c *Config) applyDefaults() {
	if c.Transcription.ResponseFormat == "" {
		c.Transcription.ResponseFormat = "json"
	}
	if c.OCR.Prompt == "" {
		c.OCR.Prompt = "Extract all text visible in this document. Return only the extracted text."
	}
	if c.OCR.MaxTokens == 0 {
		c.OCR.MaxTokens = 4096
	}
}

func (c *Config) validate() error {
	if c.Service.Type == "" {
		return fmt.Errorf("service.type is required (set SERVICE_TYPE env var)")
	}
	if len(c.Kafka.Brokers) == 0 {
		return fmt.Errorf("kafka.brokers is required")
	}
	if c.S3.Endpoint == "" {
		return fmt.Errorf("s3.endpoint is required")
	}
	if c.S3.Region == "" {
		return fmt.Errorf("s3.region is required")
	}
	if c.S3.Bucket == "" {
		return fmt.Errorf("s3.bucket is required")
	}
	if c.Inference.BaseURL == "" {
		return fmt.Errorf("inference.base_url is required")
	}
	switch c.Service.Type {
	case "transcription", "diarization", "ocr":
		// valid types; per-type validation is handled by the adapter
	default:
		return fmt.Errorf("unknown service type %q (must be transcription, diarization, or ocr)", c.Service.Type)
	}
	return nil
}
