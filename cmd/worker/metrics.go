package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/metrics"
)

// serveMetrics exposes the worker's Prometheus endpoint. The worker serves no
// API, so this is its only listener. It is behind the same admin key as the
// API's, because queue depths and failure rates are operational detail.
func serveMetrics(ctx context.Context, registry *metrics.Metrics, port int, adminKey string, logger *slog.Logger) error {
	if strings.TrimSpace(adminKey) == "" {
		logger.Warn("no admin key configured: worker metrics are not exposed", "port", port)
		<-ctx.Done()
		return nil
	}

	mux := http.NewServeMux()
	mux.Handle("GET /metrics", requireAdminKey(adminKey, registry.Handler()))
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

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

func requireAdminKey(adminKey string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Admin-Key")), []byte(adminKey)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
