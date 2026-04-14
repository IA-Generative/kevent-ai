# Service registry

The gateway is entirely config-driven. Adding a new model or service type requires only a YAML block in `config.yaml` — no Go code change.

## Service entry fields

```yaml
services:
  - type: audio                        # service type (used in /jobs/{service_type})
    model: "whisper-large-v3"          # OpenAI "model" field value
    default: true                      # fallback when request omits model field

    # Sync / OpenAI-compatible
    operations:
      transcription:
        - "/v1/audio/transcriptions"   # all paths indexed; first used for async
      translation:
        - "/v1/audio/translations"
    inference_url: "http://..."        # backend base URL (path appended at runtime)
    sync_topic: jobs.whisper.sync      # omit → use direct proxy for multipart

    # Async / Kafka
    input_topic: jobs.whisper.input
    result_topic: jobs.whisper.results
    priority_topic: jobs.whisper.priority  # optional — SA/priority consumers

    # File validation
    accepted_exts: [".mp3", ".wav", ".m4a", ".ogg", ".flac"]
    max_file_size_mb: 500

    # Backend authentication — sync-direct only (optional)
    inference_headers:
      Authorization: "Bearer ${WHISPER_API_KEY}"
```

## Model resolution order

When a request omits the `model` field:

1. Single model registered for the type/path → auto-selected
2. Model marked `default: true` → used as fallback
3. Error listing available models

## Operations

The `operations` map replaces the old flat `openai_paths` list. Each key is an operation name; the value is a list of URL paths.

- **All paths** are indexed for sync routing (`POST /v1/*`)
- **First path** of the selected operation is embedded in the `InputEvent.InferenceURL` for async jobs
- The `operation` form field selects which operation to use for async submission (required when a model has multiple operations)

```bash
# Async: specify operation when model has multiple
curl -X POST /jobs/audio \
  -F file=@audio.wav \
  -F operation=translation

# Sync: operation is implicit from the URL path
curl -X POST /v1/audio/translations -F file=@audio.wav
```

## Multiple models per type

Multiple service entries may share the same `type` with different `model` values:

```yaml
services:
  - type: audio
    model: "whisper-large-v3"
    default: true
    ...

  - type: audio
    model: "whisper-large-v3-turbo"
    ...
```

The gateway routes by the `model` field in the request. The `default: true` flag designates the fallback.

## Path patterns with `{model}`

Paths can embed the model name directly in the URL:

```yaml
operations:
  infer:
    - "/v2/models/{model}/infer"
```

The gateway extracts the model name from the URL segment and routes accordingly. No `model` field is required in the request body.

## Backend authentication (`inference_headers`)

For sync-direct services whose backend requires authentication, add `inference_headers`:

```yaml
- type: reranker
  model: "bge-reranker-v2-m3"
  operations:
    rerank:
      - "/v1/rerank"
  inference_url: "http://kevent-reranker-predictor.svc.cluster.local"
  inference_headers:
    Authorization: "Bearer ${RERANKER_API_KEY}"
```

Headers are injected on every outgoing request to the backend. Values support `${VAR}` expansion. Config headers override client headers with the same name.

!!! note
    `inference_headers` only applies to **sync-direct** proxy. Async and sync-over-Kafka jobs run via the relay sidecar (local `127.0.0.1:9000`) and are unaffected.

## Hot reload

The service registry is reloaded atomically via `POST /-/reload`. The HTTP router is swapped, Kafka consumers are reconciled (stopped for removed topics, started for new ones). Infrastructure (S3, Redis, Kafka connection) is not re-initialised.
