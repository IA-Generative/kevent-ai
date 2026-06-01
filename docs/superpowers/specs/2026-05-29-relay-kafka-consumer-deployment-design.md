# Design: Relay Kafka consumer + Deployment standard + KEDA

**Date:** 2026-05-29
**Branch:** worktree-feature+jobsink-async-direct-sync (PR #73)

## Contexte

Le relay actuel (sidecar KServe) reçoit des CloudEvents via HTTP push (KafkaSource). Problème : quand de nouveaux pods scalent up, les jobs déjà en file sont déjà bufferisés dans les pods existants — les nouveaux pods démarrent à vide.

Ce design remplace l'approche par un modèle pull direct depuis Kafka, avec KEDA pour le scaling.

## Architecture cible

```
Kafka ──pull──► relay container ──localhost:9000──► model container
                    │
                    └── KEDA ScaledObject (lag Kafka → replicas)

Gateway ──HTTP sync──► ClusterIP Service :9000 ──► pod (direct proxy)
```

- Pas de KafkaSource, pas de JobSink, pas de Knative Eventing
- Deployment Kubernetes standard (remplace KServe InferenceService)
- KEDA scale sur le lag du consumer group Kafka

## Section 1 — Relay : consumer loop

### Changements par rapport à PR73

PR73 implémentait un one-shot binary (lecture depuis `/etc/jobsink-event/event`).
Le relay redevient un processus long-running avec une boucle consumer Kafka.

### Flux par pod (Option A — 1 job à la fois)

```
FetchMessage(ctx)
  → ParseInputEvent(msg.Value)    ← message Kafka brut, pas de CloudEvent wrapper
  → SetDeletionCost(CostBusy)
  → Process(ctx, event)           ← S3 download → inference localhost:9000 → S3 upload → Kafka publish
  → SetDeletionCost(CostIdle)
  → CommitMessages(ctx, msg)
  → loop
```

CommitMessages uniquement après succès → pas de perte si le pod est tué en cours de job (le message est retraité par un autre pod après rebalance Kafka).

### Config relay — champs ajoutés

```yaml
kafka:
  input_topic: "${KAFKA_INPUT_TOPIC}"        # e.g. jobs.whisper-large-v3.input
  consumer_group: "${KAFKA_CONSUMER_GROUP}"  # e.g. inference-whisper-large-v3
```

### pod-deletion-cost

Le `lifecycle.PodAnnotator` (développé sur `develop`) est conservé :
- `CostBusy` (1000) : patch posé juste avant `Process()`
- `CostIdle` (0) : patch posé juste après `CommitMessages()`

Avec Option A (1 job à la fois), le compteur atomique `activeJobs` n'est pas nécessaire — set/unset direct autour du job.

RBAC requis : ServiceAccount + Role (patch pods) + RoleBinding dans le namespace du Deployment.

### Graceful shutdown

- `SIGTERM` → cancel du context → `FetchMessage` retourne une erreur → sortie propre
- Le job en cours se termine normalement (pas interrompu)
- `terminationGracePeriodSeconds` = durée max d'un job d'inférence (3600s pour Whisper)

### Health probe

Endpoint HTTP minimal `/health` (toujours 200 tant que le consumer loop tourne) sur `:8080`.
Utilisé pour `readinessProbe` et `livenessProbe` du container relay dans le Deployment.

## Section 2 — Deployment Kubernetes standard

### Structure du pod

```
Deployment: whisper-large-v3
  containers:
    - name: model          # ghcr.io/ia-generative/whisper-api:latest-gpu
      ports: 9000
      resources: GPU request
      volumeMounts: PVC modèle
    - name: relay          # ghcr.io/ia-generative/kevent-ai/relay:vX.Y.Z
      ports: 8080 (health)
      env: KAFKA_INPUT_TOPIC, KAFKA_CONSUMER_GROUP, INFERENCE_BASE_URL, ...
  serviceAccountName: kevent-relay
  nodeSelector: nvidia.com/gpu (ou mig config)
  terminationGracePeriodSeconds: 3600
```

### Service ClusterIP

Expose le port 9000 du container modèle pour le gateway sync path :

```yaml
kind: Service
selector: app: whisper-large-v3
ports: 9000 → 9000
```

### Scale-to-zero

`minReplicaCount: 0` (KEDA). Comportement documenté :
- Sync indisponible pendant le cold start pod (~30-60s selon le modèle)
- Le gateway reçoit connection refused → retourne 503 au client
- KEDA rescale dès qu'un message arrive sur le topic Kafka async

## Section 3 — KEDA ScaledObject

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
spec:
  scaleTargetRef:
    name: whisper-large-v3
  minReplicaCount: 0
  maxReplicaCount: N          # selon capacité GPU dispo
  pollingInterval: 15         # secondes
  cooldownPeriod: 60          # secondes avant scale-down
  triggers:
  - type: kafka
    metadata:
      bootstrapServers: default-kafka-bootstrap.infra-kafka.svc.cluster.local:9093
      consumerGroup: inference-whisper-large-v3
      topic: jobs.whisper-large-v3.input
      lagThreshold: "1"       # 1 message en attente = scale up d'un pod
      sasl: scram_sha512
      tls: enable
```

`lagThreshold: "1"` : chaque pod traite 1 job à la fois → 1 message en attente justifie 1 pod supplémentaire. KEDA calcule `ceil(lag / lagThreshold)` replicas.

## Section 4 — Fichiers k8s/ impactés

| Fichier | Action |
|---|---|
| `k8s/inference-transcription.yaml` | **Remplacé** par `k8s/deployment-transcription.yaml` |
| `k8s/kafka-sources.yaml` | **Supprimé** (plus de KafkaSource push) |
| `k8s/relay-cm.yaml` | **Mis à jour** — renommé `kevent-relay`, ajout `input_topic`/`consumer_group`, `inference.base_url` reste `localhost:9000` |
| `k8s/keda-transcription.yaml` | **Nouveau** — ScaledObject + TriggerAuthentication SASL |
| `k8s/deployment-transcription.yaml` | **Nouveau** — Deployment + Service + ServiceAccount + RBAC |

## Section 5 — Sync path gateway

Pas de changement côté gateway par rapport à PR73 : `sync_topic` supprimé, `POST /v1/*` proxied directement vers `inference_url` configuré (ClusterIP Service :9000).

Avec scale-to-zero, le gateway retourne 503 si le pod est down. Comportement acceptable documenté.
