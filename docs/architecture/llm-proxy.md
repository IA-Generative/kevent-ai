# LLM proxy

The gateway includes a built-in LLM proxy that forwards JSON requests to any OpenAI-compatible or Anthropic backend, with optional response caching and consumer metrics.

Activated per service by setting `provider` in `config.yaml`. When `provider` is set, JSON requests to that service's paths go through the proxy instead of the bare direct-proxy.

## Request flow

```
Client  POST /v1/chat/completions  {"model": "my-alias", ...}
  │
  ▼
Gateway — LLM proxy
  ├── Cache lookup (Redis, SHA-256 key)     ─── HIT → return cached response
  │                                                    X-Cache: HIT
  ├── MISS → rewrite model alias → backend_model
  ├── Build provider request (translate if anthropic)
  ├── Forward to inference_url
  ├── Translate response back to OpenAI format
  ├── Emit metrics + track consumer tokens
  ├── Write response to client              X-Cache: MISS
  └── Async cache-fill (5 s timeout, goroutine)
```

## Providers

| `provider` | Protocol | Auto-translation |
|---|---|---|
| `openai` | OpenAI Chat Completions API | none |
| `anthropic` | Anthropic Messages API | OpenAI ↔ Messages API |
| `ollama` | Ollama `/api/chat` | none (OpenAI-compatible) |
| `passthrough` | forward verbatim | none |

Use `passthrough` for vLLM, LiteLLM, or any backend that speaks the OpenAI API natively.

### Anthropic translation

When `provider: anthropic` is set, the gateway translates bidirectionally:

- **Request** — OpenAI `messages[]` (including `system` role) → Anthropic `messages[]` + `system` string; `max_tokens` / `max_completion_tokens` → `max_tokens`; `stop` (string or array) → `stop_sequences[]`
- **Response** — Anthropic `content[].text` blocks → OpenAI `choices[0].message.content`; `stop_reason` → `finish_reason`; `input_tokens` / `output_tokens` → `prompt_tokens` / `completion_tokens`

Tool-use fields (`tools`, `tool_choice`) are forwarded as-is on the request side; `tool_use` stop reason maps to `tool_calls` finish reason.

## Model aliases

The `model` field in the service config acts as the client-facing alias. Set `backend_model` to rewrite the `model` field in the request body before forwarding:

```yaml
- type: llm
  model: "llama3"            # clients send this
  provider: passthrough
  backend_model: "meta-llama/Meta-Llama-3-8B-Instruct"   # backend receives this
  inference_url: "http://vllm.default.svc.cluster.local:8000"
```

Cache lookup uses the alias, so cache keys are stable even if `backend_model` changes.

## Response caching

Responses are cached in Redis, keyed on SHA-256 of a canonical subset of the request body:

**Cacheable fields** (included in key): `model`, `messages`, `system`, `temperature`, `top_p`, `stop`, `tools`, `tool_choice`

**Excluded fields** (ignored, do not bust the cache): `user`, `metadata`, any unknown fields

**Bypass conditions:**
- `stream: true` in the request body
- `Cache-Control: no-cache` (or any directive containing `no-cache`) in the request header
- Non-2xx response from the backend
- `response_cache_ttl: 0` (disabled)

Cache-fill runs in a background goroutine after the response is written to the client — it never adds latency to the HTTP response. The goroutine has a 5-second timeout.

### Configuration

```yaml
- type: llm
  model: "gpt-4o"
  provider: openai
  response_cache_ttl: 3600   # seconds; 0 = disabled
  operations:
    chat:
      - "/v1/*"              # wildcard: all OpenAI-compatible paths
  inference_url: ""          # empty = defaults to https://api.openai.com
  inference_headers:
    Authorization: "Bearer ${OPENAI_API_KEY}"
```

## Dynamic wildcard routing

Operation paths ending with `/*` register as chi wildcard routes:

```yaml
operations:
  chat:
    - "/v1/*"
```

This matches `/v1/chat/completions`, `/v1/embeddings`, `/v1/models`, and any other path under `/v1/`. Exact paths always take priority over wildcards in chi's radix tree, so other services with explicit paths (e.g. `/v1/audio/transcriptions`) are unaffected.

## Consumer metrics

When `metrics.top_consumers` is set to a positive integer and `server.consumer_header` is configured, token usage is tracked per consumer in Redis sorted sets:

```
Key: llm:consumer:tokens:{user_type}:{prompt|completion}
Member: consumer name
Score: cumulative token count
```

A background goroutine refreshes the Prometheus `kevent_llm_consumer_tokens_top` gauge every 60 seconds (and immediately at startup), exposing the top-N consumers by token usage. This avoids unbounded label cardinality — only the top-N appear in Prometheus, not all 100k+ consumers.

For monitoring all consumers independently, query Redis directly:

```bash
redis-cli ZREVRANGEBYSCORE llm:consumer:tokens:sa:prompt +inf -inf WITHSCORES
```

## Prometheus metrics

| Metric | Labels | Description |
|---|---|---|
| `kevent_llm_requests_total` | `service_type, model, provider, user_type, status` | Request count |
| `kevent_llm_request_duration_seconds` | `service_type, model, provider, user_type` | Latency histogram |
| `kevent_llm_tokens_total` | `service_type, model, user_type, type` | Token counter (`prompt` / `completion`) |
| `kevent_llm_tokens_per_request` | `service_type, model, user_type` | Token distribution histogram |
| `kevent_llm_consumer_tokens_top` | `consumer, user_type, type` | Top-N consumer token gauge |
| `kevent_cache_hits_total` | `service_type, model` | Cache hit counter |
| `kevent_cache_misses_total` | `service_type, model` | Cache miss counter |
| `kevent_cache_errors_total` | `service_type, model, op` | Cache error counter (`key`/`get`/`set`) |

### Example queries

```promql
# Cache hit ratio per model
sum by (model) (rate(kevent_cache_hits_total[5m]))
/
sum by (model) (rate(kevent_llm_requests_total[5m]))

# p99 token count per request
histogram_quantile(0.99, sum by (le, model) (
  rate(kevent_llm_tokens_per_request_bucket[1h])
))

# Top consumers by prompt tokens (from the top-N gauge)
topk(10, kevent_llm_consumer_tokens_top{type="prompt"})
```
