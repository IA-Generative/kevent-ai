# kevent-gateway

API Gateway pour les services d'inférence KServe. Deux modes de fonctionnement coexistent pour chaque service :

| Mode | Endpoints | Quand l'utiliser |
|---|---|---|
| **Async** (Kafka) | `POST /jobs/{service_type}`, `GET /jobs/{service_type}/{id}` | Fichiers lourds, traitements longs (>30s), besoin de webhook |
| **Sync** (proxy OpenAI) | `POST /v1/*` | Intégration avec les SDK OpenAI officiels, latence faible (<30s) |

## Architecture

### Mode async

```
Client
  │
  ▼
POST /jobs/{service_type} (multipart: file, model, operation?)
  │
  ├─ 1. Fichier → S3
  ├─ 2. Job record → Redis (status: pending)
  └─ 3. InputEvent → Kafka (jobs.<model>.input)
                          │
                          ▼
                    KafkaSource → POST / → Relay sidecar
                                              │
                                              ├─ Download fichier S3
                                              ├─ POST multipart → modèle GPU (127.0.0.1:9000/<inference_url>)
                                              ├─ Upload result.json → S3
                                              └─ ResultEvent → Kafka (jobs.<model>.results)
                                                                    │
                                              ┌─────────────────────┘
                                              │  (consumer interne gateway)
                                              ▼
                                       Redis mis à jour (status: completed/failed)
                                       + Webhook POST si callback_url fourni
Client
  │
  ▼
GET /jobs/{service_type}/{id}  →  { status, result (inline JSON) }
```

### Mode sync (proxy OpenAI-compatible)

Deux chemins selon la configuration :

**A — Sync-over-Kafka** (`sync_topic` configuré, requête `multipart/form-data`) :
```
Client  POST /v1/audio/transcriptions  (multipart)
  │
  ▼
Gateway
  ├─ Upload fichier → S3
  ├─ Subscribe Redis pub/sub  job:<id>:done
  └─ InputEvent → Kafka (jobs.<model>.sync)   ← topic prioritaire
                          │
                    KafkaSource → POST /sync → Relay sidecar
                                                    │
                                                    ├─ syncPriority=1 (reporte les jobs async)
                                                    ├─ Traitement (S3 → modèle → S3)
                                                    └─ ResultEvent → jobs.<model>.results
                                                                          │
                                                               Gateway ConsumerManager
                                                                    └─ Notify Redis pub/sub
                                                                              │
Gateway (débloqué)  ◀────────────────────────────────────────────────────────┘
  └─ Fetch résultat S3 → HTTP 200 au client
```

**B — Direct proxy** (requête `application/json`, ou `multipart` sans `sync_topic`) :
```
Client  POST /v1/*
  │
  ▼
Gateway → HTTP proxy → inference_url + chemin d'origine → modèle GPU
  │
  ▼ (réponse streamée directement)
Client
```

### Composants externes requis

| Composant | Rôle |
|---|---|
| **Kafka** | Bus d'événements (mode async + sync-over-Kafka) — port 9093, SASL_SSL + SCRAM-SHA-512 |
| **Redis** | État des jobs et pub/sub pour le mode sync-over-Kafka (TTL configurable) |
| **S3** | Stockage fichiers d'entrée et résultats |

---

## Démarrage rapide

### Prérequis

- Go 1.23+
- Kafka (SASL_SSL), Redis et un bucket S3-compatible accessibles

### Build

```bash
# Gateway
go build -ldflags "-X main.version=v0.4.2" -o gateway ./cmd/gateway
CONFIG_PATH=/etc/kevent/config.yaml ./gateway

# Relay sidecar
cd relay
go build -o relay ./cmd/relay
RESULT_TOPIC=jobs.whisper-large-v3.results ./relay
```

### Docker

```bash
docker build -t kevent-gateway .
docker run \
  -e S3_ACCESS_KEY=... \
  -e S3_SECRET_KEY=... \
  -e KAFKA_BROKERS=kafka:9093 \
  -e REDIS_ADDR=redis:6379 \
  -p 8080:8080 \
  kevent-gateway
```

---

## Configuration

