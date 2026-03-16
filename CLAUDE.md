# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository overview

**kevent** is an AI inference job gateway for Kubernetes/Knative. It exposes an HTTP API that accepts file uploads, enqueues them as Kafka jobs, and returns results asynchronously. A sidecar dispatcher runs inside each Knative InferenceService pod, consuming jobs from Kafka, calling the local inference model, and publishing results back.

Two independent Go modules (separate `go.mod`, separate Docker images):
- **Gateway** — root module `kevent/gateway`, entry point `cmd/gateway/main.go`
- **Dispatcher** — module `kevent/dispatcher` in `./dispatcher/`, entry point `dispatcher/cmd/dispatcher/main.go`

## Build commands

```bash
# Gateway
go build ./cmd/gateway          # from repo root
go vet ./...
go test ./...

# Dispatcher
cd dispatcher
go build ./cmd/dispatcher
go vet ./...
go test ./...
```

### Docker images & releases

Images are hosted on **ghcr.io** and released via GitHub Actions. To release:

```bash
# Tag and push — CI builds multi-arch image + binary + GitHub Release automatically
git tag gateway/vX.Y.Z    && git push origin gateway/vX.Y.Z
git tag dispatcher/vX.Y.Z && git push origin dispatcher/vX.Y.Z
```

Images:
- Gateway:    `ghcr.io/ronan-wescale/ai-kevent/gateway:vX.Y.Z`
- Dispatcher: `ghcr.io/ronan-wescale/ai-kevent/dispatcher:vX.Y.Z`

Current tags: gateway `v0.3.3`, dispatcher `v0.3.3`.

After tagging, also update:
1. `helm/gateway/values.yaml` → `image.tag`
2. `k8s/inference-transcription.yaml` → dispatcher image tag
3. Bump `helm/gateway/Chart.yaml` version if chart files changed
4. Update `CHANGELOG.md`
5. Commit to main

## Architecture

### Request flow

**Async** (`POST /jobs/{service_type}`):
```
Client
  │  POST /jobs/{service_type} (multipart)
  ▼
Gateway (:8080)
  ├── Upload file → S3
  ├── Save job record → Redis (TTL 72h)
  └── Publish InputEvent → Kafka jobs.<model>.input
                                    │
                              KafkaSource async (Knative Eventing)
                                    │  CloudEvent → POST /
                                    ▼
                         Dispatcher sidecar (:8080)
                              ├── Check syncPriority flag — if 1, return 503 (KafkaSource retries)
                              ├── Download file from S3
                              ├── POST to local inference model (127.0.0.1:9000)
                              ├── Upload result.json → S3
                              └── Publish ResultEvent → jobs.<model>.results
                                                                │
                                                         Gateway ConsumerManager
                                                              ├── Update Redis
                                                              ├── Notify Redis pub/sub (job:<id>:done)
                                                              └── Trigger webhook (if callback_url set)
```

**Sync-over-Kafka** (`POST /v1/*` multipart with `sync_topic` configured):
```
Client
  │  POST /v1/audio/transcriptions (multipart, keep-alive)
  ▼
Gateway
  ├── Upload file → S3
  ├── Save job → Redis
  ├── Subscribe Redis pub/sub job:<id>:done  ← before publishing
  ├── Publish InputEvent → Kafka jobs.<model>.sync
  └── Wait (Redis pub/sub) ──────────────────────────────────────────────┐
                                    │                                    │
                              KafkaSource sync (Knative Eventing)        │
                                    │  CloudEvent → POST /sync           │
                                    ▼                                    │
                         Dispatcher sidecar                              │
                              ├── Set syncPriority=1 (defers async jobs) │
                              ├── Process job (S3 → inference → S3)      │
                              ├── Publish ResultEvent → results topic     │
                              └── Unset syncPriority=0                   │
                                                    │                    │
                                             ConsumerManager             │
                                                    ├── Update Redis     │
                                                    └── Notify pub/sub ──┘
                                                                │
Gateway continues:
  ├── Fetch result from S3
  ├── Return result in HTTP response (200)
  └── Cleanup (delete S3 file + Redis job)
```

**Sync direct proxy** (`POST /v1/*` JSON, or multipart without `sync_topic`):
```
Gateway → HTTP proxy → InferenceService URL (inference_url in config)
```

### Sync (OpenAI-compatible) mode — routing summary

| Request | `sync_topic` configured | Path |
|---|---|---|
| `multipart/form-data` | yes | Sync-over-Kafka (priority, keep-alive) |
| `multipart/form-data` | no | Direct proxy to `inference_url` |
| `application/json` | any | Direct proxy to `inference_url` |

