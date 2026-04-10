# Priority routing

Priority routing lets SA (service account) consumers preempt normal async jobs, ensuring low-latency processing for premium workloads.

## How it works

### Gateway side

When a request carries the priority header (configurable via `server.priority_header`), the gateway publishes the `InputEvent` to the `priority_topic` instead of the normal `input_topic`.

```
Normal async:   Kafka jobs.<model>.input    → relay POST /
Priority async: Kafka jobs.<model>.priority → relay POST /sync
```

### Relay side

The relay's `POST /sync` handler sets `syncPriority++` for the duration of the job. Concurrent `POST /` (normal async) handlers check this counter:

- `syncPriority > 0` → return `503 Service Unavailable`
- KafkaSource retries after `backoffDelay`
- Once the priority job finishes, `syncPriority--` and async jobs resume

This works across pod scale-out: each pod independently tracks its own priority state. No shared state across pods is needed.

## Configuration

```yaml
server:
  priority_header: "X-Priority"   # header name to check on incoming requests

services:
  - type: audio
    model: "whisper-large-v3"
    priority_topic: jobs.whisper-large-v3.priority   # omit to disable priority routing
    ...
```

### Knative KafkaSource for priority

Add a KafkaSource routing the priority topic to `POST /sync` on the relay:

```yaml
apiVersion: sources.knative.dev/v1beta1
kind: KafkaSource
metadata:
  name: kafka-source-whisper-priority
spec:
  topics:
    - jobs.whisper-large-v3.priority
  consumerGroup: kevent-relay-priority
  sink:
    ref:
      apiVersion: serving.knative.dev/v1
      kind: Service
      name: kevent-transcription
  delivery:
    backoffDelay: PT2S
    backoffPolicy: exponential
  extensions:
    path: /sync
```

## Consumer identification and isolation

When `server.consumer_header` is set (e.g. `X-Consumer-Username`, injected by APISIX after auth), the gateway:

1. Stores `consumer_name` in the job record (Redis JSON)
2. Maintains `consumer:{name}:jobs` sorted set (score = Unix timestamp, same TTL as job)
3. Exposes `GET /jobs` to list a consumer's jobs (paginated, most-recent-first)
4. Enforces ownership on `GET /jobs/{service_type}/{id}`: if the header is present, the job's `consumer_name` must match — returns `404` on mismatch
5. Increments `kevent_jobs_by_consumer_total{mode, service_type, model, consumer}`

```yaml
server:
  consumer_header: "X-Consumer-Username"   # set by APISIX after authentication
```

### Ownership check behaviour

| `consumer_header` configured | Header in request | Result |
|---|---|---|
| no | — | No check — auth-less deployments, all callers trusted |
| yes | absent | No check — admin/internal calls bypass isolation |
| yes | present + matches job | `200 OK` |
| yes | present + mismatch | `404` — no information leak about other consumers' jobs |

### Security note

Brute-force by job ID is not feasible — IDs are UUID v4 (2¹²² combinations). The ownership check adds defence-in-depth for authenticated deployments: even if a consumer somehow obtained another consumer's UUID, the gateway returns `404`.
