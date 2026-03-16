# kevent-gateway

Helm chart for the **kevent** API gateway — accepts file uploads, enqueues them as Kafka jobs, and exposes sync (OpenAI-compatible) and async endpoints for AI inference services running on Knative/KServe.

## Prerequisites

- Kubernetes ≥ 1.25
- Helm ≥ 3.10
- A running Kafka cluster (Strimzi recommended)
- A Redis-HA instance (included as subchart)
- Knative Serving + KServe for the inference backends

## Installation

### Via Helm repository (recommended)

The chart is published automatically to GitHub Pages on every push to `main` that changes `helm/`.

```bash
helm repo add kevent https://ronan-wescale.github.io/ai-kevent
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
  │  POST /jobs/{service_type}        (async)
  │  POST /v1/audio/transcriptions    (sync, OpenAI-compatible)
  ▼
Gateway (:8080)
  ├── S3 — upload/download (Scaleway Object Storage or any S3-compatible)
  ├── Redis — job state (TTL 72 h)
  └── Kafka — InputEvent → jobs.<model>.input
                    └── Relay sidecar (inside InferenceService pod)
                              └── ResultEvent → jobs.<model>.results → Gateway → Redis/Webhook
```

## Configuration

### Image

| Parameter | Description | Default |
|---|---|---|
| `image.repository` | Gateway image | `ghcr.io/your-org/kevent-gateway` |
| `image.tag` | Image tag | `latest` |
| `image.pullPolicy` | Pull policy | `IfNotPresent` |

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

| Parameter | Description | Default |
|---|---|---|
| `kafka.brokers` | Bootstrap brokers | `kafka:9092` |
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

Each entry in `services` registers one inference model with the gateway:

```yaml
services:
  - type: transcription
    model: "whisper-large-v3"
    # Sync (OpenAI-compatible) — optional
    openaiPaths:
      - "/v1/audio/transcriptions"
      - "/v1/audio/translations"
    inferenceURL: "http://kevent-transcription-predictor.default.svc.cluster.local"
    # Async (Kafka)
    inputTopic: "jobs.whisper-large-v3.input"
    resultTopic: "jobs.whisper-large-v3.results"
    acceptedExts: [".mp3", ".wav", ".m4a", ".ogg", ".flac"]
    maxFileSizeMB: 500
```

- **`type`** — used in the async route `/jobs/{service_type}`; multiple models can share the same type
- **`model`** — selected via the `model` form field in `POST /jobs/{service_type}`; if only one model exists for the type, it is used automatically
- **`openaiPaths`** — sync routes proxied to the InferenceService; the original request path is appended to `inferenceURL` at runtime
- **`inferenceURL`** — base URL of the Knative InferenceService predictor (cluster-local)
- Topics are named after the model, not the type (`jobs.<model>.input`)

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
    file      — binary (required)
    model     — model name (required if multiple models for the type)
    webhook   — callback URL (optional)

GET /jobs/{service_type}/{id}
  Returns: { id, status, model, result, error, created_at, updated_at }
```

### Sync (OpenAI-compatible)

```
POST /v1/audio/transcriptions
POST /v1/audio/translations
  Body: same as OpenAI API, field "model" selects the backend
```

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
- `encryption.existingSecret` option added (External Secrets Operator support)
