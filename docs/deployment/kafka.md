# Kafka configuration

## Broker

kevent supports any Kafka broker reachable from the cluster. Configure the address in `kafka.brokers`.

## SASL/TLS configuration

```yaml
kafka:
  brokers:
    - "kafka-bootstrap.<kafka-namespace>.svc.cluster.local:9093"
  sasl:
    mechanism: "SCRAM-SHA-512"  # or PLAIN, SCRAM-SHA-256
    username: "${KAFKA_USERNAME}"
    password: "${KAFKA_PASSWORD}"
  tls:
    enabled: true
    ca_cert_path: "/etc/ssl/certs/kafka-ca.crt"
```

## Strimzi KafkaUsers

Apply in the namespace where your Strimzi Kafka cluster is managed:

```bash
kubectl apply -f examples/kafka-users.yaml -n <kafka-namespace>
```

See `examples/kafka-users.yaml` in the repository root for the full manifest.

### `kevent-gateway`

- Write/Read on `jobs.*` topics
- Read on `kevent-gateway*` consumer groups

### `kevent-relay`

- Read on `jobs.*` topics
- Write on `jobs.*` topics (result events)

## Topic naming

| Topic | Producer | Consumer |
|---|---|---|
| `jobs.{model}.input` | Gateway (async submit) | Relay pull consumer |
| `jobs.{model}.priority` | Gateway (priority submit) | Relay pull consumer (dedicated deployment) |
| `jobs.{model}.results` | Relay | Gateway ConsumerManager |

## Relay consumer configuration

The relay is configured via `relay/config.yaml` (or the `kevent-relay` ConfigMap in Kubernetes). Key Kafka fields:

```yaml
kafka:
  brokers:
    - "kafka-bootstrap.<kafka-namespace>.svc.cluster.local:9093"
  input_topic: "jobs.whisper-large-v3.input"
  consumer_group: "kevent-relay-whisper"
  sasl:
    mechanism: "SCRAM-SHA-512"
    username: "${KAFKA_USERNAME}"
    password: "${KAFKA_PASSWORD}"
  tls:
    enabled: true
    ca_cert_path: "/etc/ssl/certs/kafka-ca.crt"
```

For priority routing, deploy a second relay instance with `input_topic: jobs.{model}.priority` and a distinct `consumer_group`.

## Secret hygiene

Strimzi-generated secrets must not have trailing newlines in values. Verify:

```bash
kubectl get secret kevent-relay-kafka -n default \
  -o jsonpath='{.data.sasl-type}' | base64 -d | xxd
```

The output must end with `5332 302d 3531 32` (`SCRAM-SHA-512`) — no `0a` byte at the end.
