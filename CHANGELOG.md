# Changelog

All notable changes to this project are documented here.
Format: [Keep a Changelog](https://keepachangelog.com/en/1.0.0/).
Versioning: each component is versioned independently — see tag conventions below.

## Tag conventions

| Component | Tag format | Example |
|---|---|---|
| Gateway (binary + Docker) | `gateway/vX.Y.Z` | `gateway/v0.2.5` |
| Relay (binary + Docker) | `relay/vX.Y.Z` | `relay/v0.2.5` |
| Helm chart | auto-tagged by chart-releaser | `kevent-gateway-0.2.0` |

---

## Gateway

### [v0.5.0] — 2026-04-03

#### Changed
- `UpdateJobResult` is now atomic: replaced `GetJob + SaveJob` with a Lua script to eliminate the read-modify-write race window under concurrent consumers
- `Producer.writerFor` uses `sync.RWMutex` — hot path (existing writer) acquires only a read lock
- `ConsumerManager` exposes `Wait()` backed by a `sync.WaitGroup`; `main` now drains consumers and in-flight webhooks before `Shutdown`
- Webhook goroutines are tracked in the same `WaitGroup` (no longer fire-and-forget on shutdown)
- `JobHandler` depends on `s3Store`, `asyncJobStore`, `eventProducer` interfaces instead of concrete types

#### Fixed
- S3 input file and Redis record are now cleaned up when Kafka publish fails in both `Submit` and the sync handler (previously orphaned indefinitely)
- `NotifyJobDone` logs Redis publish errors instead of silently discarding them
- Malformed JSON bodies on `POST /v1/*` now return HTTP 400 instead of routing with an empty model
- Per-request `reserved` maps moved to package-level variables

---

### [v0.4.18] — 2026-04-02

#### Changed
- Swagger UI `/docs`: inference endpoints now grouped by service type (`audio`, `ocr`, …) instead of a single "Inference" section — tags are built dynamically from the registry

---

### [v0.4.17] — 2026-04-02

#### Added
- Swagger UI now shows an **Authorize** button for API key authentication
- `securitySchemes: ApiKeyAuth` (header `apikey`) added to the generated OpenAPI spec
- Global security applies to all endpoints; `/health` is explicitly public

---

### [v0.4.16] — 2026-04-02

#### Fixed
- `/docs` `$ref` resolver errors: serve service specs at `/docs/spec/{type}/{model}` (under the `/docs*` gateway prefix) instead of blob URLs — `swagger-client` cannot resolve fragment refs against `blob:` base URLs
- Remove 404 link to non-existent `swagger-ui-standalone-preset.css`

---

### [v0.4.15] — 2026-04-02

#### Fixed
- `/docs` no longer fetches `/swagger/{type}/{model}` from the browser — service specs are now embedded inline in the HTML and exposed as blob URLs, so only `/openapi.yaml` is requested externally

---

### [v0.4.14] — 2026-04-02

#### Fixed
- Swagger UI `/docs` "No layout defined for StandaloneLayout": load `swagger-ui-standalone-preset.js` as a separate script tag and reference it as the global `SwaggerUIStandalonePreset` (not `SwaggerUIBundle.SwaggerUIStandalonePreset`)

---

### [v0.4.13] — 2026-04-02

#### Fixed
- Swagger UI `/docs` showing "no api definition provided": switch from `BaseLayout` to `StandaloneLayout` — the `urls[]` multi-spec dropdown is a Topbar feature exclusive to `StandaloneLayout`

---

### [v0.4.12] — 2026-04-02

#### Added
- `swagger_url` optional field per service — OpenAPI JSON spec fetched from URL (e.g. raw GitHub) at startup and cached in memory
- `GET /swagger/{type}/{model}` — serves the cached spec for a service
- `GET /docs` — Swagger UI now shows a multi-spec dropdown: gateway spec + one entry per service with `swagger_url`
- Fetch failures (URL unreachable, HTTP error, invalid JSON) are logged as warnings and never block startup

---

### [v0.4.11] — 2026-04-01

#### Added
- **Mode sync-direct uniquement** : un service sans `input_topic`/`result_topic` est traité en proxy direct, sans Kafka
  - `POST /v1/*` : proxy direct vers `inference_url` (comportement inchangé)
  - `POST /jobs/{service_type}` : retourne 405 pour ces services
  - Le producer et le consumer Kafka ne sont initialisés que si au moins un service utilise des topics Kafka
- `kafka.brokers` n'est plus obligatoire si aucun service ne configure de topic Kafka
- `config.existingConfigMap` dans le chart Helm : permet de référencer une ConfigMap existante

---

### [v0.4.10] — 2026-03-31

#### Fixed
- `kevent_requests_total` now recorded on sync-direct path (`proxyToInference`) with `mode="sync-direct"`

---

### [v0.4.9] — 2026-03-30

#### Added
- Prometheus metrics exposed at `GET /metrics`:
  - `kevent_requests_total` (counter, labels: `mode`, `service_type`, `model`, `status`) — all completed requests
  - `kevent_request_duration_seconds` (histogram, labels: `mode`, `service_type`, `model`) — end-to-end handler latency
  - `kevent_sync_wait_duration_seconds` (histogram, labels: `service_type`, `model`) — time blocked on Redis pub/sub in sync-over-Kafka mode
  - `kevent_sync_jobs_in_flight` (gauge) — open sync-over-Kafka connections waiting for relay results
  - `kevent_s3_operation_duration_seconds` (histogram, label: `operation`: upload/get/delete) — S3 latency
  - `kevent_s3_errors_total` (counter, label: `operation`) — S3 failures
  - `kevent_kafka_publish_duration_seconds` (histogram, label: `topic`) — Kafka write latency
  - `kevent_kafka_publish_errors_total` (counter, label: `topic`) — Kafka publish failures

---

### [v0.4.8] — 2026-03-24

#### Fixed
- **Root cause of HTTP/2 INTERNAL_ERROR**: `applyDefaults()` checked `WriteTimeout == 0` and applied a 60 s default, silently overriding the `write_timeout: 0s` set in `config.yaml`. Go's HTTP server was killing every connection after 60 s — APISix received an abrupt close and sent `RST_STREAM INTERNAL_ERROR` to the client. Removed the default entirely: 0 = no server-level write timeout, which is correct for an inference gateway.

---

### [v0.4.7] — 2026-03-24

#### Fixed
- Remove `X-Accel-Buffering: no` response header. It triggered APISix streaming proxy mode which breaks APISix plugins (key-auth, response transforms) that need to read the full body — resulting in `curl (92) HTTP/2 stream INTERNAL_ERROR`. Keepalive `\n` writes every 20 s are sufficient to keep the upstream connection alive within `proxy_read_timeout`.

---

### [v0.4.6] — 2026-03-24

#### Fixed
- Sync-over-Kafka: delay HTTP stream commitment to first keepalive tick (20 s) instead of flushing immediately. Fast inferences (< 20 s) now return proper HTTP status codes (422, 504, 500). Long inferences commit the 200 on the first tick to prevent APISix/nginx idle-connection drops; errors after that go in the JSON body.
- All unit tests pass again (`TestSyncHandler_ClientDisconnect`, `TestSyncHandler_InferenceFailure`)

---

### [v0.4.5] — 2026-03-24

#### Fixed
- Sync-over-Kafka: send HTTP 200 + headers immediately on connection open, then write a JSON-whitespace newline every 20 s while waiting for the relay result. Prevents APISix/nginx from dropping the TCP connection during long inferences ("upstream prematurely closed connection while reading response header")
- Remove duplicate `Content-Type` header set at end of `handleMultipartViaKafka`

---

### [v0.4.4] — 2026-03-23

#### Added
- Extra form fields (e.g. `vad_parameters`) are now forwarded end-to-end in all modes: gateway collects non-reserved fields from the multipart form, stores them in `InputEvent.Params`, and the relay injects them into the multipart request sent to the inference API
- `Params` override `extra_fields` from relay config when both define the same key

---

### [v0.3.0] — 2026-03-13

#### Added
- Sync-over-Kafka: `POST /v1/*` multipart requests are now routed through a dedicated priority Kafka topic (`sync_topic`) instead of proxied directly, when `sync_topic` is configured for a service
- `internal/storage/redis.go`: `SubscribeJobDone` / `NotifyJobDone` for Redis pub/sub result notification
- `internal/kafka/consumer.go`: notifies sync waiters via pub/sub when a result arrives
- `SyncTopic` field in `ServiceConfig` and `service.Def`

#### Changed
- `SyncHandler` now requires `s3`, `redis`, and `producer` dependencies (sync-over-Kafka path)
- JSON requests (`/v1/chat/completions`) continue to use direct proxy regardless of `sync_topic`

---

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
- Docker image moved to `ghcr.io/ia-generative/kevent-ai/gateway`

### [v0.2.4] — 2026-01-XX

#### Added
- SASL/TLS Kafka authentication (`internal/kafka/auth.go`)
- `kafka.sasl` and `kafka.tls` config sections
- Fix: `buildTransport` returns typed nil — must not assign directly to `RoundTripper` interface

---

## Relay

### [v0.5.1] — 2026-04-03

#### Fixed
- Proxy metrics (`kevent_relay_proxy_requests_total`, `kevent_relay_proxy_duration_seconds`) now carry the correct `service_type` label, derived automatically from `result_topic` (`jobs.<type>.results` → `<type>`). The `SERVICE_TYPE` env var is no longer required in any ServingRuntime.

---

### [v0.5.0] — 2026-04-03

#### Fixed
- `publishFailure` returns an error — when Kafka is unavailable after an inference failure, the error propagates so KafkaSource retries the job (previously the job was stuck in `pending` indefinitely)
- `newInferenceProxy` exits with `os.Exit(1)` on invalid `inference.base_url` instead of silently falling back to a hardcoded address
- `io.ReadAll` replaces `bytes.Buffer` in `decodeInputEvent`

---

### [v0.4.7] — 2026-03-31

#### Added
- `kevent_relay_proxy_requests_total` (counter, labels: `service_type`, `status`) — sync-direct requests proxied to the local model
- `kevent_relay_proxy_duration_seconds` (histogram, label: `service_type`) — sync-direct proxy latency

---

### [v0.4.6] — 2026-03-30

#### Added
- Prometheus metrics exposed at `GET /metrics`:
  - `kevent_relay_jobs_total` (counter, labels: `service_type`, `status`: completed/failed) — job outcomes
  - `kevent_relay_inference_duration_seconds` (histogram, label: `service_type`) — inference API call latency
  - `kevent_relay_input_size_bytes` (histogram, label: `service_type`) — input file size distribution
  - `kevent_relay_sync_priority` (gauge) — number of sync jobs in progress on this pod (non-zero defers async)
  - `kevent_relay_deferred_total` (counter) — async jobs returned 503 due to sync priority
  - `kevent_relay_s3_operation_duration_seconds` (histogram, label: `operation`: get/put/delete) — S3 latency
  - `kevent_relay_s3_errors_total` (counter, label: `operation`) — S3 failures
  - `kevent_relay_kafka_publish_errors_total` (counter) — result-event publish failures

---

### [v0.4.5] — 2026-03-30

#### Fixed
- Sync-direct requests (gateway in direct-proxy mode, no `syncTopic`) arriving on paths like `/v1/audio/transcriptions` were caught by the relay's `"/"` catch-all and rejected with 400 "missing job_id". KafkaSource always POSTs to exactly `"/"`, so any other path is now reverse-proxied transparently to the local inference model (`inference.base_url`).

---

### [v0.4.4] — 2026-03-24

#### Fixed
- Use `context.Background()` when publishing failure result events and deleting the input file after inference errors. Prevents silent loss of result notifications when the Knative request context is cancelled (e.g. `timeoutSeconds` exceeded), which would leave the gateway waiting indefinitely

---

### [v0.4.3] — 2026-03-23

#### Added
- `InputEvent.Params` forwarded into the multipart request to the inference API
- `Params` (from request) merged with `extra_fields` (from config), request params take precedence

---

### [v0.3.0] — 2026-03-13

#### Added
- `ServeHTTPSync` endpoint (`POST /sync`): priority CloudEvent handler that sets an in-pod `syncPriority` flag for the duration of the job
- `syncPriority atomic.Int32` field on `Relay`

#### Changed
- `ServeHTTP` (async handler): returns `503 Service Unavailable` when a sync job is in progress, causing KafkaSource to retry with backoff — giving sync jobs first access to the GPU
- Removed `SyncProxy` (`/v1/*` direct proxy): sync requests now arrive via the dedicated KafkaSource → `/sync` path

---

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
- Docker image moved to `ghcr.io/ia-generative/kevent-ai/relay`

### [v0.2.4] — 2026-01-XX

#### Added
- SASL/TLS Kafka authentication (`internal/kafka/auth.go`)
- `KAFKA_SASL_USERNAME` / `KAFKA_SASL_PASSWORD` / `KAFKA_CA_CERT_PATH` env vars

---

## Helm chart (kevent-gateway)

### [0.5.11] — 2026-04-02

#### Changed
- `appVersion` / `image.tag` → `v0.4.18`

---

### [0.5.10] — 2026-04-02

#### Changed
- `appVersion` / `image.tag` → `v0.4.17`

---

### [0.5.9] — 2026-04-02

#### Changed
- `appVersion` / `image.tag` → `v0.4.16`

---

### [0.5.8] — 2026-04-02

#### Changed
- `appVersion` / `image.tag` → `v0.4.15`

---

### [0.5.7] — 2026-04-02

#### Changed
- `appVersion` / `image.tag` → `v0.4.14`

---

### [0.5.6] — 2026-04-02

#### Changed
- `appVersion` / `image.tag` → `v0.4.13`

---

### [0.5.5] — 2026-04-02

#### Changed
- `appVersion` / `image.tag` → `v0.4.12`

---

### [0.5.4] — 2026-04-02

#### Changed
- `appVersion` / `image.tag` → `v0.4.11` (already released)

---

### [0.5.3] — 2026-04-01

#### Added
- `config.existingConfigMap` : référencer une ConfigMap existante au lieu de laisser le chart en créer une
- `input_topic` / `result_topic` conditionnels dans le template ConfigMap (services sync-direct)

#### Changed
- `appVersion` / `image.tag` → `v0.4.11`

---

### [0.5.2] — 2026-04-01

#### Added
- `config.existingConfigMap` option (intégré dans 0.5.3)

---

### [0.5.1] — 2026-03-31

#### Changed
- `image.tag` bumped to `v0.4.10` (gateway sync-direct metrics fix)
- `appVersion` bumped to `v0.4.10`

---

### [0.5.0] — 2026-03-30

#### Added
- `metrics.serviceMonitor.enabled` — crée un `ServiceMonitor` (Prometheus Operator) pointant sur `GET /metrics` du gateway
- Valeurs disponibles : `namespace`, `interval` (défaut: 30s), `scrapeTimeout` (défaut: 10s), `additionalLabels`, `relabelings`, `metricRelabelings`

---

### [0.3.0] — 2026-03-13

#### Added
- `services[].syncTopic` — optional priority Kafka topic for sync-over-Kafka routing
- ConfigMap template now renders `sync_topic` when set

#### Changed
- `appVersion` and `image.tag` updated to `0.3.0`

---

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
