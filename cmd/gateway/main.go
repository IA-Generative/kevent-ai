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

	"kevent/gateway/internal/config"
	"kevent/gateway/internal/handler"
	"kevent/gateway/internal/kafka"
	"kevent/gateway/internal/service"
	"kevent/gateway/internal/storage"
)

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

	producer, err := kafka.NewProducer(cfg.Kafka)
	if err != nil {
		slog.Error("failed to initialise Kafka producer", "error", err)
		os.Exit(1)
	}
	defer producer.Close()

	// ── HTTP router ───────────────────────────────────────────────────────────
	jobHandler := handler.NewJobHandler(registry, s3Client, redisClient, producer)

	r := chi.NewRouter()
	r.Use(chimw.RequestID)
	r.Use(chimw.RealIP)
	r.Use(handler.StructuredLogger(logger))
	r.Use(chimw.Recoverer)

	r.Get("/health", handler.Health)
	r.Get("/docs", handler.DocsUI)
	r.Get("/openapi.yaml", handler.DocsSpec)
	r.Post("/jobs/{service_type}", jobHandler.Submit)
	r.Get("/jobs/{service_type}/{id}", jobHandler.GetStatus)

	// OpenAI-compatible sync endpoints — routed by the "model" field in the payload.
	if registry.HasSyncServices() {
		syncHandler := handler.NewSyncHandler(registry)
		r.Get("/v1/models", handler.ListModels(registry))
		r.Post("/v1/*", syncHandler.ServeHTTP)
		slog.Info("sync proxy enabled", "paths", registry.SyncPaths())
	}

	// ── Result consumers ──────────────────────────────────────────────────────
	// One goroutine per registered service result topic, all sharing the same
	// context so they stop cleanly on shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	consumerManager, err := kafka.NewConsumerManager(cfg.Kafka, registry, redisClient, s3Client, logger)
	if err != nil {
		slog.Error("failed to initialise Kafka consumer manager", "error", err)
		os.Exit(1)
	}
	consumerManager.Start(ctx)

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
