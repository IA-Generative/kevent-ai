package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"kevent/relay/internal/adapter"
	"kevent/relay/internal/config"
	_ "kevent/relay/internal/metrics" // register Prometheus metrics
	"kevent/relay/internal/relay"
	"kevent/relay/internal/kafka"
	"kevent/relay/internal/storage"
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
		"result_topic", cfg.Service.ResultTopic,
		"inference_base_url", cfg.Inference.BaseURL,
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

	disp := relay.New(adp, s3Client, publisher, cfg.Service.ResultTopic)

	inferenceAddr := inferenceHostPort(cfg)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
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

	// / — async CloudEvent handler for KafkaSource (always POSTs to exactly "/").
	// Any other path (e.g. /v1/audio/transcriptions) is a direct inference request
	// from the gateway sync-direct path; proxy it straight to the local model.
	inferenceProxy := newInferenceProxy(cfg.Inference.BaseURL)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			inferenceProxy.ServeHTTP(w, r)
			return
		}
		disp.ServeHTTP(w, r)
	})

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

// newInferenceProxy returns a reverse proxy that forwards requests to the local
// inference model (base_url). Used for sync-direct requests that arrive on paths
// other than "/" (which is reserved for async CloudEvents from KafkaSource).
func newInferenceProxy(baseURL string) *httputil.ReverseProxy {
	target, err := url.Parse(baseURL)
	if err != nil {
		slog.Error("invalid inference base_url for proxy", "url", baseURL, "error", err)
		// Return a proxy that always errors; the process should have exited earlier.
		target = &url.URL{Scheme: "http", Host: "127.0.0.1:9000"}
	}
	return httputil.NewSingleHostReverseProxy(target)
}

// inferenceHostPort extracts host:port from inference.base_url (e.g. "127.0.0.1:9000").
// Used by the /health TCP readiness check.
func inferenceHostPort(cfg *config.Config) string {
	u, err := url.Parse(cfg.Inference.BaseURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}