La configuration est lue depuis `config.yaml` (chemin par défaut). Toutes les valeurs de la forme `${VAR:-défaut}` sont substituées depuis l'environnement au démarrage.

### Gateway (`config.yaml`)

```yaml
server:
  addr: ":8080"
  read_timeout: 120s    # élevé pour les gros uploads
  write_timeout: 0s     # 0 = désactivé — requis pour le mode sync (inférence longue)
  idle_timeout: 120s

kafka:
  brokers:
    - "kafka:9093"
  consumer_group: "kevent-gateway"
  sasl:
    mechanism: "SCRAM-SHA-512"   # PLAIN | SCRAM-SHA-256 | SCRAM-SHA-512
    username:  "kevent-gateway"
    password:  "${KAFKA_SASL_PASSWORD}"
  tls:
    enabled:      true
    ca_cert_path: "/etc/kafka-tls/ca.crt"

s3:
  endpoint: "https://s3.fr-par.scw.cloud"
  region: "fr-par"
  access_key: "${S3_ACCESS_KEY}"
  secret_key: "${S3_SECRET_KEY}"
  bucket: "kevent-jobs"

encryption:
  key: "${ENCRYPTION_KEY:-}"    # AES-256-GCM at-rest, vide = désactivé

redis:
  addr: "redis:6379"
  password: ""
  db: 0
  job_ttl_hours: 72

services:
  - type: audio
    model: "whisper-large-v3"
    default: true           # modèle utilisé par défaut si non précisé et plusieurs modèles configurés
    operations:
      transcription:
        - "/v1/audio/transcriptions"
      translation:
        - "/v1/audio/translations"
    inference_url: "http://kevent-transcription-predictor.default.svc.cluster.local"
    input_topic: jobs.whisper-large-v3.input
    result_topic: jobs.whisper-large-v3.results
    sync_topic: jobs.whisper-large-v3.sync   # active le mode sync-over-Kafka pour les multipart
    accepted_exts: [".mp3", ".wav", ".m4a", ".ogg", ".flac"]
    max_file_size_mb: 500

  - type: ocr
    model: "deepseek-ocr"
    default: true
    operations:
      ocr:
        - "/v1/ocr"
        - "/v1/vision/ocr"    # alias — toutes les paths d'une opération sont indexées
    inference_url: "http://kevent-ocr-predictor.default.svc.cluster.local"
    input_topic: jobs.deepseek-ocr.input
    result_topic: jobs.deepseek-ocr.results
    accepted_exts: [".pdf", ".jpg", ".jpeg", ".png", ".tiff", ".bmp"]
    max_file_size_mb: 50
```

#### Champs `services[]`

| Champ | Description |
|---|---|
| `type` | Nom du type de service (ex: `audio`, `ocr`). Plusieurs entrées peuvent partager le même type avec des modèles différents. |
| `model` | Identifiant du modèle, transmis dans le payload OpenAI pour le routage. |
| `default` | `true` → modèle par défaut pour ce type quand aucun `model` n'est précisé dans la requête. |
| `operations` | Map `nom_opération → liste de paths URL`. Tous les paths sont indexés pour le routage sync ; le premier est utilisé comme `inference_url` dans les events async. |
| `inference_url` | URL de base du backend pour le direct proxy. Le chemin de la requête d'origine y est appendé. |
| `input_topic` | Topic Kafka pour les jobs async en entrée. |
| `result_topic` | Topic Kafka pour les résultats. |
| `sync_topic` | Topic Kafka prioritaire pour le sync-over-Kafka (optionnel). |
| `accepted_exts` | Extensions acceptées (vide = tout). |
| `max_file_size_mb` | Taille max du fichier (défaut : 100 MB). |

### Relay sidecar (`relay/config.yaml`)

