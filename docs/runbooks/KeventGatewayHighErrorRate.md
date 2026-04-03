---
layout: default
title: KeventGatewayHighErrorRate
---

# KeventGatewayHighErrorRate

**Severity:** warning / critical
**Component:** Gateway

## Meaning

More than 5% (warning) or 20% (critical) of gateway requests are returning HTTP 5xx errors over the last 5 minutes.

## Impact

Clients are receiving errors. Async job submissions or sync inference calls are failing.

## Diagnosis

```bash
# Error rate by mode and service_type
rate(kevent_requests_total{status=~"5.."}[5m])

# Check gateway pod logs
kubectl logs -l app.kubernetes.io/name=kevent-gateway -n <namespace> --tail=100

# Check which status codes are returned
sum by (status, mode, service_type) (
  rate(kevent_requests_total[5m])
)
```

Common causes:
- S3 unavailable → check `kevent_s3_errors_total`
- Kafka unavailable → check `kevent_kafka_publish_errors_total`
- Redis unavailable → gateway logs will show connection errors
- Upstream inference service returning errors (sync mode)

## Mitigation

1. Check S3, Kafka, and Redis connectivity from the gateway pod
2. If S3 is down: async jobs will fail at upload — check bucket and credentials
3. If Kafka is down: async submissions fail at enqueue — check broker reachability
4. If Redis is down: sync-over-Kafka jobs will stall — check Redis HA haproxy endpoint
5. Scale the gateway deployment if the issue is resource exhaustion
