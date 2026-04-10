# Configuration reference

Configuration is loaded from `config.yaml` (or `$CONFIG_PATH`). Values of the form `${VAR}` or `${VAR:-default}` are expanded from the environment.

## Server

```yaml
server:
  addr: ":8080"
  read_timeout: 120s
  write_timeout: 0s        # 0 = no timeout (recommended for long sync jobs)
  idle_timeout: 120s
  consumer_header: ""      # header name for consumer identification (e.g. X-Consumer-Username)
  priority_header: ""      # header name for priority routing (e.g. X-Priority)
```

### `consumer_header`

When set to a non-empty header name (e.g. `X-Consumer-Username`, typically injected by APISIX after authentication):

- The consumer name is stored in the job record (`consumer_name` field in Redis).
- A Redis sorted set `consumer:{name}:jobs` is maintained per consumer (score = creation timestamp, same TTL as the job). Used by `GET /jobs`.
- `GET /jobs/{service_type}/{id}` enforces **ownership**: if the header is present in the request, `job.consumer_name` must match — returns `404` on mismatch (no information leak about other consumers' jobs).
- `kevent_jobs_by_consumer_total{mode, service_type, model, consumer}` is incremented per submission.

Leave empty in deployments without upstream authentication — no behaviour change, zero overhead.

### `priority_header`

When set and a request carries this header, the job is published to `services[].priority_topic` instead of `input_topic`. The relay processes priority-topic jobs via `POST /sync`, which sets `syncPriority++` and defers normal async jobs for its duration.

Leave empty to disable priority routing.

## Kafka

```yaml
kafka:
  brokers:
    - "kafka:9092"
  consumer_group: "kevent-gateway"
  sasl:
    mechanism: "SCRAM-SHA-512"   # PLAIN | SCRAM-SHA-256 | SCRAM-SHA-512
    username: "${KAFKA_USERNAME}"
    password: "${KAFKA_PASSWORD}"
  tls:
    enabled: true
    ca_cert_path: "/etc/ssl/certs/kafka-ca.crt"
```

## S3

```yaml
s3:
  endpoint: "${S3_ENDPOINT:-https://s3.fr-par.scw.cloud}"
  region: "${S3_REGION:-fr-par}"
  access_key: "${S3_ACCESS_KEY}"
  secret_key: "${S3_SECRET_KEY}"
  bucket: "${S3_BUCKET:-kevent-jobs}"
```

## Encryption

```yaml
encryption:
  key: "${ENCRYPTION_KEY:-}"   # hex-encoded 32-byte AES-256 key; empty = disabled
```

Generate a key:

```bash
openssl rand -hex 32
```

!!! warning
    The encryption key must be identical on all gateway and relay instances.

## Redis

```yaml
redis:
  addr: "${REDIS_ADDR:-redis:6379}"
  password: "${REDIS_PASSWORD:-}"
  db: 0
  job_ttl_hours: 72
```

## Services

```yaml
services:
  - type: audio                            # service type
    model: "whisper-large-v3"              # OpenAI model field
    default: true                          # fallback when model omitted

    # Sync routing
    operations:
      transcription:
        - "/v1/audio/transcriptions"
      translation:
        - "/v1/audio/translations"
    inference_url: "http://backend:80"     # base URL; original path appended
    sync_topic: "jobs.whisper.sync"        # omit → direct proxy for multipart

    # Async routing
    input_topic: "jobs.whisper.input"
    result_topic: "jobs.whisper.results"
    priority_topic: "jobs.whisper.priority"   # optional

    # File validation
    accepted_exts: [".mp3", ".wav", ".m4a", ".ogg", ".flac"]
    max_file_size_mb: 500

    # Swagger spec (optional)
    swagger_url: "https://example.com/openapi.json"
    swagger_headers:
      Authorization: "Bearer ${TOKEN}"
      Accept: "application/octet-stream"
```

### Field reference

| Field | Required | Default | Description |
|---|---|---|---|
| `type` | yes | — | Service type, used in `/jobs/{service_type}` |
| `model` | no | `""` | OpenAI model field value for routing |
| `default` | no | `false` | Fallback model when request omits `model` |
| `operations` | no | `{}` | Map of operation name → URL paths |
| `inference_url` | no | `""` | Backend base URL for direct proxy |
| `sync_topic` | no | `""` | Priority Kafka topic for sync-over-Kafka |
| `input_topic` | no | `""` | Kafka input topic for async jobs |
| `result_topic` | no | `""` | Kafka result topic for async jobs |
| `priority_topic` | no | `""` | Kafka topic for priority async jobs |
| `accepted_exts` | no | any | Allowed file extensions (e.g. `.mp3`) |
| `max_file_size_mb` | no | `100` | Max upload size in MB |
| `swagger_url` | no | `""` | URL to fetch an OpenAPI spec from |
| `swagger_headers` | no | `{}` | HTTP headers for `swagger_url` fetch |

## Hot reload

`POST /-/reload` re-reads the config file and atomically swaps the router. Kafka consumers are reconciled (stopped for removed topics, started for new ones). S3, Redis, and Kafka connections are not re-initialised.

The `configmap-reload` sidecar can trigger this automatically on ConfigMap changes.
