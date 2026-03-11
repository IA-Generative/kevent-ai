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
	Transcription TranscriptionConfig `yaml:"transcription"`
	Diarization   DiarizationConfig   `yaml:"diarization"`
	OCR           OCRConfig           `yaml:"ocr"`
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
// Scaleway endpoint format: https://s3.<region>.scw.cloud
type S3Config struct {
	Endpoint  string `yaml:"endpoint"`
	Region    string `yaml:"region"`
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`
	Bucket    string `yaml:"bucket"`
}

type TranscriptionConfig struct {
	EndpointURL    string `yaml:"endpoint_url"`
	Model          string `yaml:"model"`
	Language       string `yaml:"language"`
	ResponseFormat string `yaml:"response_format"`
	APIKey         string `yaml:"api_key"`
	Timeout        string `yaml:"timeout"`
}

func (c TranscriptionConfig) TimeoutDuration() time.Duration {
	if d, err := time.ParseDuration(c.Timeout); err == nil && d > 0 {
		return d
	}
	return 300 * time.Second
}

type DiarizationConfig struct {
	EndpointURL string `yaml:"endpoint_url"`
	Model       string `yaml:"model"`
	NumSpeakers int    `yaml:"num_speakers"`
	APIKey      string `yaml:"api_key"`
	Timeout     string `yaml:"timeout"`
}

func (c DiarizationConfig) TimeoutDuration() time.Duration {
	if d, err := time.ParseDuration(c.Timeout); err == nil && d > 0 {
		return d
	}
	return 600 * time.Second
}

type OCRConfig struct {
	EndpointURL string `yaml:"endpoint_url"`
	Model       string `yaml:"model"`
	Prompt      string `yaml:"prompt"`
	MaxTokens   int    `yaml:"max_tokens"`
	APIKey      string `yaml:"api_key"`
	Timeout     string `yaml:"timeout"`
}

func (c OCRConfig) TimeoutDuration() time.Duration {
	if d, err := time.ParseDuration(c.Timeout); err == nil && d > 0 {
		return d
	}
	return 120 * time.Second
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
	// Apply model defaults first so auto-derivation of result_topic uses the model name.
	if c.Transcription.Model == "" {
		c.Transcription.Model = "whisper-large-v3"
	}
	if c.Diarization.Model == "" {
		c.Diarization.Model = "pyannote-audio-3.1"
	}
	if c.OCR.Model == "" {
		c.OCR.Model = "llava-v1.6-mistral-7b"
	}
	// Auto-derive result topic from the active model name.
	if c.Service.ResultTopic == "" && c.Service.Type != "" {
		c.Service.ResultTopic = "jobs." + c.activeModel() + ".results"
	}
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

// activeModel returns the model name for the configured service type.
func (c *Config) activeModel() string {
	switch c.Service.Type {
	case "transcription":
		return c.Transcription.Model
	case "diarization":
		return c.Diarization.Model
	case "ocr":
		return c.OCR.Model
	}
	return c.Service.Type
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
	switch c.Service.Type {
	case "transcription":
		if c.Transcription.EndpointURL == "" {
			return fmt.Errorf("transcription.endpoint_url is required")
		}
	case "diarization":
		if c.Diarization.EndpointURL == "" {
			return fmt.Errorf("diarization.endpoint_url is required")
		}
	case "ocr":
		if c.OCR.EndpointURL == "" {
			return fmt.Errorf("ocr.endpoint_url is required")
		}
	default:
		return fmt.Errorf("unknown service type %q (must be transcription, diarization, or ocr)", c.Service.Type)
	}
	return nil
}
