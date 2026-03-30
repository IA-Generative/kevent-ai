package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// JobsTotal counts all completed jobs labelled by service_type and outcome
	// (completed / failed / deferred).
	JobsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kevent_relay_jobs_total",
		Help: "Total number of jobs processed by the relay.",
	}, []string{"service_type", "status"})

	// InferenceDuration measures time spent in the adapter.Call (inference API).
	InferenceDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "kevent_relay_inference_duration_seconds",
		Help:    "Inference call duration in seconds.",
		Buckets: []float64{.5, 1, 5, 10, 30, 60, 120, 300, 600},
	}, []string{"service_type"})

	// InputSizeBytes tracks the size of input files downloaded from S3.
	// Uses an exponential scale from ~1 KB to ~1 GB.
	InputSizeBytes = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "kevent_relay_input_size_bytes",
		Help:    "Input file size in bytes.",
		Buckets: prometheus.ExponentialBuckets(1024, 4, 10),
	}, []string{"service_type"})

	// SyncPriority mirrors the relay's internal syncPriority counter.
	// Non-zero means at least one sync job is in progress on this pod.
	SyncPriority = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "kevent_relay_sync_priority",
		Help: "Number of sync jobs currently in progress (non-zero defers async jobs).",
	})

	// DeferredTotal counts async jobs returned with 503 due to sync priority.
	DeferredTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "kevent_relay_deferred_total",
		Help: "Total number of async jobs deferred because a sync job was in progress.",
	})

	// S3OperationDuration measures S3 latency per operation (get/put/delete).
	S3OperationDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "kevent_relay_s3_operation_duration_seconds",
		Help:    "S3 operation duration in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"operation"})

	// S3ErrorsTotal counts S3 operation failures.
	S3ErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kevent_relay_s3_errors_total",
		Help: "Total number of S3 operation errors.",
	}, []string{"operation"})

	// KafkaPublishErrorsTotal counts result-event publish failures.
	KafkaPublishErrorsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "kevent_relay_kafka_publish_errors_total",
		Help: "Total number of Kafka result-event publish errors.",
	})
)
