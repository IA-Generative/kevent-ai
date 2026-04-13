# Kafka & SASL configuration

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

See [`examples/kafka-users.yaml`](../../examples/kafka-users.yaml) for the full manifest.

### `kevent-gateway`

- Write/Read on `jobs.*` topics
- Read on `kevent-gateway*` consumer groups

### `kevent-relay`

- Read on `jobs.*` topics
- Write on `jobs.*` topics
- Read + Describe + **Delete** on `inference-*` groups

!!! warning "Delete ACL required"
    The `Delete` ACL on `inference-*` groups is required by the Knative controller for ConsumerGroup finalization. Without it, KafkaSources will fail to clean up.

## Secret hygiene

Strimzi-generated secrets must not have trailing newlines in values. The Knative KafkaSource controller does exact string comparison on `sasl-type` — a `\n` suffix causes:

```
[protocol SASL_SSL] unsupported SASL mechanism
```

Verify:

```bash
kubectl get secret kevent-relay-kafka -n default \
  -o jsonpath='{.data.sasl-type}' | base64 -d | xxd
```

The output must end with `5332 302d 3531 32` (`S` `C` `R` `A` `M` `-` `S` `H` `A` `-` `5` `1` `2`) — no `0a` byte at the end.

## KafkaSources

Each service type requires two KafkaSources in the InferenceService namespace:

| Source | Topic | Relay endpoint | Purpose |
|---|---|---|---|
| `kafka-source-{model}` | `jobs.{model}.input` | `POST /` | Normal async jobs |
| `kafka-source-{model}-sync` | `jobs.{model}.sync` | `POST /sync` | Sync-over-Kafka (priority) |
| `kafka-source-{model}-priority` | `jobs.{model}.priority` | `POST /sync` | SA priority async jobs |

The `sync` and `priority` sources route to `POST /sync`, which sets `syncPriority++` and defers normal async processing.

## Topic naming

| Topic | Producer | Consumer |
|---|---|---|
| `jobs.{model}.input` | Gateway (async submit) | Relay via KafkaSource |
| `jobs.{model}.sync` | Gateway (sync-over-Kafka) | Relay via KafkaSource |
| `jobs.{model}.priority` | Gateway (priority submit) | Relay via KafkaSource |
| `jobs.{model}.results` | Relay | Gateway ConsumerManager |
