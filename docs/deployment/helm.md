# Helm chart

The gateway is packaged as a Helm chart and published to GitHub Pages.

## Install

```bash
helm repo add kevent https://ia-generative.github.io/kevent-ai
helm repo update

helm install kevent-gateway kevent/kevent-gateway -f values.yaml
```

## Upgrade

```bash
helm upgrade kevent-gateway kevent/kevent-gateway -f values.yaml
```

## Install from local sources

```bash
helm upgrade --install kevent-gateway ./helm/gateway -f values.yaml
```

## Minimal `values.yaml`

```yaml
image:
  tag: "v0.13.0"

config:
  s3:
    endpoint: "https://s3.fr-par.scw.cloud"
    region: "fr-par"
    bucket: "my-kevent-jobs"
  redis:
    addr: "redis:6379"
  kafka:
    brokers:
      - "kafka:9092"

secrets:
  s3AccessKey: "my-access-key"
  s3SecretKey: "my-secret-key"
```

## Redis HA

The chart deploys Redis with Redis-HA (HAProxy front-end) by default. Redis is required for job state, rate limiting, and result caching.

## ConfigMap hot reload

The chart supports `configmap-reload` sidecar to trigger `POST /-/reload` automatically when the ConfigMap changes:

```yaml
configReloader:
  enabled: true
  image: ghcr.io/jimmidyson/configmap-reload:v0.14.0
```

## Ingress

```yaml
ingress:
  enabled: true
  className: nginx
  host: kevent.example.com
  tls:
    enabled: true
    secretName: kevent-tls
```

## Extra environment variables

```yaml
extraEnvVars:
  - name: ENCRYPTION_KEY
    valueFrom:
      secretKeyRef:
        name: kevent-encryption
        key: key
```

## Apply Strimzi Kafka users

```bash
kubectl apply -f examples/kafka-users.yaml -n <kafka-namespace>
```

See `examples/kafka-users.yaml` in the repository root. Adjust the `strimzi.io/cluster` label and namespace to match your Strimzi installation.

## Deploy relay (Deployment + KEDA)

The relay runs as a standalone Deployment alongside the inference container. Apply the manifests from the repository:

```bash
# ConfigMap
kubectl apply -f k8s/relay-cm.yaml

# Deployment (2 containers: inference model + relay)
kubectl apply -f k8s/deployment-transcription.yaml

# KEDA ScaledObject (scales on Kafka consumer lag)
kubectl apply -f k8s/keda-transcription.yaml
```

The KEDA ScaledObject uses `lagThreshold: 1` and `minReplicaCount: 0` — pods scale to zero when no jobs are pending. Replace the `<placeholder>` values (broker address, secret names, image tags) before applying.
