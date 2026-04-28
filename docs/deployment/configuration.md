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
  user_type_header: ""     # header name for user type (e.g. X-User-Type) — rate limiting + LLM metrics
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

### `user_type_header`

HTTP header injected by the upstream API gateway (e.g. APISIX, OPA) after token introspection. Typical value: `X-User-Type`. The header value (e.g. `sa`, `user`) is used:

- As a label on all LLM request/token/duration metrics (`user_type`)
- To select the rate limit tier from `rate_limits[service_type][user_type]`

Leave empty to disable user-type differentiation — the `"*"` fallback tier applies for rate limiting.

## Metrics

```yaml
metrics:
  top_consumers: 10        # expose top-N consumers in Prometheus; 0 = disabled
  consumer_labels: false   # direct per-consumer Prometheus labels (< 50 consumers only)
```

### `top_consumers`

When set to a positive integer, enables Redis sorted-set tracking of per-consumer LLM token usage. A background goroutine reads the top-N consumers every 60 seconds (and immediately at startup) and exposes them as `kevent_llm_consumer_tokens_top{consumer, user_type, type}`.

Only suitable when `server.consumer_header` is configured. Requires `server.user_type_header` for `user_type` labelling.

!!! warning
    Do **not** use `consumer_labels: true` with more than ~50 consumers. Each consumer creates a new Prometheus time series; at 100 k+ consumers this causes OOM. Use `top_consumers` instead.

## Rate limits

```yaml
rate_limits:
  audio:
    sa:
      rate: 100
      period: 1m
    user:
      rate: 20
      period: 1m
    "*":             # fallback: user_type absent or not listed
      rate: 10
      period: 1m
  ocr:
    "*":
      rate: 5
      period: 1m
```

Per-consumer, per-service fixed-window rate limiting backed by Redis. Returns `429 Too Many Requests` with a `Retry-After` header when exceeded.

| Field | Description |
|---|---|
| Key (e.g. `audio`) | Matches `services[].type` — all models of the same type share the counter |
| Sub-key (e.g. `sa`) | User type from `server.user_type_header`; `"*"` is the catch-all fallback |
| `rate` | Maximum requests allowed in the `period` |
| `period` | Window duration: `30s`, `1m`, `1h`, `24h` |

Leave `rate_limits` empty to disable. See [Rate limiting](../architecture/rate-limiting.md) for details.

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

    # Backend authentication (sync-direct only, optional)
    inference_headers:
      Authorization: "Bearer ${RERANKER_API_KEY}"
      X-Api-Key: "${BACKEND_KEY}"

    # LLM proxy (optional — activates when provider is set)
    provider: passthrough          # openai | anthropic | ollama | passthrough
    backend_model: "meta-llama/Meta-Llama-3-8B-Instruct"  # rewrites model field sent to backend
    response_cache_ttl: 3600       # seconds; 0 = disabled

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
| `inference_headers` | no | `{}` | HTTP headers injected on every sync-direct / LLM proxy request |
| `provider` | no | `""` | LLM provider: `openai`, `anthropic`, `ollama`, `passthrough` |
| `backend_model` | no | `""` | Backend model name — gateway rewrites the `model` field in the request |
| `response_cache_ttl` | no | `0` | Redis response cache TTL in seconds; `0` = disabled |
| `swagger_url` | no | `""` | URL to fetch an OpenAPI spec from |
| `swagger_headers` | no | `{}` | HTTP headers for `swagger_url` fetch |

### `inference_headers`

Arbitrary HTTP headers injected on every request forwarded to the inference backend. Only applies to the **sync-direct proxy** flow (JSON requests and multipart without `sync_topic`). Has no effect on async or sync-over-Kafka jobs.

- Header values support `${VAR}` / `${VAR:-default}` env expansion — store credentials in environment variables, not in plain config.
- Config headers **override** any header with the same name sent by the client.

```yaml
# Typical use cases:
inference_headers:
  Authorization: "Bearer ${RERANKER_API_KEY}"   # Bearer token
  X-Api-Key: "${BACKEND_KEY}"                   # custom API key header
  apikey: "${BACKEND_KEY}"                      # APISIX-style key
```

## Hot reload

`POST /-/reload` re-reads the config file and atomically swaps the router. Kafka consumers are reconciled (stopped for removed topics, started for new ones). S3, Redis, and Kafka connections are not re-initialised.

The `configmap-reload` sidecar can trigger this automatically on ConfigMap changes.
