package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"time"

	"kevent/relay/internal/adapter"
	"kevent/relay/internal/kafka"
	"kevent/relay/internal/lifecycle"
	"kevent/relay/internal/metrics"
	"kevent/relay/internal/model"
	"kevent/relay/internal/storage"
)

type objectStore interface {
	GetObject(ctx context.Context, key string) (io.ReadCloser, int64, string, error)
	PutObject(ctx context.Context, key string, body io.Reader, size int64, contentType string) error
	DeleteObject(ctx context.Context, key string) error
}

type eventPublisher interface {
	PublishResultEvent(ctx context.Context, topic string, event *model.ResultEvent) error
}

// Processor runs the full processing pipeline for a single InputEvent pulled from Kafka.
type Processor struct {
	adapter     adapter.Adapter
	s3          objectStore
	publisher   eventPublisher
	resultTopic string
	annotator   *lifecycle.PodAnnotator
}

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

// Process runs the full job pipeline for the given InputEvent.
// It returns an error only for transient infrastructure failures (S3, Kafka,
// network) so the caller can exit 1 and let Knative retry the Job.
// Inference errors are published as failed ResultEvents and return nil.
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

// ParseInputEvent parses an InputEvent from raw bytes.
// It detects structured CloudEvent format (has "specversion" field) and
// extracts the InputEvent from the "data" field. Otherwise the bytes are
// treated as a raw InputEvent JSON.
func ParseInputEvent(data []byte) (*model.InputEvent, error) {
	payload := data
	var probe struct {
		SpecVersion string          `json:"specversion"`
		Data        json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &probe); err == nil && probe.SpecVersion != "" {
		if probe.Data == nil {
			return nil, fmt.Errorf("structured CloudEvent has no data field")
		}
		payload = probe.Data
	}
	var event model.InputEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil, err
	}
	return &event, nil
}

