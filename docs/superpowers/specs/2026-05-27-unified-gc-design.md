# Unified GC — Design Spec

**Date:** 2026-05-27
**Branch:** fix/webhook-result-cleanup

## Problem

The current stale-job GC only handles one case: pending jobs stuck too long (marks them failed, deletes their S3 `input_ref`). Two categories of S3 orphans are never cleaned:

1. **Orphan `result_ref`** — completed jobs whose result was never fetched (no webhook, client never polled), or jobs where `persists_result=true`. When the Redis TTL expires, the S3 result file remains indefinitely.
2. **Orphan `input_ref`** — input files for completed/failed jobs that slipped through (e.g. relay crashed mid-processing).

## Solution

Rename and extend the stale-job GC into a **unified GC** with two phases per tick, controlled by a single `enabled` flag.

## Config

New block under `lifecycle.gc` in `config.yaml` and Helm values:

```yaml
lifecycle:
  gc:
    enabled: false        # master switch; default off (hot-reload safe)
    interval: "15m"       # tick frequency; default 15m
    orphan_min_age: "5m"  # min S3 object age before orphan check; default 5m
```

`redis.pending_max_age_hours` is **renamed** to `redis.pending_max_age` (duration string, e.g. `"2h"`). The old integer field is removed; `config.Load` validates the new field with `parseDuration`.

Hot-reload updates `enabled`, `interval`, `orphan_min_age`, and `pending_max_age` via atomics without pod restart.

## GC Phases

### Phase 1 — Stale-pending sweep (existing, renamed)

- Scan `queue:*` Redis keys for jobs pending longer than `redis.pending_max_age`
- Mark each as failed (`"stale: pending too long"`)
- Delete their S3 `input_ref`
- Skipped when `pending_max_age` is unset or zero

### Phase 2 — Orphan S3 cleanup (new)

**Safeguard first:**
1. `Ping` Redis — if error: log `"orphan GC: Redis unavailable, skipping S3 cleanup"` and abort phase 2.

**Inventory:**
2. `ListObjectsV2` S3 (paginated) — collect all objects in the bucket.
3. Group by first key segment (= jobID): `{jobID}/input.wav`, `{jobID}/result.json` → `map[jobID][]objectKey` with the oldest `LastModified` per group.
4. Filter out job IDs whose oldest object is younger than `orphan_min_age` — these may be in-flight uploads not yet committed to Redis.

**Cross-check:**
5. `MGET job:{id}...` for all remaining job IDs — if Redis returns an error: log `"orphan GC: Redis inventory unreliable, skipping S3 cleanup"` and abort phase 2.
6. Job IDs absent from Redis → orphaned.

**Cleanup:**
7. For each orphaned job ID, delete all its S3 objects (input + result). Log each deletion.

## New code

| File | Change |
|---|---|
| `internal/config/config.go` | Add `GCConfig` struct; embed in `LifecycleConfig` |
| `internal/storage/s3.go` | Add `ListJobObjects() (map[string]s3JobObjects, error)` — paginates `ListObjectsV2`, returns `map[jobID]{keys []string, oldestModTime time.Time}` |
| `internal/storage/redis.go` | Add `JobsExistBatch(ctx, ids []string) (map[string]bool, error)` — single `MGET`, returns presence map |
| `cmd/gateway/main.go` | Replace stale-job goroutine with unified GC goroutine; interval from atomic; `enabled` guard; phase 2 logic |
| `config.yaml` | Add `lifecycle.gc` block (commented defaults) |
| `helm/gateway/values.yaml` | Add `lifecycle.gc` block |
| `helm/gateway/templates/configmap.yaml` | Render `lifecycle.gc` fields |
| `cmd/gateway/main.go` `reloadFn` | Propagate `gc.enabled`, `gc.interval`, `gc.orphan_min_age` via atomics |

## S3 key format (reference)

- Input: `{jobID}/input{ext}` (e.g. `abc123/input.wav`)
- Result: `{jobID}/result.json`

Both share the same `{jobID}/` prefix, so grouping by first path segment gives a clean per-job view.

## Error handling

| Failure | Behaviour |
|---|---|
| Redis `Ping` fails | Skip phase 2, log error, continue phase 1 |
| Redis `MGET` fails | Skip S3 deletion for that batch, log error |
| S3 `ListObjectsV2` fails | Skip phase 2, log error |
| S3 `DeleteObject` fails | Log error, continue with other orphans |
| Phase 1 `SweepStalePendingJobs` fails | Log error, continue (existing behaviour) |

## Metrics

Reuse existing `gmetrics.AsyncStaleJobsSweptTotal` for phase 1.
No new metrics in this iteration — orphan counts go to structured logs only.

## Testing

- Unit test `ListJobObjects`: mock S3 paginator, verify grouping and age filtering.
- Unit test `JobsExistBatch`: mock Redis MGET, verify presence map and error path.
- Integration test for phase 2: seed S3 with objects, some with Redis keys, some without; run GC; verify only orphans deleted.
