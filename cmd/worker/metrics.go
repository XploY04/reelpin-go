package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/XploY04/reelpin-go/internal/metrics"
)

// serveMetrics exposes the worker's Prometheus endpoint. The worker serves no
// API, so this is its only listener, and it is behind the same admin key as the
// API's: queue depths and failure rates are operational detail.
func serveMetrics(ctx context.Context, registry *metrics.Metrics, port int, adminKey string, logger *slog.Logger) error {
	if adminKey == "" {
		logger.Warn("no ADMIN_KEY: worker metrics are collected but not served", "port", port)
		<-ctx.Done()
		return nil
	}

	mux := http.NewServeMux()
	mux.Handle("GET /metrics", registry.GuardedHandler(adminKey))

	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	logger.Info("worker metrics listening", "port", port)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("metrics server: %w", err)
	}
	return nil
}