```yaml
service:
  result_topic: "${RESULT_TOPIC}"   # ex: jobs.whisper-large-v3.results

kafka:
  brokers: ["${KAFKA_BROKERS:-kafka:9092}"]
  sasl:
    mechanism: "${KAFKA_SASL_MECHANISM:-}"
    username:  "${KAFKA_SASL_USERNAME:-}"
    password:  "${KAFKA_SASL_PASSWORD:-}"
  tls:
    enabled:      ${KAFKA_TLS_ENABLED:-false}
    ca_cert_path: "${KAFKA_CA_CERT_PATH:-}"

s3:
  endpoint:   "${S3_ENDPOINT:-https://s3.fr-par.scw.cloud}"
  region:     "${S3_REGION:-fr-par}"
  access_key: "${S3_ACCESS_KEY}"
  secret_key: "${S3_SECRET_KEY}"
  bucket:     "${S3_BUCKET:-kevent-jobs}"

encryption:
  key: "${ENCRYPTION_KEY:-}"

# URL de base du container d'inférence local (même pod).
# Le chemin OpenAI est fourni par le gateway dans InputEvent.inference_url
# et appendé dynamiquement : base_url + inference_url.
inference:
  base_url: "http://127.0.0.1:${INFERENCE_PORT:-9000}"
  api_key:  ""
  timeout:  "300s"
  extra_fields:             # champs form optionnels ajoutés à chaque requête multipart
    response_format: "json"
    # language: "fr"
    # prompt: "..."
```

### Variables d'environnement (gateway)

| Variable | Valeur par défaut | Description |
|---|---|---|
| `CONFIG_PATH` | `config.yaml` | Chemin vers le fichier de configuration |
| `S3_ENDPOINT` | `https://s3.fr-par.scw.cloud` | Endpoint S3 |
| `S3_REGION` | `fr-par` | Région |
| `S3_ACCESS_KEY` | — | Access Key ID (**requis**) |
| `S3_SECRET_KEY` | — | Secret Key (**requis**) |
| `S3_BUCKET` | `kevent-jobs` | Nom du bucket |
| `KAFKA_BROKERS` | `kafka:9092` | Adresse(s) des brokers Kafka |
| `KAFKA_SASL_PASSWORD` | _(vide)_ | Mot de passe SASL Kafka |
| `REDIS_ADDR` | `redis:6379` | Adresse Redis |
| `REDIS_PASSWORD` | _(vide)_ | Mot de passe Redis |
| `ENCRYPTION_KEY` | _(vide)_ | Clé AES-256-GCM hex-encodée (32 octets) |

### Variables d'environnement (relay sidecar)

| Variable | Valeur par défaut | Description |
|---|---|---|
| `RESULT_TOPIC` | — | Topic Kafka résultats (**requis**) |
| `INFERENCE_PORT` | `9000` | Port du serveur de modèle local |
| `KAFKA_BROKERS` | `kafka:9092` | Brokers Kafka |
| `KAFKA_SASL_MECHANISM` | _(vide)_ | Mécanisme SASL |
| `KAFKA_SASL_USERNAME` | _(vide)_ | Utilisateur SASL |
| `KAFKA_SASL_PASSWORD` | _(vide)_ | Mot de passe SASL |
| `KAFKA_TLS_ENABLED` | `false` | Activer TLS |
| `KAFKA_CA_CERT_PATH` | _(vide)_ | Chemin vers la CA Strimzi |
| `S3_ACCESS_KEY` | — | Access Key ID (**requis**) |
| `S3_SECRET_KEY` | — | Secret Key (**requis**) |
| `ENCRYPTION_KEY` | _(vide)_ | Doit correspondre à la valeur du gateway |

---

## Kafka — authentification SASL/TLS

Le gateway et le relay sidecar supportent tous deux SASL (PLAIN, SCRAM-SHA-256, SCRAM-SHA-512) et TLS avec CA personnalisée, configurés via `config.yaml`.

En production (Strimzi) le cluster tourne sur le port **9093** (`SASL_SSL`). Les credentials sont injectés depuis des Secrets Kubernetes.

### Prérequis Strimzi

**KafkaUser `kevent-gateway`** (namespace `infra-kafka`) — ACLs minimales :

| Ressource | Type | Pattern | Opérations |
|---|---|---|---|
| `jobs.*` | topic | prefix | `Read`, `Write`, `Create`, `Describe` |
| `kevent-gateway` | group | prefix | `Read`, `Describe`, `Delete` |

**KafkaUser `kevent-relay`** (namespace `infra-kafka`) — ACLs minimales :

