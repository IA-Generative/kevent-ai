# kevent-gateway

Helm chart for the **kevent** API gateway — accepts file uploads, enqueues them as Kafka jobs, and exposes sync (OpenAI-compatible) and async endpoints for AI inference services running on Knative/KServe.

## Prerequisites

- Kubernetes ≥ 1.25
- Helm ≥ 3.10
- Redis-HA instance (included as subchart)
- Knative Serving + KServe for the inference backends
- Kafka cluster (Strimzi recommended) — **only required if services use Kafka topics** (async or sync-over-Kafka mode)

## Installation

### Via Helm repository (recommended)

The chart is published automatically to GitHub Pages on every push to `main` that changes `helm/`.

```bash
helm repo add kevent https://ia-generative.github.io/kevent-ai
helm repo update
helm install kevent-gateway kevent/kevent-gateway -f values.yaml
```

### From source

```bash
helm dependency update ./helm/gateway
helm upgrade --install kevent-gateway ./helm/gateway -f values.yaml
```

## Architecture

```
Client
  │  POST /jobs/{service_type}        (async — Kafka)
  │  POST /v1/audio/transcriptions    (sync-over-Kafka or direct proxy)
  │  POST /v1/rerank                  (sync direct proxy — no Kafka)
  ▼
Gateway (:8080)
  ├── S3 — upload/download
  ├── Redis — job state (TTL 72 h)
  └── Kafka — InputEvent → jobs.<model>.input   [only if service has Kafka topics]
                    └── Relay sidecar (inside InferenceService pod)
                              └── ResultEvent → jobs.<model>.results → Gateway → Redis/Webhook
```

## Configuration

### Image

| Parameter | Description | Default |
|---|---|---|
| `image.repository` | Gateway image | `ghcr.io/ia-generative/kevent-ai/gateway` |
| `image.tag` | Image tag | `v0.4.11` |
| `image.pullPolicy` | Pull policy | `IfNotPresent` |

### Config

| Parameter | Description | Default |
|---|---|---|
| `config.existingConfigMap` | **Option B** — name of an existing ConfigMap containing a `config.yaml` key. When set, the chart does not create a ConfigMap. | `""` |

When `config.existingConfigMap` is set, the chart mounts the referenced ConfigMap as `/etc/kevent/config.yaml`. The ConfigMap must contain the key `config.yaml`. Use this to manage configuration externally (e.g. with a GitOps tool or External Secrets).

### S3

Two options — choose one:

| Parameter | Description |
|---|---|
| `s3.endpoint` | S3-compatible endpoint (e.g. `https://s3.fr-par.scw.cloud`) |
| `s3.region` | Region (e.g. `fr-par`) |
| `s3.bucket` | Bucket name |
| `s3.accessKey` | **Option A** — access key (chart creates a Secret) |
| `s3.secretKey` | **Option A** — secret key |
| `s3.existingSecret` | **Option B** — name of an existing Secret containing `S3_ACCESS_KEY` and `S3_SECRET_KEY` |

### Kafka

Kafka is optional when all configured services are sync-direct (no `inputTopic` / `resultTopic`). The gateway skips producer and consumer initialisation in that case.

| Parameter | Description | Default |
|---|---|---|
| `kafka.brokers` | Bootstrap brokers (required if any service uses Kafka topics) | `kafka:9092` |
| `kafka.sasl.enabled` | Enable SASL authentication | `false` |
| `kafka.sasl.mechanism` | SASL mechanism | `SCRAM-SHA-512` |
| `kafka.sasl.username` | SASL username | `kevent-gateway` |
| `kafka.sasl.password` | **Option A** — SASL password (chart creates a Secret) | `""` |
| `kafka.sasl.existingSecret` | **Option B** — existing Secret with key `KAFKA_SASL_PASSWORD` | `""` |
| `kafka.tls.enabled` | Enable TLS | `false` |
| `kafka.tls.existingCACertSecret` | Secret containing `ca.crt` (Strimzi: `kafka-cluster-ca-cert`) | `kafka-cluster-ca-cert` |

### At-rest encryption (S3)

Files are encrypted with AES-256-GCM before upload and decrypted on download. The same key must be configured in all relay sidecars (`ENCRYPTION_KEY` env var).

Generate a key: `openssl rand -hex 32`

| Parameter | Description |
|---|---|
| `encryption.key` | **Option A** — hex-encoded 32-byte key (chart creates a Secret) |
| `encryption.existingSecret` | **Option B** — existing Secret with key `ENCRYPTION_KEY` (e.g. managed by External Secrets Operator) |

Leave both empty to disable encryption.

### Services

Each entry in `services` registers one inference model with the gateway. Three operating modes are supported per service:

