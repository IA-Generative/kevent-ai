# Priority routing

Priority routing lets SA (service account) consumers bypass the normal async queue, ensuring low-latency processing for premium workloads.

## How it works

When a request carries the priority header (`server.priority_header`), the gateway publishes the `InputEvent` to `priority_topic` instead of `input_topic`.

```
Normal async:   jobs.<model>.input    → relay consumer group (standard)
Priority async: jobs.<model>.priority → relay consumer group (dedicated deployment)
```

A dedicated relay Deployment — configured to consume `priority_topic` with its own consumer group — processes priority jobs independently of the normal async queue. Because each relay is a long-running pull consumer, there is no shared `syncPriority` flag: isolation is achieved at the Kafka consumer group level.

## Configuration

```yaml
server:
  priority_header: "X-Priority"   # header name to check on incoming requests

services:
  - type: audio
    model: "whisper-large-v3"
    input_topic: jobs.whisper-large-v3.input
    priority_topic: jobs.whisper-large-v3.priority   # omit to disable priority routing
    result_topic: jobs.whisper-large-v3.results
```

Deploy a second relay instance with `kafka.input_topic: jobs.whisper-large-v3.priority` and a distinct `kafka.consumer_group` to consume priority jobs.

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