| Ressource | Type | Pattern | Opérations |
|---|---|---|---|
| `jobs.*` | topic | prefix | `Read`, `Write`, `Create`, `Describe` |
| `inference-*` | group | prefix | `Read`, `Describe`, `Delete` |

> `Delete` sur le group est requis par le contrôleur Knative pour la finalisation du ConsumerGroup.

### Secret SASL (à créer dans le namespace du gateway)

```bash
kubectl get secret kevent-gateway -n infra-kafka -o yaml \
  | sed 's/namespace: infra-kafka/namespace: default/' \
  | kubectl apply -f -
```

Le secret doit contenir la clé `KAFKA_SASL_PASSWORD` — **sans newline final** (erreur courante lors de la création manuelle).

---

## Helm — déploiement du gateway

```bash
helm upgrade --install kevent-gateway ./helm/gateway \
  -f values.yaml \
  --namespace default
```

### Valeurs clés (`values.yaml`)

```yaml
image:
  repository: ghcr.io/ronan-wescale/ai-kevent/gateway
  tag: v0.4.2

kafka:
  brokers: "default-kafka-bootstrap.infra-kafka.svc.cluster.local:9093"
  sasl:
    enabled: true
    mechanism: SCRAM-SHA-512
    username: kevent-gateway
    existingSecret: "kevent-gateway"
  tls:
    enabled: true
    existingCACertSecret: "default-cluster-ca-cert"

services:
  - type: audio
    model: "whisper-large-v3"
    default: true
    operations:
      transcription:
        - "/v1/audio/transcriptions"
      translation:
        - "/v1/audio/translations"
    inferenceURL: "http://kevent-transcription-predictor.default.svc.cluster.local"
    inputTopic: "jobs.whisper-large-v3.input"
    resultTopic: "jobs.whisper-large-v3.results"
    syncTopic: "jobs.whisper-large-v3.sync"
    acceptedExts: [".mp3", ".wav", ".m4a", ".ogg", ".flac"]
    maxFileSizeMB: 500
```

---

## Ajouter un service d'inférence

Aucun changement de code n'est nécessaire. Il suffit d'ajouter un bloc dans `config.yaml` (gateway) et de déployer un relay sidecar configuré avec le `RESULT_TOPIC` correspondant.

**Gateway `config.yaml`** :

```yaml
services:
  - type: audio
    model: "pyannote-audio-3.1"
    operations:
      diarization:
        - "/v1/audio/diarizations"
    inference_url: "http://kevent-diarization-predictor.default.svc.cluster.local"
    input_topic: jobs.pyannote-audio-3.1.input
    result_topic: jobs.pyannote-audio-3.1.results
    sync_topic: jobs.pyannote-audio-3.1.sync
    accepted_exts: [".mp3", ".wav", ".m4a", ".ogg", ".flac"]
    max_file_size_mb: 500
```

**Relay sidecar** (env vars dans le manifest KServe) :

```yaml
- name: RESULT_TOPIC
  value: "jobs.pyannote-audio-3.1.results"
- name: INFERENCE_PORT
  value: "9000"
```

> **Multi-modèles par type** : plusieurs entrées peuvent partager le même `type` avec des `model` différents. Le gateway sélectionne le backend d'après le champ `model` de la requête. Le champ `default: true` désigne le modèle utilisé si `model` est absent et que plusieurs modèles sont configurés.

> **Multi-opérations par modèle** : un même modèle peut exposer plusieurs opérations (ex: transcription et translation) via `operations`. En mode async, préciser l'opération avec `-F operation=transcription` quand le modèle en propose plusieurs.

> **Pré-requis Kafka** : les topics `input_topic`, `result_topic` et `sync_topic` doivent être créés avant le démarrage (`AllowAutoTopicCreation: false`).

---

## API

### Documentation interactive

Le gateway génère le spec OpenAPI 3.0 à chaque démarrage depuis le registre de services :

- **Swagger UI** : `GET /docs`
- **Spec brut** : `GET /openapi.yaml`

### Mode sync — Endpoints OpenAI-compatibles

Ces endpoints sont exposés dynamiquement d'après les `operations` configurées dans `config.yaml`.

