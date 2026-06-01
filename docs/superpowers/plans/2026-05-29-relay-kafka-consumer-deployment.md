# Relay Kafka Consumer + Deployment Standard + KEDA Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remplacer le relay one-shot JobSink de PR73 par un consumer Kafka long-running dans un Deployment Kubernetes standard, avec KEDA pour le scaling sur le lag Kafka.

**Architecture:** Chaque pod contient deux containers (model + relay). Le relay pull ses jobs depuis Kafka via consumer group (kafka-go Reader). KEDA scale le Deployment en fonction du lag. Le gateway proxie les requêtes sync directement vers le ClusterIP Service du Deployment.

**Tech Stack:** Go 1.21+, github.com/segmentio/kafka-go v0.4.47, KEDA v2, Kubernetes Deployments, Prometheus metrics

---

## File Structure

**Fichiers modifiés (relay Go) :**
- `relay/internal/config/config.go` — Ajoute `InputTopic` + `ConsumerGroup` à `KafkaConfig`
- `relay/config.yaml` — Ajoute `input_topic` + `consumer_group`
- `relay/internal/kafka/auth.go` — Ajoute `buildDialer` (pour Readers)
- `relay/internal/kafka/consumer.go` — **Nouveau** : wrapper `Consumer` autour de `kafka.NewReader`
- `relay/internal/relay/relay.go` — Ajoute `annotator *lifecycle.PodAnnotator`; set/unset dans `Process`
- `relay/cmd/relay/main.go` — **Réécrit** : boucle consumer + health server + graceful shutdown

**Fichiers k8s/ (nouveaux ou modifiés) :**
- `k8s/relay-cm.yaml` — Renomme ConfigMap `kevent-sidecar` → `kevent-relay`; ajoute champs consumer
- `k8s/deployment-transcription.yaml` — **Nouveau** : Deployment + Service + RBAC
- `k8s/keda-transcription.yaml` — **Nouveau** : TriggerAuthentication + ScaledObject

**Fichiers k8s/ supprimés :**
- `k8s/inference-transcription.yaml` — Remplacé par `deployment-transcription.yaml`
- `k8s/kafka-sources.yaml` — Plus de KafkaSource push

---

## Task 1: Config — InputTopic + ConsumerGroup

**Files:**
- Modify: `relay/internal/config/config.go`
- Modify: `relay/config.yaml`
- Test: `relay/internal/config/config_test.go`

- [ ] **Step 1: Ajouter les champs à KafkaConfig**

Dans `relay/internal/config/config.go`, modifier `KafkaConfig` :

```go
type KafkaConfig struct {
	Brokers       []string   `yaml:"brokers"`
	SASL          SASLConfig `yaml:"sasl"`
	TLS           TLSConfig  `yaml:"tls"`
	InputTopic    string     `yaml:"input_topic"`
	ConsumerGroup string     `yaml:"consumer_group"`
}
```

- [ ] **Step 2: Valider les nouveaux champs**

Dans `validate()` de `relay/internal/config/config.go`, ajouter après la validation Kafka existante :

```go
if c.Kafka.InputTopic == "" {
    return fmt.Errorf("kafka.input_topic is required (set KAFKA_INPUT_TOPIC env var)")
}
if c.Kafka.ConsumerGroup == "" {
    return fmt.Errorf("kafka.consumer_group is required (set KAFKA_CONSUMER_GROUP env var)")
}
```

- [ ] **Step 3: Écrire le test de validation**

Dans `relay/internal/config/config_test.go`, ajouter :

