package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/XploY04/reelpin-go/internal/auth"
	"github.com/XploY04/reelpin-go/internal/config"
	"github.com/XploY04/reelpin-go/internal/db"
	"github.com/XploY04/reelpin-go/internal/embed"
	"github.com/XploY04/reelpin-go/internal/enqueue"
	"github.com/XploY04/reelpin-go/internal/httpapi"
	"github.com/XploY04/reelpin-go/internal/lifecycle"
	"github.com/XploY04/reelpin-go/internal/mapview"
	"github.com/XploY04/reelpin-go/internal/metrics"
	"github.com/XploY04/reelpin-go/internal/notify"
	"github.com/XploY04/reelpin-go/internal/postgres"
	"github.com/XploY04/reelpin-go/internal/ratelimit"
	"github.com/XploY04/reelpin-go/internal/safehttp"
	"github.com/XploY04/reelpin-go/internal/search"
	"github.com/XploY04/reelpin-go/internal/sharetoken"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
	"github.com/XploY04/reelpin-go/internal/spend"
	"github.com/XploY04/reelpin-go/internal/workerhealth"
	"github.com/redis/go-redis/v9"
)

type pingFunc func(ctx context.Context) error

func (f pingFunc) Ping(ctx context.Context) error { return f(ctx) }

type workerCountFunc metrics.WorkerCount

func (f workerCountFunc) LiveWorkers(ctx context.Context) (int, error) { return f(ctx) }

// workerCounter keeps a nil count function from becoming a non-nil interface
// holding nil, which would look configured and then report an error on every
// readiness check.
func workerCounter(count metrics.WorkerCount) httpapi.WorkerCounter {
	if count == nil {
		return nil
	}
	return workerCountFunc(count)
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	// This binary takes no arguments. Being handed some means something meant
	// to run a different command inside the image and forgot --entrypoint, and
	// silently serving instead would hide it.
	if len(os.Args) > 1 {
		logger.Error("this command takes no arguments",
			"args", os.Args[1:],
			"hint", "to run another binary in this image, pass --entrypoint")
		os.Exit(2)
	}

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

	meters := metrics.New()

	// Redis is optional outside production: without it there are no limits, and
	// submissions fail closed while every read keeps serving.
	var (
		limiter     httpapi.RateLimiter
		redisPinger httpapi.Pinger
		liveWorkers metrics.WorkerCount
	)
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
		redisPinger = pingFunc(func(ctx context.Context) error { return client.Ping(ctx).Err() })
		liveWorkers = func(ctx context.Context) (int, error) {
			return workerhealth.Live(ctx, client, cfg.RedisKeyPrefix)
		}
	} else {
		logger.Warn("no REDIS_URL: submissions fail closed, reads are unaffected",
			"environment", cfg.Environment)
	}

	// One resolver serves both preview and submission, so a link that previews
	// resolves identically when it is submitted.
	resolver := &sourceidentity.Resolver{Redirects: safehttp.New(safehttp.Config{})}
	shareTokens := sharetoken.NewStore(pool)

	// Every search embeds its query, so the API spends money too and writes the
	// same ledger the worker does.
	prices, err := spend.ParsePricesOrNone(cfg.CostGatePrices)
	if err != nil {
		return fmt.Errorf("COST_GATE_PRICES: %w", err)
	}
	ledger := postgres.NewSpend(pool)

	// Without a key the embedder answers ErrNotConfigured, which search treats
	// as one arm being unavailable rather than as a failure.
	embedder := embed.NewGemini(embed.GeminiConfig{
		APIKey:    cfg.GeminiAPIKey,
		Model:     cfg.EmbeddingModel,
		Dimension: cfg.EmbeddingDimension,
		Usage:     spend.NewLedger(ledger, prices, meters, logger),
	})
	if cfg.AdminKey == "" {
		logger.Warn("no ADMIN_KEY: metrics are collected but /metrics is not served")
	}
	go metrics.Sample(ctx, meters, pool, liveWorkers)

	// The API is the one process that publishes the fleet's month-to-date
	// spend, whether or not a gate is configured: watching it is how the
	// limits get chosen.
	go metrics.SampleSpend(ctx, meters, func(ctx context.Context) (float64, error) {
		total, err := ledger.MonthToDateMicros(ctx)
		return total.USD(), err
	})

	gate, err := costGate(cfg, ledger, meters, logger)
	if err != nil {
		return err
	}

	searchService := search.NewService(pool, embedder, logger, time.Now)
	searchService.Metrics = meters

	// Without a service-role key the account deletion still removes every row
	// and then reports that the identity is still there, which the app already
	// handles; inventing a deletion would be the only worse answer.
	if cfg.SupabaseServiceRoleKey == "" {
		logger.Warn("no SUPABASE_SERVICE_ROLE_KEY: an account deletion removes the data, leaves the sign-in working, and stays pending")
	}

	api, err := httpapi.New(httpapi.Deps{
		DB:             pool,
		Auth:           verifier,
		Reels:          postgres.NewReels(pool),
		Jobs:           postgres.NewCombinedJobs(pool),
		Enqueue:        enqueue.New(postgres.NewEnqueue(pool), resolver, optionalGate(gate)),
		ShareTokens:    shareTokens,
		Resolver:       resolver,
		Collections:    postgres.NewCollections(pool, cfg.ShareBaseURL, time.Now),
		Lifecycle:      lifecycle.New(pool, auth.NewAdmin(cfg.SupabaseURL, cfg.SupabaseServiceRoleKey), nil, logger),
		Map:            mapview.New(pool, time.Now),
		Notifications:  notify.NewService(pool, notify.NewFCM(cfg.FirebaseCredentialsJSON, cfg.FirebaseProjectID, 0), logger, time.Now),
		Search:         searchService,
		Limiter:        limiter,
		Metrics:        meters,
		AdminKey:       cfg.AdminKey,
		IPBucketSecret: cfg.IPBucketSecret,
		Redis:          redisPinger,
		Workers:        workerCounter(liveWorkers),
		Logger:         logger,
		Version:        cfg.Version,
	})
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           api.Routes(),
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