#### `POST /v1/audio/transcriptions` — Transcription audio

**Content-Type** : `multipart/form-data`

| Champ | Type | Requis | Description |
|---|---|---|---|
| `model` | string | si plusieurs modèles | Ex: `whisper-large-v3`. Optionnel si un seul modèle ou un défaut configuré. |
| `file` | file | oui | Fichier audio (.mp3, .wav, .m4a, .ogg, .flac) |

```bash
curl https://api.kevent.example.com/v1/audio/transcriptions \
  -F model=whisper-large-v3 \
  -F file=@interview.wav
```

**Avec le SDK OpenAI Python**

```python
from openai import OpenAI

client = OpenAI(base_url="https://api.kevent.example.com", api_key="unused")

with open("interview.wav", "rb") as f:
    transcript = client.audio.transcriptions.create(
        model="whisper-large-v3",
        file=f,
    )
print(transcript.text)
```

---

#### `POST /v1/ocr` — OCR (documents, images)

**Content-Type** : `multipart/form-data`

| Champ | Type | Requis | Description |
|---|---|---|---|
| `model` | string | si plusieurs modèles | Ex: `deepseek-ocr` |
| `file` | file | oui | Document (.pdf, .jpg, .jpeg, .png, .tiff, .bmp) |

```bash
curl https://api.kevent.example.com/v1/ocr \
  -F model=deepseek-ocr \
  -F file=@document.pdf
```

---

### Mode async — Jobs

#### `POST /jobs/{service_type}` — Soumettre un job

**Content-Type** : `multipart/form-data`

| Champ | Type | Requis | Description |
|---|---|---|---|
| `model` | string | si plusieurs modèles sans défaut | Ex: `whisper-large-v3`. Optionnel si un seul modèle ou `default: true` configuré. |
| `operation` | string | si plusieurs opérations | Ex: `transcription` ou `translation`. Optionnel si une seule opération pour le modèle. |
| `file` | file | oui | Fichier à traiter |
| `callback_url` | string | non | URL appelée en POST à la complétion du job |

**Réponse** `202 Accepted`

```json
{
  "job_id": "550e8400-e29b-41d4-a716-446655440000",
  "service_type": "audio",
  "model": "whisper-large-v3",
  "status": "pending"
}
```

```bash
# Modèle et opération explicites
curl -X POST http://localhost:8080/jobs/audio \
  -F "model=whisper-large-v3" \
  -F "operation=transcription" \
  -F "file=@interview.wav" \
  -F "callback_url=https://mon-app.example.com/hooks/inference"

# Modèle par défaut, opération unique → champs optionnels
curl -X POST http://localhost:8080/jobs/audio \
  -F "file=@interview.wav"
```

---

#### `GET /jobs/{service_type}/{id}` — Statut d'un job

**Réponse** `200 OK`

```json
{
  "job_id": "550e8400-e29b-41d4-a716-446655440000",
  "service_type": "audio",
  "model": "whisper-large-v3",
  "status": "completed",
  "result": { "text": "Bonjour, bienvenue à cette réunion..." },
  "created_at": "2026-03-05T10:00:00Z",
  "updated_at": "2026-03-05T10:04:32Z"
}
```

| Champ | Description |
|---|---|
| `status` | `pending` \| `processing` \| `completed` \| `failed` |
| `result` | Payload JSON du résultat d'inférence (présent uniquement si `completed`) |
| `error` | Message d'erreur (présent uniquement si `failed`) |

> **Attention** : le fichier résultat S3 est supprimé après cet appel — les appels suivants retournent 404.

**Polling simple**

```bash
JOB_ID="550e8400-e29b-41d4-a716-446655440000"
while true; do
  RESPONSE=$(curl -s http://localhost:8080/jobs/audio/$JOB_ID)
  STATUS=$(echo $RESPONSE | jq -r '.status')
  [ "$STATUS" = "completed" ] && echo $RESPONSE | jq '.result' && break
  [ "$STATUS" = "failed" ]    && echo "Erreur : $(echo $RESPONSE | jq -r '.error')" && break
  sleep 10
done
```

---

