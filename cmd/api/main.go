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
	"github.com/XploY04/reelpin-go/internal/cache"
	"github.com/XploY04/reelpin-go/internal/collections"
	"github.com/XploY04/reelpin-go/internal/config"
	"github.com/XploY04/reelpin-go/internal/db"
	"github.com/XploY04/reelpin-go/internal/embed"
	"github.com/XploY04/reelpin-go/internal/enqueue"
	"github.com/XploY04/reelpin-go/internal/httpapi"
	"github.com/XploY04/reelpin-go/internal/lifecycle"
	"github.com/XploY04/reelpin-go/internal/mapview"
	"github.com/XploY04/reelpin-go/internal/notify"
	"github.com/XploY04/reelpin-go/internal/platform/social"
	"github.com/XploY04/reelpin-go/internal/postgres"
	"github.com/XploY04/reelpin-go/internal/ratelimit"
	"github.com/XploY04/reelpin-go/internal/safehttp"
	"github.com/XploY04/reelpin-go/internal/search"
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

// limiterOrNil keeps a nil *ratelimit.Limiter from becoming a non-nil
// interface holding nil, which would look configured and then panic.
func limiterOrNil(limiter *ratelimit.Limiter) httpapi.RateLimiter {
	if limiter == nil {
		return nil
	}
	return limiter
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

	// Redis is optional outside production: without it there are no limits and
	// no caches, and every read still works.
	var (
		limiter       *ratelimit.Limiter
		responseCache *cache.Cache
	)
	if cfg.RedisURL != "" {
		options, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			return fmt.Errorf("redis url: %w", err)
		}
		client := redis.NewClient(options)
		defer client.Close()

		if err := client.Ping(ctx).Err(); err != nil {
			return fmt.Errorf("redis connect: %w", err)
		}
		limiter = ratelimit.New(client, cfg.RateLimitPrefix())
		responseCache = cache.New(client, cfg.CachePrefix())
	} else {
		logger.Warn("no REDIS_URL configured: rate limits and caches are disabled",
			"environment", cfg.Environment)
	}

	// One resolver serves both share resolution and enqueue, so a link that
	// resolves in the app resolves identically when it is shared.
	safeClient := safehttp.New(safehttp.Config{})
	shareResolver := &sourceidentity.Resolver{
		Redirects: safeClient,
		// Reddit mobile share links need the authenticated API to resolve, and
		// the resolver only calls it for that exact link shape.
		Reddit: social.NewRedditClient(
			cfg.RedditClientID, cfg.RedditClientSecret, cfg.RedditUserAgent, safeClient),
	}

	srv := &http.Server{
		Addr: fmt.Sprintf(":%d", cfg.Port),
		Handler: httpapi.New(httpapi.Deps{
			DB:          pool,
			Auth:        verifier,
			Reels:       postgres.NewReels(pool),
			Jobs:        postgres.NewJobs(pool),
			Share:       shareResolver,
			Enqueue:     enqueue.New(pool, shareResolver, enqueue.DefaultLimits),
			ShareTokens: sharetoken.NewStore(pool),
			Collections: collections.New(pool, cfg.CollectionShareBaseURL, time.Now),
			Notifications: notify.NewService(pool,
				notify.NewFCM(cfg.FirebaseCredentialsJSON, cfg.FirebaseProjectID, 0), logger, time.Now),
			AdminKey: cfg.AdminKey,
			// The identity itself is deleted through Supabase, which this
			// service does not own; until that adapter exists the account
			// delete removes everything else and reports honestly.
			Lifecycle: lifecycle.New(pool, nil, responseCache, logger),
			Map: mapview.NewService(pool,
				mapview.NewGooglePlaces(cfg.GooglePlacesAPIKey, 0), time.Now),
			Search: search.NewService(pool,
				embed.NewGemini(cfg.GeminiAPIKey, 0), logger, time.Now),
			Limiter:        limiterOrNil(limiter),
			Cache:          responseCache,
			TrustedProxies: cfg.TrustedProxyCIDRs,
			Logger:         logger,
			Version:        cfg.Version,
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
