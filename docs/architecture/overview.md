# Architecture overview

kevent-ai consists of two independent components deployed separately:

- **Gateway** — HTTP server that accepts requests, routes them, and returns results
- **Relay** — sidecar that runs inside each Knative InferenceService pod, consuming Kafka jobs

## Infrastructure dependencies

![Architecture overview](overview.drawio.png)

## Component responsibilities

### Gateway

- Accepts HTTP requests from clients
- Routes to the correct service based on `service_type`, `model`, and path
- Uploads input files to S3
- Persists job records in Redis (TTL 72h)
- Publishes `InputEvent` messages to Kafka
- Consumes `ResultEvent` messages and notifies clients
- Proxies sync requests directly to `inference_url`

### Relay

- Runs as a sidecar container in the InferenceService pod
- Consumes `InputEvent` from Kafka via KafkaSource (Knative Eventing)
- Downloads input from S3
- Calls the local inference model on `127.0.0.1:9000`
- Uploads results to S3
- Publishes `ResultEvent` to Kafka

## Data flow

See [Request flows](request-flows.md) for detailed sequence diagrams for each mode.

## Service registry

The gateway is entirely config-driven. See [Service registry](service-registry.md).

## Priority mechanism

High-priority consumers can preempt async jobs. See [Priority routing](priority.md).