| Mode | Required fields | Behaviour |
|---|---|---|
| **Async (Kafka)** | `inputTopic`, `resultTopic` | `POST /jobs/{type}` enqueues to Kafka; `GET /jobs/{type}/{id}` polls result |
| **Sync-over-Kafka** | `inputTopic`, `resultTopic`, `syncTopic` | `POST /v1/*` multipart → priority Kafka topic, result streamed back |
| **Sync direct proxy** | none (no topics) | `POST /v1/*` → proxied directly to `inferenceURL`; `POST /jobs/{type}` → 405 |

```yaml
services:
  # Full mode: async + sync-over-Kafka
  - type: audio
    model: "whisper-large-v3"
    default: true                 # fallback when request omits "model" field
    operations:
      transcription:
        - "/v1/audio/transcriptions"
      translation:
        - "/v1/audio/translations"
    inferenceURL: "http://kevent-transcription-predictor.default.svc.cluster.local"
    inputTopic: "jobs.whisper-large-v3.input"
    resultTopic: "jobs.whisper-large-v3.results"
    syncTopic: "jobs.whisper-large-v3.sync"   # enables sync-over-Kafka for multipart POST /v1/*
    acceptedExts: [".mp3", ".wav", ".m4a", ".ogg", ".flac"]
    maxFileSizeMB: 500

  # Sync-direct only: no Kafka, POST /v1/* proxied directly
  - type: reranker
    model: "bge-reranker-v2-m3"
    operations:
      rerank:
        - "/v1/rerank"
    inferenceURL: "http://kevent-reranker-predictor.default.svc.cluster.local"
    # No inputTopic / resultTopic → sync-direct mode only
```

**Field reference:**

| Field | Description |
|---|---|
| `type` | Service type used in async routes (`/jobs/{type}`). Multiple models can share the same type. |
| `model` | Model identifier — matched against the `model` field in the request. |
| `default` | `true` → used as fallback when `model` is omitted and multiple models are registered for the type. |
| `operations` | Map of `operationName → [url-paths]`. All paths are indexed for sync routing. The first path of the selected operation is forwarded in async InputEvents. |
| `inferenceURL` | Base URL of the Knative InferenceService predictor (cluster-local). The original request path is appended at runtime. |
| `inputTopic` | Kafka topic for async input events. Absent = no async support for this service. |
| `resultTopic` | Kafka topic for result events. Must be set if `inputTopic` is set (and vice versa). |
| `syncTopic` | Priority Kafka topic for sync-over-Kafka (`POST /v1/*` multipart). Optional. |
| `acceptedExts` | Allowed file extensions (e.g. `[".mp3", ".wav"]`). Empty or absent = all extensions accepted. |
| `maxFileSizeMB` | Maximum upload size in MB. `0` or absent = 100 MB default. |
| `swaggerURL` | Optional URL to an OpenAPI JSON spec for this service (e.g. raw GitHub URL). Fetched once at startup; served at `GET /swagger/{type}/{model}` and shown in the `/docs` dropdown. Failures are logged and skipped — startup is never blocked. |

### Redis HA

