package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"kevent/gateway/internal/config"
	"kevent/gateway/internal/handler"
	"kevent/gateway/internal/kafka"
	_ "kevent/gateway/internal/metrics" // register Prometheus metrics
	"kevent/gateway/internal/service"
	"kevent/gateway/internal/storage"
)

// version is set at build time via -ldflags "-X main.version=v0.4.1".
var version = "dev"

func main() {
	// JSON structured logger — compatible with log aggregators (Loki, Datadog, …).
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	// ── Config ────────────────────────────────────────────────────────────────
	cfgPath := "config.yaml"
	if v := os.Getenv("CONFIG_PATH"); v != "" {
		cfgPath = v
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// ── Service registry ──────────────────────────────────────────────────────
	registry := service.NewRegistry(cfg.Services)
	slog.Info("service registry initialised", "types", registry.Types())

	// ── Dependencies ──────────────────────────────────────────────────────────
	s3Client, err := storage.NewS3Client(cfg.S3, cfg.Encryption)
	if err != nil {
		slog.Error("failed to initialise S3 storage", "error", err)
		os.Exit(1)
	}
	slog.Info("S3 storage initialised", "encryption", cfg.Encryption.Key != "")

	redisClient, err := storage.NewRedis(cfg.Redis)
	if err != nil {
		slog.Error("failed to initialise Redis", "error", err)
		os.Exit(1)
	}
	defer redisClient.Close()

	var producer *kafka.Producer
	if registry.HasKafkaServices() {
		producer, err = kafka.NewProducer(cfg.Kafka)
		if err != nil {
			slog.Error("failed to initialise Kafka producer", "error", err)
			os.Exit(1)
		}
		defer producer.Close()
		slog.Info("Kafka producer initialised")
	}

	// ── HTTP router ───────────────────────────────────────────────────────────
	jobHandler := handler.NewJobHandler(registry, s3Client, redisClient, producer)

	r := chi.NewRouter()
	r.Use(chimw.RequestID)
	r.Use(chimw.RealIP)
	r.Use(handler.StructuredLogger(logger))
	r.Use(chimw.Recoverer)

	spec := handler.GenerateSpec(registry, version)
	swaggerSpecs := handler.FetchSwaggerSpecs(cfg.Services)

	r.Get("/health", handler.Health)
	r.Get("/metrics", promhttp.Handler().ServeHTTP)
	r.Get("/docs", handler.DocsUI(swaggerSpecs))
	r.Get("/openapi.yaml", handler.NewDocsSpec(spec))
	r.Get("/swagger/{type}/{model}", handler.NewSwaggerHandler(swaggerSpecs))
	r.Post("/jobs/{service_type}", jobHandler.Submit)
	r.Get("/jobs/{service_type}/{id}", jobHandler.GetStatus)

	// OpenAI-compatible sync endpoints — routed by the "model" field in the
	// request body or extracted from the URL for pattern paths like
	// /v2/models/{model}/infer. Routes are registered dynamically from config
	// so no code change is needed when adding new endpoint paths or versions.
	if registry.HasSyncServices() {
		syncHandler := handler.NewSyncHandler(registry, s3Client, redisClient, producer)
		r.Get("/v1/models", handler.ListModels(registry))
		for _, prefix := range registry.SyncPathPrefixes() {
			r.Post(prefix+"/*", syncHandler.ServeHTTP)
		}
		slog.Info("sync proxy enabled", "paths", registry.SyncPaths())
	}

	// ── Result consumers ──────────────────────────────────────────────────────
	// One goroutine per registered service result topic, all sharing the same
	// context so they stop cleanly on shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if registry.HasKafkaServices() {
		consumerManager, err := kafka.NewConsumerManager(cfg.Kafka, registry, redisClient, s3Client, logger)
		if err != nil {
			slog.Error("failed to initialise Kafka consumer manager", "error", err)
			os.Exit(1)
		}
		consumerManager.Start(ctx)
	}

	// ── HTTP server ───────────────────────────────────────────────────────────
	srv := &http.Server{
		Addr:         cfg.Server.Addr,
		Handler:      r,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		IdleTimeout:  cfg.Server.IdleTimeout,
	}

	serverErr := make(chan error, 1)
	go func() {
		slog.Info("server starting", "addr", cfg.Server.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()

	// ── Graceful shutdown ─────────────────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		slog.Error("server error", "error", err)
	case sig := <-quit:
		slog.Info("shutdown signal received", "signal", sig)
	}

	slog.Info("shutting down…")
	cancel() // stop all Kafka consumers

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("server forced to shutdown", "error", err)
	}

	slog.Info("server stopped")
}
