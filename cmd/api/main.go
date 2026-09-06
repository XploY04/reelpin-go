package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/XploY04/reelpin-go/internal/auth"
	"github.com/XploY04/reelpin-go/internal/config"
	"github.com/XploY04/reelpin-go/internal/db"
	"github.com/XploY04/reelpin-go/internal/enqueue"
	"github.com/XploY04/reelpin-go/internal/httpapi"
	"github.com/XploY04/reelpin-go/internal/postgres"
	"github.com/XploY04/reelpin-go/internal/ratelimit"
	"github.com/XploY04/reelpin-go/internal/safehttp"
	"github.com/XploY04/reelpin-go/internal/sharetoken"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
	"github.com/redis/go-redis/v9"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if err := run(logger); err != nil {
		logger.Error("startup failed", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("db connect: %w", err)
	}
	defer pool.Close()

	verifier, err := auth.NewVerifier(ctx, cfg.SupabaseURL, cfg.SupabaseJWTAudience)
	if err != nil {
		return fmt.Errorf("supabase jwks: %w", err)
	}
	// Runs before the pool closes, after the server has stopped serving.
	defer func() {
		if err := verifier.Shutdown(context.Background()); err != nil {
			logger.Error("jwks cache shutdown failed", "error", err)
		}
	}()

	// Redis is optional outside production: without it there are no limits, and
	// submissions fail closed while every read keeps serving.
	var limiter httpapi.RateLimiter
	if cfg.RedisURL != "" {
		options, err := cfg.RedisOptions()
		if err != nil {
			return err
		}
		client := redis.NewClient(options)
		defer client.Close()
		if err := client.Ping(ctx).Err(); err != nil {
			return fmt.Errorf("redis connect: %w", err)
		}
		limiter = ratelimit.New(client, cfg.RedisKeyPrefix, ratelimit.NewHasher(cfg.RateLimitSalt))
	} else {
		logger.Warn("no REDIS_URL: submissions fail closed, reads are unaffected",
			"environment", cfg.Environment)
	}

	// One resolver serves both preview and submission, so a link that previews
	// resolves identically when it is submitted.
	resolver := &sourceidentity.Resolver{Redirects: safehttp.New(safehttp.Config{})}
	shareTokens := sharetoken.NewStore(pool)

	srv := &http.Server{
		Addr: fmt.Sprintf(":%d", cfg.Port),
		Handler: httpapi.New(httpapi.Deps{
			DB:          pool,
			Auth:        verifier,
			Reels:       postgres.NewReels(pool),
			Jobs:        postgres.NewJobs(pool),
			Enqueue:     enqueue.New(postgres.NewEnqueue(pool), resolver),
			ShareTokens: shareTokens,
			Resolver:    resolver,
			Limiter:     limiter,
			Logger:      logger,
			Version:     cfg.Version,
		}).Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("listening", "port", cfg.Port, "environment", cfg.Environment, "version", cfg.Version)
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
	}
	return nil
}