```go
func TestValidate_MissingInputTopic(t *testing.T) {
	cfg := &Config{
		Service: ServiceConfig{ResultTopic: "jobs.x.results"},
		Kafka: KafkaConfig{
			Brokers:       []string{"kafka:9092"},
			ConsumerGroup: "g",
		},
		S3: S3Config{
			Endpoint: "https://s3.example.com",
			Region:   "fr-par",
			Bucket:   "bucket",
		},
		Inference: InferenceConfig{BaseURL: "http://127.0.0.1:9000"},
	}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "input_topic") {
		t.Errorf("expected input_topic validation error, got %v", err)
	}
}

func TestValidate_MissingConsumerGroup(t *testing.T) {
	cfg := &Config{
		Service: ServiceConfig{ResultTopic: "jobs.x.results"},
		Kafka: KafkaConfig{
			Brokers:    []string{"kafka:9092"},
			InputTopic: "jobs.x.input",
		},
		S3: S3Config{
			Endpoint: "https://s3.example.com",
			Region:   "fr-par",
			Bucket:   "bucket",
		},
		Inference: InferenceConfig{BaseURL: "http://127.0.0.1:9000"},
	}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "consumer_group") {
		t.Errorf("expected consumer_group validation error, got %v", err)
	}
}
```

Ajouter `"strings"` aux imports du fichier de test.

- [ ] **Step 4: Mettre à jour relay/config.yaml**

Ajouter dans la section `kafka:` de `relay/config.yaml` :

```yaml
kafka:
  brokers: ["${KAFKA_BROKERS:-kafka:9092}"]
  sasl:
    mechanism: "${KAFKA_SASL_MECHANISM:-}"
    username:  "${KAFKA_SASL_USERNAME:-}"
    password:  "${KAFKA_SASL_PASSWORD:-}"
  tls:
    enabled:      ${KAFKA_TLS_ENABLED:-false}
    ca_cert_path: "${KAFKA_CA_CERT_PATH:-}"
  input_topic:    "${KAFKA_INPUT_TOPIC}"
  consumer_group: "${KAFKA_CONSUMER_GROUP}"
```

- [ ] **Step 5: Lancer les tests config**

```bash
cd relay && go test ./internal/config/... -v
```

Expected: PASS (tous les tests existants + les 2 nouveaux)

- [ ] **Step 6: Commit**

```bash
git add relay/internal/config/config.go relay/internal/config/config_test.go relay/config.yaml
git commit -m "feat(relay): add input_topic and consumer_group to kafka config"
```

---

## Task 2: Kafka auth — buildDialer pour les Readers

**Files:**
- Modify: `relay/internal/kafka/auth.go`

- [ ] **Step 1: Ajouter buildDialer**

Dans `relay/internal/kafka/auth.go`, ajouter après `buildTransport` :

```go
// buildDialer returns a Dialer configured with SASL and/or TLS for Readers.
// Returns kafkago.DefaultDialer when neither is configured.
func buildDialer(cfg config.KafkaConfig) (*kafkago.Dialer, error) {
	mechanism, err := buildSASL(cfg.SASL)
	if err != nil {
		return nil, err
	}
	tlsCfg, err := buildTLS(cfg.TLS)
	if err != nil {
		return nil, err
	}
	if mechanism == nil && tlsCfg == nil {
		return kafkago.DefaultDialer, nil
	}
	return &kafkago.Dialer{
		SASLMechanism: mechanism,
		TLS:           tlsCfg,
	}, nil
}
```

- [ ] **Step 2: Build relay pour vérifier la compilation**

```bash
cd relay && go build ./...
```

Expected: no errors

- [ ] **Step 3: Commit**

```bash
git add relay/internal/kafka/auth.go
git commit -m "feat(relay/kafka): add buildDialer for consumer readers"
```

---

## Task 3: Kafka Consumer wrapper

**Files:**
- Create: `relay/internal/kafka/consumer.go`

- [ ] **Step 1: Créer le consumer**

Créer `relay/internal/kafka/consumer.go` :

