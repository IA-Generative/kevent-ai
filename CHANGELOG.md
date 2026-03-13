# Changelog

All notable changes to this project are documented here.
Format: [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).
Versioning: each component is versioned independently — see tag conventions below.

## Tag conventions

| Component | Tag format | Example |
|---|---|---|
| Gateway (binary + Docker) | `gateway/vX.Y.Z` | `gateway/v0.2.5` |
| Dispatcher (binary + Docker) | `dispatcher/vX.Y.Z` | `dispatcher/v0.2.5` |
| Helm chart | auto-tagged by chart-releaser | `kevent-gateway-0.2.0` |

---

## Gateway

### [v0.2.7] — 2026-03-13

#### Added
- Startup log indicating whether at-rest encryption is enabled (`"S3 storage initialised" encryption=true/false`)

---

### [v0.2.6] — 2026-03-12

#### Fixed
- S3 upload failing with `request stream is not seekable` when encryption is enabled — replaced `PutObject` with `s3manager.Uploader` (multipart) which handles non-seekable `io.Pipe` streams natively

#### Changed
- Removed provider-specific references (Scaleway) from log messages and code comments

---

### [v0.2.5] — 2026-03-11

#### Added
- AES-256-GCM at-rest encryption for S3 objects (`internal/crypto/aes.go`) — chunked streaming, no extra dependencies
- `encryption.key` config field; empty = encryption disabled
- `model` field in `POST /jobs/{service_type}` multipart body — selects inference backend when multiple models share a type
- `Job.Model` and `InputEvent.Model` fields in data model

#### Changed
- Job routes restructured: `POST /jobs/{service_type}`, `GET /jobs/{service_type}/{id}`
- `openai_path` (string) → `openai_paths` (list) — each model can expose multiple OpenAI-compatible paths
- `inference_url` is now a base URL; the original request path is appended at runtime
- Kafka topics renamed from `jobs.<type>.*` to `jobs.<model>.*`
- `registry.RouteAsync(serviceType, model)` replaces `registry.Route(serviceType)` — supports multi-model types
- `MaxFileSizeForType` used for `MaxBytesReader` before multipart parse (maximum across all models for the type)
- Docker image moved to `ghcr.io/ronan-wescale/ai-kevent/gateway`

### [v0.2.4] — 2026-01-XX

#### Added
- SASL/TLS Kafka authentication (`internal/kafka/auth.go`)
- `kafka.sasl` and `kafka.tls` config sections
- Fix: `buildTransport` returns typed nil — must not assign directly to `RoundTripper` interface

---

## Dispatcher

### [v0.2.7] — 2026-03-13

#### Added
- Startup log indicating whether at-rest encryption is enabled (`"S3 storage initialised" encryption=true/false`)

---

### [v0.2.6] — 2026-03-12

#### Fixed
- S3 upload failing with `request stream is not seekable` when encryption is enabled — replaced `PutObject` with `s3manager.Uploader` (multipart)

#### Changed
- Removed provider-specific references (Scaleway) from code comments

---

### [v0.2.5] — 2026-03-11

#### Added
- AES-256-GCM at-rest decryption/encryption for S3 downloads/uploads (`internal/crypto/aes.go`)
- `encryption.key` config field
- `InputEvent.Model` field support
- `result_topic` auto-derived from active model: `jobs.<model>.results` when left empty

#### Changed
- Docker image moved to `ghcr.io/ronan-wescale/ai-kevent/dispatcher`

### [v0.2.4] — 2026-01-XX

#### Added
- SASL/TLS Kafka authentication (`internal/kafka/auth.go`)
- `KAFKA_SASL_USERNAME` / `KAFKA_SASL_PASSWORD` / `KAFKA_CA_CERT_PATH` env vars

---

## Helm chart (kevent-gateway)

### [0.2.3] — 2026-03-13

#### Changed
- `appVersion` updated to `0.2.7`
- `image.tag` default updated to `v0.2.7`

---

### [0.2.2] — 2026-03-13

#### Fixed
- ConfigMap template missing `encryption.key: "${ENCRYPTION_KEY:-}"` — the env var was injected in the pod but never referenced in `config.yaml`, so encryption was always disabled regardless of key configuration

---

### [0.2.1] — 2026-03-12

#### Changed
- `appVersion` updated to `0.2.6`
- `image.tag` default updated to `v0.2.6`

---

### [0.2.0] — 2026-03-11

#### Added
- `encryption.key` / `encryption.existingSecret` — AES-256-GCM key injection (Option A: chart creates Secret, Option B: External Secrets)
- `kevent-gateway.encryptionSecretName` helper in `_helpers.tpl`
- `README.md` for the chart

#### Changed
- `services[].openai_path` → `services[].openaiPaths` (list)
- `services[].inferenceURL` is now a base URL
- Topic fields renamed to model-based convention (`jobs.<model>.*`)
- `appVersion` updated to `0.2.5`

### [0.1.0] — 2026-01-XX

#### Added
- Initial chart: Deployment, Service, Ingress, ConfigMap, Secret
- Redis HA subchart (dandydeveloper/redis-ha)
- S3 credentials: `s3.accessKey/secretKey` or `s3.existingSecret`
- Kafka SASL: `kafka.sasl.password` or `kafka.sasl.existingSecret`
- Kafka TLS: `kafka.tls.existingCACertSecret`
