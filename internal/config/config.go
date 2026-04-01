package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server     ServerConfig     `yaml:"server"`
	Kafka      KafkaConfig      `yaml:"kafka"`
	S3         S3Config         `yaml:"s3"`
	Redis      RedisConfig      `yaml:"redis"`
	Services   []ServiceConfig  `yaml:"services"`
	Encryption EncryptionConfig `yaml:"encryption"`
}

type EncryptionConfig struct {
	// Key is a hex-encoded 32-byte AES-256 key. Empty = encryption disabled.
	Key string `yaml:"key"`
}

type ServerConfig struct {
	Addr         string        `yaml:"addr"`
	ReadTimeout  time.Duration `yaml:"read_timeout"`
	WriteTimeout time.Duration `yaml:"write_timeout"`
	IdleTimeout  time.Duration `yaml:"idle_timeout"`
}

type KafkaConfig struct {
	Brokers       []string   `yaml:"brokers"`
	ConsumerGroup string     `yaml:"consumer_group"`
	SASL          SASLConfig `yaml:"sasl"`
	TLS           TLSConfig  `yaml:"tls"`
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
	Endpoint  string `yaml:"endpoint"`   // e.g. https://s3.fr-par.scw.cloud
	Region    string `yaml:"region"`     // e.g. fr-par
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`
	Bucket    string `yaml:"bucket"`
}

type RedisConfig struct {
	Addr     string `yaml:"addr"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
	JobTTLH  int    `yaml:"job_ttl_hours"`
}

// ServiceConfig declares a single inference service type.
// New services are added here (config.yaml) — no Go code required.
type ServiceConfig struct {
	Type string `yaml:"type"`
	// Sync / OpenAI-compatible mode (optional).
	// Model is the value of the "model" field in the OpenAI payload used to
	// route the request to the correct InferenceService backend.
	Model string `yaml:"model"`
	// Default marks this model as the default for its service type.
	// Used when a request omits the model field and multiple models are configured.
	// At most one entry per type should be marked as default.
	Default bool `yaml:"default"`
	// Operations maps operation names to their URL paths.
	// e.g. {"transcription": ["/v1/audio/transcriptions"], "translation": ["/v1/audio/translations"]}
	// Multiple paths per operation are all indexed for sync routing; the first is used for async.
	Operations   map[string][]string `yaml:"operations"`
	InferenceURL string              `yaml:"inference_url"` // InferenceService cluster URL
	// Async / Kafka mode.
	InputTopic string `yaml:"input_topic"`
	ResultTopic string `yaml:"result_topic"`
	// SyncTopic is the dedicated Kafka topic for priority (sync-over-Kafka) jobs.
	// When set, POST /v1/* multipart requests are routed through Kafka instead of
	// proxied directly, giving them priority over async jobs via a second KafkaSource.
	SyncTopic     string   `yaml:"sync_topic"`
	AcceptedExts  []string `yaml:"accepted_exts"`
	MaxFileSizeMB int64    `yaml:"max_file_size_mb"`
}

// Load reads and validates the YAML config file at path.
// Values of the form ${VAR} or ${VAR:-default} are expanded from the environment.
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
// Si la variable est définie et non vide, sa valeur est retournée ; sinon le défaut est utilisé.
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
	if c.Server.Addr == "" {
		c.Server.Addr = ":8080"
	}
	if c.Server.ReadTimeout == 0 {
		c.Server.ReadTimeout = 120 * time.Second
	}
	// WriteTimeout has no default: 0 means no timeout, which is correct for a
	// gateway that handles long-running sync-over-Kafka jobs (OCR, large audio).
	// Set explicitly in config if a hard limit is desired.
	if c.Server.IdleTimeout == 0 {
		c.Server.IdleTimeout = 120 * time.Second
	}
	if c.Kafka.ConsumerGroup == "" {
		c.Kafka.ConsumerGroup = "kevent-gateway"
	}
	if c.Redis.JobTTLH == 0 {
		c.Redis.JobTTLH = 72
	}
	for i := range c.Services {
		if c.Services[i].MaxFileSizeMB == 0 {
			c.Services[i].MaxFileSizeMB = 100
		}
	}
}

func (c *Config) validate() error {
	if c.S3.Endpoint == "" {
		return fmt.Errorf("s3.endpoint is required")
	}
	if c.S3.Region == "" {
		return fmt.Errorf("s3.region is required")
	}
	if c.S3.Bucket == "" {
		return fmt.Errorf("s3.bucket is required")
	}
	if c.Redis.Addr == "" {
		return fmt.Errorf("redis.addr is required")
	}
	if len(c.Services) == 0 {
		return fmt.Errorf("at least one service must be configured")
	}
	needsKafka := false
	for _, svc := range c.Services {
		if svc.Type == "" {
			return fmt.Errorf("a service has an empty type")
		}
		// input_topic and result_topic must both be set or both be empty.
		if (svc.InputTopic == "") != (svc.ResultTopic == "") {
			return fmt.Errorf("service %q: input_topic and result_topic must both be set or both be empty", svc.Type)
		}
		if svc.InputTopic != "" || svc.ResultTopic != "" || svc.SyncTopic != "" {
			needsKafka = true
		}
	}
	if needsKafka && len(c.Kafka.Brokers) == 0 {
		return fmt.Errorf("kafka.brokers is required when services use Kafka topics")
	}
	return nil
}
