---
layout: default
title: KeventGatewaySyncJobsInFlightHigh
---

# KeventGatewaySyncJobsInFlightHigh

**Severity:** warning
**Component:** Gateway

## Meaning

More than the configured threshold of sync-over-Kafka connections are open simultaneously in the gateway. Each open connection is a client waiting for an inference result.

## Impact

High in-flight count indicates saturation. New sync requests may time out if relay pods cannot keep up. Knative may be scaling up pods (normal), or the autoscaler is lagging.

## Diagnosis

```bash
# Current in-flight connections
kevent_sync_jobs_in_flight

# Sync wait duration distribution (check for long waits)
histogram_quantile(0.95, rate(kevent_sync_wait_duration_seconds_bucket[5m]))

# Check relay pod count
kubectl get pods -n <namespace> | grep predictor

# Check relay sync priority (1 = sync job active on that pod)
kevent_relay_sync_priority
```

Common causes:
- Inference is slow (large files, complex models) — normal spike during heavy load
- Relay pods not scaling fast enough (check Knative autoscaler config)
- Relay pods crashing mid-job — check relay logs

## Mitigation

1. If inference is legitimately slow: increase `scaleTarget` or reduce `containerConcurrency` on the KServe InferenceService to trigger faster scale-out
2. If relay pods are failing: check relay logs and Knative pod events
3. If the spike is transient: no action needed — Knative will scale out and drain the queue