// process orchestre le pipeline complet. Il retourne une erreur uniquement pour
// les pannes infrastructure (S3 indisponible, réseau) afin que le Job puisse
// sortir avec exit 1 et être retenté par Knative. Les échecs d'inférence sont
// publiés en ResultEvent et ne génèrent pas d'erreur (le job est définitivement
// terminé, en échec).
//
// Stratégie de retry : chaque étape transiente (inférence, S3 put result,
// Kafka publish result) est retentée une fois immédiatement avant de
// déléguer à Knative. Le retry inférence implique un nouveau téléchargement
// du fichier depuis S3 (le stream S3 précédent est épuisé).
// Le téléchargement initial (GetObject input) n'est pas retenté : une erreur
// infra S3 à cette étape remonte directement à Knative.
func (p *Processor) process(ctx context.Context, event *model.InputEvent) error {
	log := slog.With("job_id", event.JobID, "service_type", event.ServiceType)
	log.Info("processing job", "input_ref", event.InputRef)

	body, size, contentType, err := p.s3.GetObject(ctx, event.InputRef)
	if err != nil {
		if storage.IsNotFound(err) {
			log.Error("input file not found, publishing permanent failure", "input_ref", event.InputRef)
			metrics.JobsTotal.WithLabelValues(event.ServiceType, "failed").Inc()
			if perr := p.publishFailure(context.Background(), event, "input file not found: "+event.InputRef); perr != nil {
				return fmt.Errorf("publishing not-found failure event: %w", perr)
			}
			return nil
		}
		return fmt.Errorf("s3 get: %w", err)
	}
	defer body.Close()

	if size > 0 {
		metrics.InputSizeBytes.WithLabelValues(event.ServiceType).Observe(float64(size))
	}

	result, inferErr := p.runInference(ctx, event, body, size, contentType)
	if inferErr != nil {
		// Retry inference once immediately: re-download for a fresh stream.
		log.Warn("inference attempt failed, retrying immediately", "error", inferErr)
		body2, size2, ct2, getErr := p.s3.GetObject(context.WithoutCancel(ctx), event.InputRef)
		if getErr != nil {
			if storage.IsNotFound(getErr) {
				log.Error("input file not found on inference retry, publishing permanent failure", "input_ref", event.InputRef)
				metrics.JobsTotal.WithLabelValues(event.ServiceType, "failed").Inc()
				if perr := p.publishFailure(context.Background(), event, "input file not found on retry: "+event.InputRef); perr != nil {
					return fmt.Errorf("publishing not-found failure event: %w", perr)
				}
				return nil
			}
			return fmt.Errorf("s3 get on inference retry: %w", getErr)
		}
		defer body2.Close()
		result, inferErr = p.runInference(ctx, event, body2, size2, ct2)
	}

	if inferErr != nil {
		log.Error("inference failed", "error", inferErr)
		metrics.JobsTotal.WithLabelValues(event.ServiceType, "failed").Inc()
		if perr := p.publishFailure(context.Background(), event, fmt.Sprintf("inference: %v", inferErr)); perr != nil {
			return fmt.Errorf("publishing failure event: %w", perr)
		}
		if derr := p.s3.DeleteObject(context.Background(), event.InputRef); derr != nil {
			log.Error("failed to delete input file after failure", "input_ref", event.InputRef, "error", derr)
		}
		return nil
	}

	resultKey := event.JobID + "/result.json"
	if err := p.s3.PutObject(ctx, resultKey, bytes.NewReader(result), int64(len(result)), "application/json"); err != nil {
		log.Warn("s3 put attempt failed, retrying immediately", "error", err)
		if err := p.s3.PutObject(ctx, resultKey, bytes.NewReader(result), int64(len(result)), "application/json"); err != nil {
			return fmt.Errorf("s3 put: %w", err)
		}
	}

	resultEvent := &model.ResultEvent{
		JobID:       event.JobID,
		ServiceType: event.ServiceType,
		Status:      model.JobStatusCompleted,
		ResultRef:   resultKey,
		CompletedAt: time.Now().UTC(),
	}
	if err := p.publisher.PublishResultEvent(ctx, p.resultTopic, resultEvent); err != nil {
		log.Warn("publish result attempt failed, retrying immediately", "error", err)
		if err := p.publisher.PublishResultEvent(ctx, p.resultTopic, resultEvent); err != nil {
			log.Error("failed to publish result event after retry", "error", err)
		}
	}

	metrics.JobsTotal.WithLabelValues(event.ServiceType, "completed").Inc()
	log.Info("job completed", "result_ref", resultKey)

	if err := p.s3.DeleteObject(context.Background(), event.InputRef); err != nil {
		log.Error("failed to delete input file", "input_ref", event.InputRef, "error", err)
	}

	return nil
}

// runInference calls the adapter and records timing metrics.
func (p *Processor) runInference(ctx context.Context, event *model.InputEvent, body io.Reader, size int64, contentType string) ([]byte, error) {
	inferStart := time.Now()
	result, err := p.adapter.Call(ctx, adapter.CallInput{
		JobID:        event.JobID,
		Filename:     filepath.Base(event.InputRef),
		ContentType:  contentType,
		Size:         size,
		Body:         body,
		Model:        event.Model,
		InferenceURL: event.InferenceURL,
		Params:       event.Params,
	})
	metrics.InferenceDuration.WithLabelValues(event.ServiceType).Observe(time.Since(inferStart).Seconds())
	return result, err
}

func (p *Processor) publishFailure(ctx context.Context, event *model.InputEvent, errMsg string) error {
	resultEvent := &model.ResultEvent{
		JobID:       event.JobID,
		ServiceType: event.ServiceType,
		Status:      model.JobStatusFailed,
		Error:       errMsg,
		CompletedAt: time.Now().UTC(),
	}
	if err := p.publisher.PublishResultEvent(ctx, p.resultTopic, resultEvent); err != nil {
		slog.Error("failed to publish failure event", "job_id", event.JobID, "error", err)
		return err
	}
	return nil
}
