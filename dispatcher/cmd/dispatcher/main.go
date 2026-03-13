package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"kevent/dispatcher/internal/adapter"
	"kevent/dispatcher/internal/config"
	"kevent/dispatcher/internal/dispatcher"
	"kevent/dispatcher/internal/kafka"
	"kevent/dispatcher/internal/storage"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
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

	slog.Info("sidecar starting",
		"service_type", cfg.Service.Type,
		"result_topic", cfg.Service.ResultTopic,
	)

	s3Client, err := storage.NewS3Client(cfg.S3, cfg.Encryption)
	if err != nil {
		slog.Error("failed to initialise S3 client", "error", err)
		os.Exit(1)
	}
	slog.Info("S3 storage initialised", "encryption", cfg.Encryption.Key != "")

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

	disp := dispatcher.New(adp, s3Client, publisher, cfg.Service.ResultTopic)

	inferenceAddr := inferenceHostPort(cfg)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		if inferenceAddr != "" {
			conn, err := net.DialTimeout("tcp", inferenceAddr, time.Second)
			if err != nil {
				http.Error(w, "inference not ready", http.StatusServiceUnavailable)
				return
			}
			conn.Close()
		}
		w.WriteHeader(http.StatusOK)
	})

	// /sync — priority CloudEvent handler for sync-over-Kafka jobs.
	// Sets syncPriority flag so async jobs on the same pod are deferred (503).
	mux.HandleFunc("/sync", disp.ServeHTTPSync)

	mux.Handle("/", disp) // async CloudEvent handler (KafkaSource → POST /)

	srv := &http.Server{
		Addr:        ":8080",
		Handler:     mux,
		ReadTimeout: 30 * time.Second,
		// WriteTimeout désactivé : le handler bloque pendant toute la durée de
		// l'inférence (jusqu'à 10 min). Le timeout est géré par Knative via
		// spec.template.spec.timeoutSeconds sur le Service.
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		slog.Info("server listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		slog.Error("server error", "error", err)
	case sig := <-quit:
		slog.Info("shutdown signal received", "signal", sig)
	}

	slog.Info("shutting down…")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("server forced to shutdown", "error", err)
	}

	slog.Info("server stopped")
}

// inferenceHostPort extracts host:port from the active service's endpoint URL
// (e.g. "127.0.0.1:9000"). Used by the /health TCP readiness check.
func inferenceHostPort(cfg *config.Config) string {
	u, err := url.Parse(inferenceEndpointURL(cfg))
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}

// inferenceEndpointURL returns the full configured endpoint URL for the active
// service type.
func inferenceEndpointURL(cfg *config.Config) string {
	switch cfg.Service.Type {
	case "transcription":
		return cfg.Transcription.EndpointURL
	case "diarization":
		return cfg.Diarization.EndpointURL
	case "ocr":
		return cfg.OCR.EndpointURL
	}
	return ""
}