### `GET /health`

```json
{ "status": "ok", "time": "2026-03-05T10:00:00Z" }
```

---

## Contrat Kafka (mode async)

### InputEvent — publié par le gateway sur `input_topic`

```json
{
  "job_id": "550e8400-e29b-41d4-a716-446655440000",
  "service_type": "audio",
  "model": "whisper-large-v3",
  "input_ref": "550e8400-.../input.wav",
  "inference_url": "/v1/audio/transcriptions",
  "created_at": "2026-03-05T10:00:00Z"
}
```

| Champ | Description |
|---|---|
| `input_ref` | Clé objet S3 du fichier d'entrée |
| `inference_url` | Chemin OpenAI à appeler sur le modèle local (appendé à `inference.base_url` du relay) — dérivé du premier path de l'opération choisie |

### ResultEvent — publié par le relay sur `result_topic`

```json
{
  "job_id": "550e8400-e29b-41d4-a716-446655440000",
  "service_type": "audio",
  "status": "completed",
  "result_ref": "550e8400-.../result.json",
  "completed_at": "2026-03-05T10:04:32Z"
}
```

| Champ | Description |
|---|---|
| `status` | `completed` ou `failed` |
| `result_ref` | Clé objet S3 du fichier résultat (si `completed`) |
| `error` | Message d'erreur lisible (si `failed`) |

---

## Webhook (optionnel, mode async)

Si `callback_url` est fourni à la soumission, le gateway effectue un `POST` sur cette URL dès que le job passe à l'état `completed` ou `failed`. En cas d'échec HTTP (5xx ou timeout), 3 tentatives sont faites avec un backoff exponentiel (2 s → 4 s → 8 s).

```json
{
  "job_id": "550e8400-e29b-41d4-a716-446655440000",
  "service_type": "audio",
  "status": "completed",
  "result_ref": "550e8400-.../result.json",
  "completed_at": "2026-03-05T10:04:32Z"
}
```

---

## Structure du projet

```
.
├── cmd/gateway/main.go          # Point d'entrée — wiring et graceful shutdown
├── internal/
│   ├── config/config.go         # Chargement YAML + expansion des variables d'env
│   ├── model/job.go             # Types partagés : Job, InputEvent, ResultEvent
│   ├── service/registry.go      # Registre config-driven (routing sync + async, défaut par type)
│   ├── storage/
│   │   ├── s3.go                # Client S3 (AWS SDK v2)
│   │   └── redis.go             # Persistance des jobs (JSON blob + TTL)
│   ├── kafka/
│   │   ├── auth.go              # Helpers SASL (PLAIN/SCRAM) + TLS
│   │   ├── producer.go          # Producteur Kafka — un writer par topic
│   │   └── consumer.go          # Consumer résultats — une goroutine par result_topic
│   └── handler/
│       ├── jobs.go              # POST /jobs/{service_type}  •  GET /jobs/{service_type}/{id}
│       ├── sync.go              # POST /v1/*  (sync-over-Kafka ou direct proxy)
│       ├── docs.go              # GET /docs (Swagger UI)  •  GET /openapi.yaml (spec généré dynamiquement)
│       ├── health.go            # GET /health
│       └── middleware.go        # Logger structuré (slog/JSON)
├── relay/                       # Relay sidecar (module Go séparé : kevent/relay)
│   ├── cmd/relay/main.go
│   ├── internal/
│   │   ├── config/config.go     # Config relay : inference.base_url + extra_fields
│   │   ├── kafka/               # SASL + TLS, publisher résultats
│   │   ├── relay/               # Handler CloudEvent (async POST /, sync POST /sync)
│   │   ├── adapter/             # Adapter multipart générique (model + extra_fields + file)
│   │   └── storage/             # Client S3
│   └── config.yaml              # Config template (env vars expansées au démarrage)
├── helm/gateway/                # Chart Helm du gateway (inclut Redis-HA)
├── k8s/                         # Manifestes Kubernetes (KafkaUser, KafkaSource, ServingRuntime)
├── config.yaml                  # Configuration par défaut du gateway
└── Dockerfile                   # Multi-stage build → image distroless (~10 MB)
```
