# Request flows

## Async mode

`POST /jobs/{service_type}` — fire and forget, poll for result.

![Async flow](async-flow.drawio.png)

**Client polling:**

```
GET /jobs/{service_type}/{job_id}
→ { status: "pending" | "completed" | "failed", result: {...} }
```

---

## Sync-over-Kafka mode

`POST /v1/*` multipart with `sync_topic` configured. The connection stays open; result is returned inline.

![Sync-over-Kafka flow](sync-kafka-flow.drawio.png)

---

## Sync direct proxy mode

`POST /v1/*` JSON body, or multipart without `sync_topic`.

```
Client
  │  POST /v1/audio/transcriptions
  │  { "model": "whisper-large-v3", ... }
  ▼
Gateway
  └── HTTP proxy ──────────────────────────────► InferenceService
                                                 (inference_url + original path)
                                                          │
                                                          ▼
                                                 Response proxied back
```

---

## Routing decision

![Routing decision](routing.drawio.png)
