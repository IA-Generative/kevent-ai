package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// RequestsTotal counts all completed requests labelled by mode (async/sync),
	// service_type, model, and HTTP status code.
	RequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kevent_requests_total",
		Help: "Total number of requests handled by the gateway.",
	}, []string{"mode", "service_type", "model", "status"})

	// RequestDuration measures end-to-end handler latency.
	RequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "kevent_request_duration_seconds",
		Help:    "End-to-end request duration in seconds.",
		Buckets: []float64{.1, .5, 1, 5, 10, 30, 60, 120, 300},
	}, []string{"mode", "service_type", "model"})

	// SyncWaitDuration measures the time the gateway spends blocked on the Redis
	// pub/sub notification in sync-over-Kafka mode.
	SyncWaitDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "kevent_sync_wait_duration_seconds",
		Help:    "Time spent waiting for sync-over-Kafka job results (Redis pub/sub).",
		Buckets: []float64{.5, 1, 5, 10, 30, 60, 120, 300},
	}, []string{"service_type", "model"})

	// SyncJobsInFlight tracks the number of sync-over-Kafka connections that are
	// currently open and waiting for relay results.
	SyncJobsInFlight = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "kevent_sync_jobs_in_flight",
		Help: "Number of sync-over-Kafka jobs currently waiting for results.",
	})

	// S3OperationDuration measures latency for each S3 operation (upload/get/delete).
	S3OperationDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "kevent_s3_operation_duration_seconds",
		Help:    "S3 operation duration in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"operation"})

	// S3ErrorsTotal counts S3 operation failures.
	S3ErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kevent_s3_errors_total",
		Help: "Total number of S3 operation errors.",
	}, []string{"operation"})

	// KafkaPublishDuration measures Kafka WriteMessages latency per topic.
	KafkaPublishDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "kevent_kafka_publish_duration_seconds",
		Help:    "Kafka publish duration in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"topic"})

	// KafkaPublishErrorsTotal counts Kafka publish failures per topic.
	KafkaPublishErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "kevent_kafka_publish_errors_total",
		Help: "Total number of Kafka publish errors.",
	}, []string{"topic"})
)