```go
package kafka

import (
	"context"
	"time"

	kafkago "github.com/segmentio/kafka-go"

	"kevent/relay/internal/config"
)

// Consumer wraps a kafka-go Reader for manual-commit message consumption.
// Each pod holds one Consumer; Kafka's consumer group protocol distributes
// partitions across pods automatically on scale-up/down.
type Consumer struct {
	reader *kafkago.Reader
}

// NewConsumer creates a Consumer for the topic and group in cfg.
func NewConsumer(cfg config.KafkaConfig) (*Consumer, error) {
	dialer, err := buildDialer(cfg)
	if err != nil {
		return nil, err
	}
	r := kafkago.NewReader(kafkago.ReaderConfig{
		Brokers:  cfg.Brokers,
		GroupID:  cfg.ConsumerGroup,
		Topic:    cfg.InputTopic,
		Dialer:   dialer,
		MinBytes: 1,
		MaxBytes: 10 << 20, // 10 MB
		MaxWait:  5 * time.Second,
	})
	return &Consumer{reader: r}, nil
}

// FetchMessage blocks until a message is available or ctx is cancelled.
// The message is NOT committed until CommitMessages is called.
func (c *Consumer) FetchMessage(ctx context.Context) (kafkago.Message, error) {
	return c.reader.FetchMessage(ctx)
}

// CommitMessages marks msg as processed in the consumer group.
// Call only after successful processing.
func (c *Consumer) CommitMessages(ctx context.Context, msg kafkago.Message) error {
	return c.reader.CommitMessages(ctx, msg)
}

// Close closes the underlying reader and leaves the consumer group.
func (c *Consumer) Close() error {
	return c.reader.Close()
}
```

- [ ] **Step 2: Build pour vérifier la compilation**

```bash
cd relay && go build ./...
```

Expected: no errors

- [ ] **Step 3: Commit**

```bash
git add relay/internal/kafka/consumer.go
git commit -m "feat(relay/kafka): add Consumer pull-based reader"
```

---

## Task 4: Processor — pod-deletion-cost annotator

**Files:**
- Modify: `relay/internal/relay/relay.go`

- [ ] **Step 1: Ajouter l'annotator au Processor**

Dans `relay/internal/relay/relay.go`, modifier la struct et le constructeur.

Struct actuelle :
```go
type Processor struct {
	adapter     adapter.Adapter
	s3          objectStore
	publisher   eventPublisher
	resultTopic string
}
```

Remplacer par :
```go
type Processor struct {
	adapter     adapter.Adapter
	s3          objectStore
	publisher   eventPublisher
	resultTopic string
	annotator   *lifecycle.PodAnnotator
}
```

- [ ] **Step 2: Ajouter l'import lifecycle**

Dans les imports de `relay.go`, ajouter :
```go
"kevent/relay/internal/lifecycle"
```

- [ ] **Step 3: Mettre à jour New pour accepter l'annotator**

Remplacer la fonction `New` actuelle par :

```go
func New(
	adp adapter.Adapter,
	s3 *storage.S3Client,
	pub *kafka.Publisher,
	resultTopic string,
	annotator *lifecycle.PodAnnotator,
) *Processor {
	return &Processor{
		adapter:     adp,
		s3:          s3,
		publisher:   pub,
		resultTopic: resultTopic,
		annotator:   annotator,
	}
}
```

- [ ] **Step 4: Ajouter les appels set/unset dans Process**

La méthode `Process` actuelle :
```go
func (p *Processor) Process(ctx context.Context, event *model.InputEvent) error {
	return p.process(ctx, event)
}
```

Remplacer par :
```go
func (p *Processor) Process(ctx context.Context, event *model.InputEvent) error {
	if p.annotator != nil {
		if err := p.annotator.SetDeletionCost(ctx, lifecycle.CostBusy); err != nil {
			slog.Warn("failed to set pod deletion cost busy", "error", err)
		}
	}
	err := p.process(ctx, event)
	if p.annotator != nil {
		if setErr := p.annotator.SetDeletionCost(context.Background(), lifecycle.CostIdle); setErr != nil {
			slog.Warn("failed to set pod deletion cost idle", "error", setErr)
		}
	}
	return err
}
```

- [ ] **Step 5: Vérifier que les tests existants compilent**

Les tests utilisent `newTestProcessor` qui construit `&Processor{...}` directement sans passer par `New` — le champ `annotator` sera nil, ce qui est le comportement attendu hors K8s.

```bash
cd relay && go test ./internal/relay/... -v
```