// costGate builds the gate from the approved values, or returns nil when none
// are configured. A nil gate refuses nothing: the amounts are a product
// decision, and inventing one would look like it had been made.
func costGate(cfg config.Config, ledger *postgres.Spend, meters *metrics.Metrics, logger *slog.Logger) (*spend.Gate, error) {
	if !cfg.CostGateConfigured() {
		logger.Warn("no cost gate is configured: provider spending is measured but not limited",
			"set", "COST_GATE_WARN_USD, COST_GATE_STOP_USD, COST_GATE_STOP_ORDER, COST_GATE_PRICES")
		return nil, nil
	}
	limits, err := spend.NewLimits(cfg.CostGateWarnUSD, cfg.CostGateStopUSD, cfg.CostGateStopOrder)
	if err != nil {
		return nil, fmt.Errorf("the cost gate: %w", err)
	}
	ladder := make([]string, 0, len(limits.StopOrder))
	for position, group := range limits.StopOrder {
		ladder = append(ladder, fmt.Sprintf("%s at $%.2f", group, limits.ShedAt(position).USD()))
	}
	logger.Info("cost gate enabled",
		"warn_usd", limits.WarnMicros.USD(),
		"stop_usd", limits.StopMicros.USD(),
		"ladder", strings.Join(ladder, ", "))
	return spend.NewGate(limits, ledger, meters), nil
}

// optionalGate prevents a nil *spend.Gate from becoming a non-nil interface.
// Calling a method through that typed-nil interface panics on the first
// submission when the development cost gate is intentionally disabled.
func optionalGate(gate *spend.Gate) enqueue.Gate {
	if gate == nil {
		return nil
	}
	return gate
}