The chart includes [redis-ha](https://github.com/DandyDeveloper/charts/tree/master/charts/redis-ha) as a subchart. HAProxy is enabled by default to expose a stable master endpoint.

| Parameter | Description | Default |
|---|---|---|
| `redis-ha.enabled` | Deploy Redis HA | `true` |
| `redis-ha.haproxy.enabled` | Enable HAProxy frontend | `true` |
| `redis-ha.replicas` | Redis replica count | `3` |

## API reference

### Async

```
POST /jobs/{service_type}
  Content-Type: multipart/form-data
  Fields:
    file         — binary (required)
    model        — model name (required if multiple models for the type, no default)
    operation    — operation name (required if multiple operations, no default)
    callback_url — webhook URL called on completion (optional)

GET /jobs/{service_type}/{id}
  Returns: { job_id, status, model, result, error, created_at, updated_at }
  Note: result S3 file is deleted after this call — subsequent calls return 404.
```

> `POST /jobs/{service_type}` returns **405** for sync-direct only services (no `inputTopic` configured).

### Sync (OpenAI-compatible)

```
POST /v1/<operation-path>
  Body: same as OpenAI API; "model" field selects the backend
  Routing:
    multipart + syncTopic configured → sync-over-Kafka (priority, keep-alive)
    multipart + no syncTopic         → direct proxy to inferenceURL
    application/json                 → direct proxy to inferenceURL
```

### Other endpoints

```
GET /health        → { "status": "ok", "time": "..." }
GET /metrics       → Prometheus text format
GET /openapi.yaml  → OpenAPI 3.0.3 spec (generated at startup from registry)
GET /docs          → Swagger UI
```

## Monitoring (Prometheus Operator)

A `ServiceMonitor` can be created automatically if the [Prometheus Operator](https://github.com/prometheus-operator/prometheus-operator) is installed in the cluster.

| Parameter | Description | Default |
|---|---|---|
| `metrics.serviceMonitor.enabled` | Create a `ServiceMonitor` | `false` |
| `metrics.serviceMonitor.namespace` | Namespace for the `ServiceMonitor` (defaults to release namespace) | `""` |
| `metrics.serviceMonitor.interval` | Scrape interval | `30s` |
| `metrics.serviceMonitor.scrapeTimeout` | Scrape timeout | `10s` |
| `metrics.serviceMonitor.additionalLabels` | Extra labels on the `ServiceMonitor` — use to match the Prometheus Operator `serviceMonitorSelector` | `{}` |
| `metrics.serviceMonitor.relabelings` | Prometheus `relabelings` rules | `[]` |
| `metrics.serviceMonitor.metricRelabelings` | Prometheus `metricRelabelings` rules | `[]` |

Example with `kube-prometheus-stack`:

```yaml
metrics:
  serviceMonitor:
    enabled: true
    additionalLabels:
      release: kube-prometheus-stack   # matches Prometheus Operator selector
    interval: 30s
```

### Gateway metrics

| Metric | Type | Labels |
|---|---|---|
| `kevent_requests_total` | counter | `mode`, `service_type`, `model`, `status` |
| `kevent_request_duration_seconds` | histogram | `mode`, `service_type`, `model` |
| `kevent_sync_wait_duration_seconds` | histogram | `service_type`, `model` |
| `kevent_sync_jobs_in_flight` | gauge | — |
| `kevent_s3_operation_duration_seconds` | histogram | `operation` |
| `kevent_s3_errors_total` | counter | `operation` |
| `kevent_kafka_publish_duration_seconds` | histogram | `topic` |
| `kevent_kafka_publish_errors_total` | counter | `topic` |

### Relay sidecar metrics

The relay sidecar exposes its own `/metrics` endpoint (scraped separately, e.g. via a PodMonitor on port 8080 of the InferenceService pod).

| Metric | Type | Labels |
|---|---|---|
| `kevent_relay_jobs_total` | counter | `service_type`, `status` |
| `kevent_relay_inference_duration_seconds` | histogram | `service_type` |
| `kevent_relay_input_size_bytes` | histogram | `service_type` |
| `kevent_relay_sync_priority` | gauge | — |
| `kevent_relay_deferred_total` | counter | — |
| `kevent_relay_s3_operation_duration_seconds` | histogram | `operation` |
| `kevent_relay_s3_errors_total` | counter | `operation` |
| `kevent_relay_kafka_publish_errors_total` | counter | — |
| `kevent_relay_proxy_requests_total` | counter | `service_type`, `status` |
| `kevent_relay_proxy_duration_seconds` | histogram | `service_type` |

## Strimzi KafkaUser

The gateway requires a `KafkaUser` in the `infra-kafka` namespace:

```yaml
# k8s/kafka-users.yaml
apiVersion: kafka.strimzi.io/v1beta2
kind: KafkaUser
metadata:
  name: kevent-gateway
spec:
  authentication:
    type: scram-sha-512
  authorization:
    type: simple
    acls:
      - resource: { type: topic, name: jobs., patternType: prefix }
        operations: [Read, Write, Describe, Create]
      - resource: { type: group, name: kevent-gateway, patternType: prefix }
        operations: [Read]
```

The generated secret (`kevent-gateway` in `infra-kafka`) must be copied to the gateway namespace and referenced via `kafka.sasl.existingSecret`, or its password extracted and passed via `kafka.sasl.password`.

## Upgrade notes

### 0.1.0 → 0.2.0

- `openai_path` (string) renamed to `openai_paths` (list) in service config
- `inference_url` is now a base URL; the original request path is appended at runtime
- Kafka topics renamed from `jobs.<type>.*` to `jobs.<model>.*`
- At-rest AES-256-GCM encryption added (`encryption.key` / `encryption.existingSecret`)

### 0.2.x → 0.3.0

- `openai_paths` (flat list) replaced by `operations` map (`operationName → [paths]`)
- `syncTopic` field added per service (enables sync-over-Kafka for multipart `POST /v1/*`)

### 0.3.x → 0.5.x

- `config.existingConfigMap` option added — reference an external ConfigMap instead of letting the chart create one
- Services without `inputTopic`/`resultTopic` are now valid (sync-direct only mode); `kafka.brokers` is no longer required when no service uses Kafka topics
- `acceptedExts` empty = all extensions accepted (previously would reject all files)
- `maxFileSizeMB: 0` or absent = 100 MB default (previously 0 meant no limit)