Configured via `services[].sync_topic`, `services[].openai_paths`, `services[].model`, `services[].inference_url` in `config.yaml`.

### Priority mechanism

When a sync job arrives at the dispatcher via `POST /sync`, it sets `syncPriority = 1`. Concurrent async CloudEvents to `POST /` see this flag and return `503 Service Unavailable`. KafkaSource retries them after `backoffDelay` (configured on the async KafkaSource). Once the sync job finishes, the flag is cleared and async jobs proceed normally.

This works across pod scale-out: each pod independently tracks its own sync job. No shared state across pods is needed — each pod that has a sync job defers its own async queue.

### Config loading

Both binaries use `config.Load(path)` which reads a YAML file and expands `${VAR}` / `${VAR:-default}` with `os.Expand` before unmarshalling. The config path defaults to `config.yaml` in the working directory, overridden by env var `CONFIG_PATH`.

**Adding a new service type** requires only a new entry in `config.yaml` (and `values.yaml` for Helm). No Go code change is needed — the service registry (`internal/service/registry.go`) is entirely config-driven.

### Kafka authentication (SASL/TLS)

`github.com/segmentio/kafka-go` v0.4.47 is used for both reading and writing.

- Readers use `kafkago.Dialer` (built by `internal/kafka/auth.go:buildDialer`)
- Writers use `kafkago.Transport` (built by `internal/kafka/auth.go:buildTransport`)

**Critical**: `buildTransport` returns `(*kafkago.Transport, error)` where transport can be `nil` when SASL/TLS is not configured. Never assign the result directly to `w.Transport` (a `RoundTripper` interface) — a typed nil pointer produces a non-nil interface value and panics. Use:
```go
if p.transport != nil {
    w.Transport = p.transport
}
```

Broker: `default-kafka-bootstrap.infra-kafka.svc.cluster.local:9093` (SASL_SSL, SCRAM-SHA-512).

### Strimzi KafkaUsers (`k8s/kafka-users.yaml`)

- `kevent-gateway` (namespace `infra-kafka`) — Write/Read on `jobs.*` topics, Read on `kevent-gateway*` groups
- `kevent-dispatcher` (namespace `infra-kafka`) — Read on `jobs.*` topics, **Read + Describe + Delete** on `inference-*` groups (Delete is required by the Knative controller for ConsumerGroup finalization), Write on `jobs.*` topics

### Secret hygiene

Strimzi-generated secrets (e.g. `kevent-dispatcher-kafka`) must not have trailing newlines in values. The Knative KafkaSource controller does exact string comparison on `sasl-type` — a `\n` suffix causes `[protocol SASL_SSL] unsupported SASL mechanism`. Verify with:
```bash
kubectl get secret kevent-dispatcher-kafka -n default \
  -o jsonpath='{.data.sasl-type}' | base64 -d | xxd
```

## Key files

| File | Purpose |
|---|---|
| `config.yaml` | Gateway config template (env-expanded at startup) |
| `dispatcher/config.yaml` | Dispatcher config template |
| `values.yaml` | Helm values for production deployment |
| `helm/gateway/` | Helm chart — generates ConfigMap, Secret, Deployment, Ingress |
| `k8s/kafka-users.yaml` | Strimzi KafkaUser ACLs (apply in `infra-kafka` namespace) |
| `k8s/inference-transcription.yaml` | KServe InferenceService + ServingRuntime for Whisper |
| `internal/service/registry.go` | Config-driven service registry (no code change needed to add services) |
| `internal/kafka/auth.go` | SASL/TLS dialer+transport construction for gateway |
| `dispatcher/internal/kafka/auth.go` | SASL/TLS transport construction for dispatcher |

## Deployment

Helm chart deploys the gateway with Redis-HA (HAProxy front-end). The dispatcher runs as a sidecar in the `ServingRuntime` (KServe), not managed by Helm.

The Helm chart is published to GitHub Pages at `https://ronan-wescale.github.io/ai-kevent` (auto-updated on push to `main` when `helm/` changes). The `gh-pages` branch must exist in the repository.

```bash
# Add Helm repo
helm repo add kevent https://ronan-wescale.github.io/ai-kevent
helm repo update
helm install kevent-gateway kevent/kevent-gateway -f values.yaml

# Or deploy from local sources
helm upgrade --install kevent-gateway ./helm/gateway -f values.yaml

# Apply Strimzi users (namespace infra-kafka)
kubectl apply -f k8s/kafka-users.yaml -n infra-kafka

# Apply InferenceService + ServingRuntime
kubectl apply -f k8s/inference-transcription.yaml
```
