---
layout: default
title: KeventRelayInferenceSlow
---

# KeventRelayInferenceSlow

**Severity:** warning
**Component:** Relay sidecar

## Meaning

The 95th percentile inference duration exceeds the configured threshold (default: 120 s). This measures the time the relay spends calling the local inference model.

## Impact

- Sync-over-Kafka clients are held open longer, increasing `kevent_sync_jobs_in_flight`
- Async jobs complete later than expected
- Risk of Knative `timeoutSeconds` being hit, causing relay context cancellation

## Diagnosis

```bash
# p50 / p95 / p99 inference duration by service_type
histogram_quantile(0.95,
  sum by (service_type, le) (
    rate(kevent_relay_inference_duration_seconds_bucket[10m])
  )
)

# Input file size distribution (large files = expected slow inference)
histogram_quantile(0.95,
  sum by (service_type, le) (
    rate(kevent_relay_input_size_bytes_bucket[10m])
  )
)

# GPU utilization (if available via DCGM exporter)
# DCGM_FI_DEV_GPU_UTIL
```

Common causes:
- Large input files (audio > 10 min, high-resolution images)
- GPU contention (multiple concurrent jobs on one pod)
- Model cold start after scale-from-zero
- Inference model degraded (OOM pressure, throttling)

## Mitigation

1. If caused by large inputs: expected behaviour — consider raising the alerting threshold or use `max_file_size_mb` to limit input size
2. If GPU contention: lower `containerConcurrency` on the InferenceService to prevent concurrent GPU usage
3. If cold start: configure Knative `minReplicas: 1` to avoid scale-to-zero for latency-sensitive services
4. Check model pod resource limits — GPU memory pressure can slow inference significantly
