# KeventGatewayKafkaPublishErrors

**Severity:** critical
**Component:** Gateway

## Meaning

The gateway failed to publish one or more input events to Kafka in the last 5 minutes. Affected jobs are immediately marked `failed` in Redis.

## Impact

Async job submissions (`POST /jobs/{service_type}`) return 500. Files are uploaded to S3 but never processed.

## Diagnosis

```bash
# Total publish errors by topic
increase(kevent_kafka_publish_errors_total[5m])

# Gateway logs
kubectl logs -l app.kubernetes.io/name=kevent-gateway -n <namespace> | grep "kafka publish failed"

# Check broker connectivity from the gateway pod
kubectl exec -it <gateway-pod> -n <namespace> -- nc -zv <broker-host> <broker-port>
```

Common causes:
- Kafka broker unreachable (network policy, broker restart)
- SASL credentials expired or rotated
- Topic does not exist (`AllowAutoTopicCreation: false` — topic must be pre-created)
- Broker disk full / under-replicated

## Mitigation

1. Verify broker address in gateway config (`kafka.brokers`)
2. Check SASL credentials — rotate secret if needed
3. Confirm topic exists: `kafka-topics.sh --list --bootstrap-server <broker>`
4. Check Strimzi KafkaUser ACLs (`examples/kafka-users.yaml`) — `Write` on `jobs.*` required
5. Failed jobs remain in Redis with status `failed` for TTL duration — clients must resubmit
