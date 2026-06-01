# Architecture overview

kevent-ai consists of two independent components deployed separately:

- **Gateway** — HTTP server that accepts requests, routes them, and returns results
- **Relay** — standalone Kafka pull consumer deployed alongside the inference pod, processing async jobs

## Infrastructure dependencies

![Architecture overview](overview.drawio.png)

## Component responsibilities

### Gateway

- Accepts HTTP requests from clients
- Routes to the correct service based on `service_type`, `model`, and path
- Enforces per-consumer, per-service rate limits (Redis fixed-window)
- Uploads input files to S3
- Persists job records in Redis (configurable TTL per status via `lifecycle.job_ttl`)
- Publishes `InputEvent` messages to Kafka
- Consumes `ResultEvent` messages and notifies clients
- Proxies sync requests directly to `inference_url`
- LLM proxy for JSON requests: provider translation, response caching, consumer token tracking

### Relay

- Runs as a separate container in the inference Deployment (not a sidecar)
- Pulls `InputEvent` messages from Kafka via a long-running consumer loop (kafka-go Reader, manual commit)
- Waits for the local inference service to be ready before consuming
- Downloads input from S3
- Calls the local inference model
- Uploads results to S3
- Publishes `ResultEvent` to Kafka
- Annotates pod with `pod-deletion-cost` during active inference to prevent eviction
- Scaled by KEDA on Kafka consumer lag (`lagThreshold: 1`, `minReplicaCount: 0`)

## Data flow

See [Request flows](request-flows.md) for detailed sequence diagrams for each mode.

## Service registry

The gateway is entirely config-driven. See [Service registry](service-registry.md).

## LLM proxy

JSON requests to services with `provider` set go through a built-in LLM proxy with provider translation (OpenAI ↔ Anthropic), response caching, and consumer metrics. See [LLM proxy](llm-proxy.md).

## Rate limiting

Per-consumer fixed-window rate limiting across all request modes. See [Rate limiting](rate-limiting.md).
