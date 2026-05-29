package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"kevent/relay/internal/adapter"
	"kevent/relay/internal/config"
	"kevent/relay/internal/kafka"
	"kevent/relay/internal/relay"
	"kevent/relay/internal/storage"
)

const jobsinkEventPath = "/etc/jobsink-event/event"

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

	adp, err := adapter.New(cfg)
	if err != nil {
		slog.Error("failed to initialise adapter", "error", err)
		os.Exit(1)
	}

	proc := relay.New(adp, s3Client, publisher, cfg.Service.ResultTopic)

	inferenceHealthURL := strings.TrimRight(cfg.Inference.BaseURL, "/") + "/health"
	healthClient := &http.Client{Timeout: cfg.Inference.HealthCheckTimeoutDuration()}
	waitForInference(inferenceHealthURL, healthClient, cfg.Inference.ReadyTimeoutDuration(), cfg.Inference.ReadyIntervalDuration())

	eventData, err := os.ReadFile(jobsinkEventPath)
	if err != nil {
		slog.Error("failed to read jobsink event", "path", jobsinkEventPath, "error", err)
		os.Exit(1)
	}

	event, err := relay.ParseInputEvent(eventData)
	if err != nil {
		slog.Error("failed to parse input event", "error", err)
		os.Exit(1)
	}
	if event.JobID == "" {
		slog.Error("input event missing job_id")
		os.Exit(1)
	}

	slog.Info("relay job starting",
		"job_id", event.JobID,
		"service_type", event.ServiceType,
		"inference_base_url", cfg.Inference.BaseURL,
	)

	if err := proc.Process(context.Background(), event); err != nil {
		slog.Error("job failed (system error)", "job_id", event.JobID, "error", err)
		os.Exit(1)
	}

	slog.Info("job done", "job_id", event.JobID)
}

func waitForInference(healthURL string, client *http.Client, timeout, interval time.Duration) {
	slog.Info("waiting for inference service", "health_url", healthURL, "timeout", timeout)
	deadline := time.Now().Add(timeout)
	for {
		resp, err := client.Get(healthURL)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				slog.Info("inference service ready")
				return
			}
		}
		if time.Now().After(deadline) {
			slog.Error("inference service did not become ready within timeout", "health_url", healthURL)
			os.Exit(1)
		}
		slog.Info("inference not ready yet, retrying", "health_url", healthURL, "interval", interval)
		time.Sleep(interval)
	}
}
