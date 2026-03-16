package dispatcher

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"kevent/dispatcher/internal/adapter"
	"kevent/dispatcher/internal/kafka"
	"kevent/dispatcher/internal/model"
	"kevent/dispatcher/internal/storage"
)

// Dispatcher handles incoming CloudEvent HTTP requests from KafkaSource.
//
// En tant que sidecar, le handler est synchrone : il bloque jusqu'à la fin du
// job et retourne 200 ou 500. Knative Pod Autoscaler mesure la durée de la
// requête HTTP en vol pour calibrer le nombre de replicas (= pods GPU actifs).
// containerConcurrency dans le Knative Service spec remplace le sémaphore.
type Dispatcher struct {
	adapter      adapter.Adapter
	s3           *storage.S3Client
	publisher    *kafka.Publisher
	resultTopic  string
	syncPriority atomic.Int32 // 1 = sync job in progress, 0 = idle
}

func New(
	adp adapter.Adapter,
	s3 *storage.S3Client,
	pub *kafka.Publisher,
	resultTopic string,
) *Dispatcher {
	return &Dispatcher{
		adapter:     adp,
		s3:          s3,
		publisher:   pub,
		resultTopic: resultTopic,
	}
}

// ServeHTTP is the async CloudEvent handler (KafkaSource → POST /).
//
// Returns 503 when a priority sync job is in progress so KafkaSource retries
// after its configured backoffDelay — giving sync jobs the GPU first.
//
// Sémantique des codes retour :
//   - 200 : job traité — pas de retry
//   - 400 : message malformé — pas de retry
//   - 503 : sync job en cours — KafkaSource doit retenter après backoffDelay
//   - 500 : erreur infra transitoire — KafkaSource doit retenter
func (d *Dispatcher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if d.syncPriority.Load() == 1 {
		slog.Info("async job deferred: sync job in progress")
		http.Error(w, "sync job in progress, retry later", http.StatusServiceUnavailable)
		return
	}
	d.serveHTTP(w, r)
}

// serveHTTP is the shared CloudEvent processing implementation used by both
// ServeHTTP (async) and ServeHTTPSync (priority).
func (d *Dispatcher) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	event, err := decodeInputEvent(r)
	if err != nil {
		slog.Warn("failed to decode input event", "error", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if event.JobID == "" {
		slog.Warn("received event with empty job_id")
		http.Error(w, "missing job_id", http.StatusBadRequest)
		return
	}

	slog.Info("event received", "job_id", event.JobID, "service_type", event.ServiceType, "input_ref", event.InputRef)

	if err := d.process(r.Context(), event); err != nil {
		slog.Error("transient error, letting KafkaSource retry", "job_id", event.JobID, "error", err)
		http.Error(w, "transient error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// process orchestre le pipeline complet. Il retourne une erreur uniquement pour
// les pannes infrastructure (S3 indisponible, réseau) afin que KafkaSource
// puisse retenter. Les échecs d'inférence sont publiés en ResultEvent et ne
// génèrent pas d'erreur (le job est définitivement terminé, en échec).
func (d *Dispatcher) process(ctx context.Context, event *model.InputEvent) error {
	log := slog.With("job_id", event.JobID, "service_type", event.ServiceType)
	log.Info("processing job", "input_ref", event.InputRef)

	body, size, contentType, err := d.s3.GetObject(ctx, event.InputRef)
	if err != nil {
		// Erreur transitoire : pas de ResultEvent, KafkaSource retentera.
		return fmt.Errorf("s3 get: %w", err)
	}
	defer body.Close()

	result, err := d.adapter.Call(ctx, adapter.CallInput{
		JobID:        event.JobID,
		Filename:     filepath.Base(event.InputRef),
		ContentType:  contentType,
		Size:         size,
		Body:         body,
		Model:        event.Model,
		InferenceURL: event.InferenceURL,
	})
	if err != nil {
		// Échec métier (modèle, fichier invalide…) : on publie le failure et on
		// retourne nil pour que KafkaSource ne retente pas.
		log.Error("inference failed", "error", err)
		d.publishFailure(ctx, event, fmt.Sprintf("inference: %v", err))
		if derr := d.s3.DeleteObject(ctx, event.InputRef); derr != nil {
			log.Error("failed to delete input file after failure", "input_ref", event.InputRef, "error", derr)
		}
		return nil
	}

	resultKey := event.JobID + "/result.json"
	if err := d.s3.PutObject(ctx, resultKey, bytes.NewReader(result), int64(len(result)), "application/json"); err != nil {
		// Le résultat est en RAM et le job n'est pas encore commité : on laisse
		// KafkaSource retenter pour qu'on puisse re-tenter le PutObject.
		return fmt.Errorf("s3 put: %w", err)
	}

	resultEvent := &model.ResultEvent{
		JobID:       event.JobID,
		ServiceType: event.ServiceType,
		Status:      model.JobStatusCompleted,
		ResultRef:   resultKey,
		CompletedAt: time.Now().UTC(),
	}
	if err := d.publisher.PublishResultEvent(ctx, d.resultTopic, resultEvent); err != nil {
		// Le résultat est persisté en S3. On logue l'erreur mais on ne retente
		// pas le job entier : le gateway pourra interroger S3 directement si besoin.
		log.Error("failed to publish result event", "error", err)
	}

	// Supprimer le fichier d'entrée maintenant que le résultat est stocké.
	if err := d.s3.DeleteObject(ctx, event.InputRef); err != nil {
		log.Error("failed to delete input file", "input_ref", event.InputRef, "error", err)
	}

	log.Info("job completed", "result_ref", resultKey)
	return nil
}

// decodeInputEvent reads an InputEvent from the HTTP request body.
// It handles both CloudEvent delivery modes:
//   - Binary mode (default): body is the raw Kafka message (JSON InputEvent directly)
//   - Structured mode (Content-Type: application/cloudevents+json): body is a
//     CloudEvent JSON envelope; the InputEvent is nested inside the "data" field.
func decodeInputEvent(r *http.Request) (*model.InputEvent, error) {
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(io.LimitReader(r.Body, 1<<20)); err != nil {
		return nil, fmt.Errorf("reading body: %w", err)
	}

	payload := buf.Bytes()

	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/cloudevents+json") {
		// Structured CloudEvent — InputEvent is nested inside "data".
		var envelope struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(payload, &envelope); err != nil {
			return nil, fmt.Errorf("structured CloudEvent envelope: %w", err)
		}
		payload = envelope.Data
	}

	var event model.InputEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil, err
	}
	return &event, nil
}

func (d *Dispatcher) publishFailure(ctx context.Context, event *model.InputEvent, errMsg string) {
	resultEvent := &model.ResultEvent{
		JobID:       event.JobID,
		ServiceType: event.ServiceType,
		Status:      model.JobStatusFailed,
		Error:       errMsg,
		CompletedAt: time.Now().UTC(),
	}
	if err := d.publisher.PublishResultEvent(ctx, d.resultTopic, resultEvent); err != nil {
		slog.Error("failed to publish failure event", "job_id", event.JobID, "error", err)
	}
}