Expected: tous les tests PASS (annotator nil = pas d'appels, comportement identique)

- [ ] **Step 6: Commit**

```bash
git add relay/internal/relay/relay.go
git commit -m "feat(relay): add pod-deletion-cost annotator to processor"
```

---

## Task 5: Main — boucle consumer + health server + graceful shutdown

**Files:**
- Modify: `relay/cmd/relay/main.go`

- [ ] **Step 1: Réécrire main.go**

Remplacer entièrement `relay/cmd/relay/main.go` :

```go
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	kafkago "github.com/segmentio/kafka-go"

	"kevent/relay/internal/adapter"
	"kevent/relay/internal/config"
	"kevent/relay/internal/kafka"
	"kevent/relay/internal/lifecycle"
	"kevent/relay/internal/relay"
	"kevent/relay/internal/storage"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfgPath := "config.yaml"
	if v := os.Getenv("CONFIG_PATH"); v != "" {
		cfgPath = v
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	s3Client, err := storage.NewS3Client(cfg.S3, cfg.Encryption)
	if err != nil {
		slog.Error("failed to initialise S3 client", "error", err)
		os.Exit(1)
	}

	publisher, err := kafka.NewPublisher(cfg.Kafka)
	if err != nil {
		slog.Error("failed to initialise Kafka publisher", "error", err)
		os.Exit(1)
	}
	defer publisher.Close()

	consumer, err := kafka.NewConsumer(cfg.Kafka)
	if err != nil {
		slog.Error("failed to initialise Kafka consumer", "error", err)
		os.Exit(1)
	}
	defer consumer.Close()

	adp, err := adapter.New(cfg)
	if err != nil {
		slog.Error("failed to initialise adapter", "error", err)
		os.Exit(1)
	}

	annotator := lifecycle.New()
	proc := relay.New(adp, s3Client, publisher, cfg.Service.ResultTopic, annotator)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go serveHealth()

	slog.Info("relay consumer started",
		"topic", cfg.Kafka.InputTopic,
		"consumer_group", cfg.Kafka.ConsumerGroup,
	)

	for {
		msg, err := consumer.FetchMessage(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				slog.Info("relay shutting down")
				break
			}
			slog.Error("kafka fetch error", "error", err)
			os.Exit(1)
		}

		if err := handleMessage(proc, consumer, msg); err != nil {
			slog.Error("fatal job error, exiting for Kafka redeliver", "error", err)
			os.Exit(1)
		}

		// After completing a job, honour a pending shutdown signal.
		if ctx.Err() != nil {
			slog.Info("relay shutting down after job completion")
			break
		}
	}
}

// handleMessage processes one Kafka message.
// Returns an error only for transient infra failures that warrant pod restart.
// Malformed/invalid messages are skipped (committed without processing).
func handleMessage(proc *relay.Processor, consumer *kafka.Consumer, msg kafkago.Message) error {
	event, err := relay.ParseInputEvent(msg.Value)
	if err != nil || event.JobID == "" {
		slog.Error("skipping unparseable message", "error", err, "offset", msg.Offset, "partition", msg.Partition)
		// Use Background context: ctx may be cancelled (SIGTERM) at this point.
		return consumer.CommitMessages(context.Background(), msg)
	}

	slog.Info("processing job", "job_id", event.JobID, "service_type", event.ServiceType, "offset", msg.Offset)

	// Use a detached context so SIGTERM does not interrupt an in-flight inference job.
	jobCtx := context.WithoutCancel(ctx)
	if err := proc.Process(jobCtx, event); err != nil {
		return err // transient infra error — caller will os.Exit(1)
	}

	if err := consumer.CommitMessages(context.WithoutCancel(ctx), msg); err != nil {
		return err
	}

	slog.Info("job committed", "job_id", event.JobID)
	return nil
}

func serveHealth() {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	if err := http.ListenAndServe(":8080", mux); err != nil {
		slog.Error("health server stopped", "error", err)
	}
}
```

- [ ] **Step 2: Build relay**

```bash
cd relay && go build ./cmd/relay
```

Expected: no errors, binary produced

- [ ] **Step 3: Lancer tous les tests relay**

```bash
cd relay && go test ./...
```

Expected: tous les tests PASS

- [ ] **Step 4: Commit**

```bash
git add relay/cmd/relay/main.go
git commit -m "feat(relay): rewrite as Kafka pull consumer with graceful shutdown"
```

---

## Task 6: k8s — Mise à jour relay ConfigMap

**Files:**
- Modify: `k8s/relay-cm.yaml`

- [ ] **Step 1: Mettre à jour relay-cm.yaml**

Remplacer entièrement `k8s/relay-cm.yaml` :

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: kevent-relay
  namespace: default
data:
  config.yaml: |
    service:
      result_topic: "${RESULT_TOPIC}"

    kafka:
      brokers: ["${KAFKA_BROKERS:-kafka:9092}"]
      sasl:
        mechanism: "${KAFKA_SASL_MECHANISM:-}"
        username:  "${KAFKA_SASL_USERNAME:-}"
        password:  "${KAFKA_SASL_PASSWORD:-}"
      tls:
        enabled:      ${KAFKA_TLS_ENABLED:-false}
        ca_cert_path: "${KAFKA_CA_CERT_PATH:-}"
      input_topic:    "${KAFKA_INPUT_TOPIC}"
      consumer_group: "${KAFKA_CONSUMER_GROUP}"

    encryption:
      key: "${ENCRYPTION_KEY:-}"

    s3:
      endpoint:   "${S3_ENDPOINT:-https://s3.fr-par.scw.cloud}"
      region:     "${S3_REGION:-fr-par}"
      access_key: "${S3_ACCESS_KEY}"
      secret_key: "${S3_SECRET_KEY}"
      bucket:     "${S3_BUCKET:-kevent-jobs}"

    inference:
      base_url:             "http://127.0.0.1:${INFERENCE_PORT:-9000}"
      api_key:              ""
      timeout:              "${INFERENCE_TIMEOUT:-3600s}"
      ready_timeout:        "${INFERENCE_READY_TIMEOUT:-10m}"
      ready_interval:       "${INFERENCE_READY_INTERVAL:-5s}"
      health_check_timeout: "${INFERENCE_HEALTH_CHECK_TIMEOUT:-2s}"
      extra_fields:
        response_format: "json"
```

Note: `inference.base_url` reste `http://127.0.0.1:${INFERENCE_PORT:-9000}` — le model container et le relay sont dans le même pod.

- [ ] **Step 2: Commit**

```bash
git add k8s/relay-cm.yaml
git commit -m "feat(k8s): rename relay ConfigMap to kevent-relay, add consumer fields"
```

---

## Task 7: k8s — Deployment + Service + RBAC

**Files:**
- Create: `k8s/deployment-transcription.yaml`

- [ ] **Step 1: Créer deployment-transcription.yaml**

Créer `k8s/deployment-transcription.yaml` :

```yaml
# Deployment standard pour le service de transcription Whisper.
# Remplace l'InferenceService KServe + sidecar relay.
#
# Deux containers par pod :
#   - whisper-api  : modèle d'inférence (GPU), port 9000
#   - relay        : consumer Kafka pull, appelle localhost:9000
#
# Scaling : géré par KEDA (voir k8s/keda-transcription.yaml).
# Sync path : le gateway proxie POST /v1/audio/transcriptions
#             directement vers le Service ClusterIP :9000.
#
# Note scale-to-zero : si minReplicaCount KEDA = 0, le sync path
# retourne 503 pendant le cold start (~30-60s). Comportement attendu.
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: kevent-relay
  namespace: default
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: kevent-relay-pod-annotator
  namespace: default
rules:
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: kevent-relay-pod-annotator
  namespace: default
subjects:
  - kind: ServiceAccount
    name: kevent-relay
    namespace: default
roleRef:
  kind: Role
  name: kevent-relay-pod-annotator
  apiGroup: rbac.authorization.k8s.io
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: whisper-large-v3
  namespace: default
  labels:
    app: whisper-large-v3
spec:
  # Replicas managed by KEDA — do not set replicas here to avoid conflicts.
  selector:
    matchLabels:
      app: whisper-large-v3
  template:
    metadata:
      labels:
        app: whisper-large-v3
    spec:
      serviceAccountName: kevent-relay
      terminationGracePeriodSeconds: 3600  # max inference job duration
      nodeSelector:
        nvidia.com/mig.config: all-1g.10gb
      containers:
        # ── Inference model ──────────────────────────────────────────────────
        - name: whisper-api
          image: ghcr.io/ia-generative/whisper-api:latest-gpu
          imagePullPolicy: IfNotPresent
          ports:
            - containerPort: 9000
              protocol: TCP
          env:
            - name: HF_HOME
              value: /mnt/models
            - name: WHISPER_DEVICE
              value: cuda
            - name: WHISPER_COMPUTE_TYPE
              value: float16
            - name: MODEL_NAME
              value: whisper
            - name: HTTP_PORT
              value: "9000"
            - name: WHISPER_NUM_WORKERS
              value: "4"
            - name: MODEL_DIR
              value: /mnt/models/models/Systran/faster-whisper-large-v3
          volumeMounts:
            - name: model-pvc
              mountPath: /mnt/models
          resources:
            requests:
              cpu: "1"
              memory: 16Gi
              nvidia.com/gpu: "1"
            limits:
              cpu: "2"
              memory: 32Gi
              nvidia.com/gpu: "1"
          readinessProbe:
            httpGet:
              path: /health
              port: 9000
            initialDelaySeconds: 30
            periodSeconds: 10
        # ── Relay consumer ───────────────────────────────────────────────────
        - name: relay
          image: ghcr.io/ia-generative/kevent-ai/relay:v0.6.0
          imagePullPolicy: IfNotPresent
          ports:
            - containerPort: 8080
              protocol: TCP
          env:
            - name: CONFIG_PATH
              value: /etc/kevent/config.yaml
            - name: INFERENCE_PORT
              value: "9000"
            - name: RESULT_TOPIC
              value: jobs.whisper-large-v3.results
            - name: KAFKA_INPUT_TOPIC
              value: jobs.whisper-large-v3.input
            - name: KAFKA_CONSUMER_GROUP
              value: inference-whisper-large-v3
            - name: KAFKA_BROKERS
              value: "default-kafka-bootstrap.infra-kafka.svc.cluster.local:9093"
            - name: KAFKA_SASL_MECHANISM
              value: "SCRAM-SHA-512"
            - name: KAFKA_TLS_ENABLED
              value: "true"
            - name: KAFKA_CA_CERT_PATH
              value: /etc/kafka-tls/ca.crt
            - name: KAFKA_SASL_USERNAME
              valueFrom:
                secretKeyRef:
                  name: kevent-relay-kafka
                  key: username
            - name: KAFKA_SASL_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: kevent-relay-kafka
                  key: password
            - name: S3_BUCKET
              value: test-kevent-jobs
            - name: S3_ACCESS_KEY
              valueFrom:
                secretKeyRef:
                  name: kevent-s3-credentials
                  key: access-key
            - name: S3_SECRET_KEY
              valueFrom:
                secretKeyRef:
                  name: kevent-s3-credentials
                  key: secret-key
            - name: ENCRYPTION_KEY
              valueFrom:
                secretKeyRef:
                  name: kevent-encryption-key
                  key: ENCRYPTION_KEY
                  optional: true
            - name: INFERENCE_TIMEOUT
              value: "3600s"
          volumeMounts:
            - name: kevent-config
              mountPath: /etc/kevent
              readOnly: true
            - name: kafka-tls
              mountPath: /etc/kafka-tls
              readOnly: true
          readinessProbe:
            httpGet:
              path: /health
              port: 8080
            initialDelaySeconds: 5
            periodSeconds: 10
          livenessProbe:
            httpGet:
              path: /health
              port: 8080
            initialDelaySeconds: 10
            periodSeconds: 30
            failureThreshold: 3
          resources:
            requests:
              cpu: "100m"
              memory: "128Mi"
            limits:
              cpu: "500m"
              memory: "256Mi"
      volumes:
        - name: model-pvc
          persistentVolumeClaim:
            claimName: whisper-1-claim
        - name: kevent-config
          configMap:
            name: kevent-relay
        - name: kafka-tls
          secret:
            secretName: kafka-cluster-ca-cert
---
apiVersion: v1
kind: Service
metadata:
  name: whisper-large-v3
  namespace: default
spec:
  type: ClusterIP
  selector:
    app: whisper-large-v3
  ports:
    - port: 9000
      targetPort: 9000
      protocol: TCP
```

- [ ] **Step 2: Commit**

```bash
git add k8s/deployment-transcription.yaml
git commit -m "feat(k8s): add Deployment + Service + RBAC for whisper-large-v3"
```

---

## Task 8: k8s — KEDA ScaledObject

**Files:**
- Create: `k8s/keda-transcription.yaml`

- [ ] **Step 1: Créer keda-transcription.yaml**

Créer `k8s/keda-transcription.yaml` :

```yaml
# KEDA ScaledObject pour le Deployment whisper-large-v3.
#
# Scale basé sur le lag du consumer group Kafka :
#   lagThreshold: "1"  → 1 message en attente = 1 pod supplémentaire
#   (cohérent avec Option A : 1 job à la fois par pod)
#
# minReplicaCount: 0 → scale-to-zero quand le topic est vide.
# Premier message reçu → KEDA crée 1 pod (~15s polling interval).
# Cold start GPU : ~30-60s selon le modèle.
# Pendant ce temps, le sync path retourne 503 (comportement attendu).
---
apiVersion: keda.sh/v1alpha1
kind: TriggerAuthentication
metadata:
  name: keda-kafka-triggerauth
  namespace: default
spec:
  secretTargetRef:
    # Réutilise le secret Kafka existant — pas de duplication de credentials.
    - parameter: username
      name: kevent-relay-kafka
      key: username
    - parameter: password
      name: kevent-relay-kafka
      key: password
    - parameter: ca
      name: kafka-cluster-ca-cert
      key: ca.crt
---
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: whisper-large-v3-scaler
  namespace: default
spec:
  scaleTargetRef:
    name: whisper-large-v3
  minReplicaCount: 0
  maxReplicaCount: 4      # ajuster selon le nombre de GPU disponibles
  pollingInterval: 15     # secondes entre chaque check du lag
  cooldownPeriod: 60      # secondes d'inactivité avant scale-down
  triggers:
    - type: kafka
      authenticationRef:
        name: keda-kafka-triggerauth
      metadata:
        bootstrapServers: default-kafka-bootstrap.infra-kafka.svc.cluster.local:9093
        consumerGroup: inference-whisper-large-v3
        topic: jobs.whisper-large-v3.input
        lagThreshold: "1"
        sasl: scram_sha512
        tls: enable
```

- [ ] **Step 2: Commit**

```bash
git add k8s/keda-transcription.yaml
git commit -m "feat(k8s): add KEDA ScaledObject for whisper-large-v3 Kafka lag scaling"
```

---

## Task 9: k8s — Supprimer les anciens manifests

**Files:**
- Delete: `k8s/inference-transcription.yaml`
- Delete: `k8s/kafka-sources.yaml`

- [ ] **Step 1: Supprimer les fichiers obsolètes**

```bash
git rm k8s/inference-transcription.yaml k8s/kafka-sources.yaml
```

- [ ] **Step 2: Commit**

```bash
git commit -m "chore(k8s): remove KServe InferenceService and KafkaSources (replaced by Deployment + KEDA)"
```

---

## Task 10: Tests finaux et build complet

- [ ] **Step 1: Tests complets relay**

```bash
cd relay && go test ./... -v
```

Expected: tous les tests PASS, aucun test skipped

- [ ] **Step 2: Vet relay**

```bash
cd relay && go vet ./...
```

Expected: no output (no errors)

- [ ] **Step 3: Tests gateway (s'assurer qu'on n'a rien cassé)**

```bash
go test ./...
```

Expected: tous les tests PASS

- [ ] **Step 4: Vérifier que le relay compile en mode production**

```bash
cd relay && CGO_ENABLED=0 GOOS=linux go build -o /tmp/relay-bin ./cmd/relay && echo "build OK"
```

Expected: `build OK`

- [ ] **Step 5: Commit final si nécessaire**

```bash
git status  # vérifier qu'il n'y a rien d'oublié
```
