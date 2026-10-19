// Command worker consumes the processing queues. One process runs one media,
// one light, one notification and one scheduled consumer, the outbox
// dispatcher and the lease sweeper; scaling is more processes, not more flags.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/XploY04/reelpin-go/internal/ai"
	"github.com/XploY04/reelpin-go/internal/config"
	"github.com/XploY04/reelpin-go/internal/db"
	"github.com/XploY04/reelpin-go/internal/embed"
	"github.com/XploY04/reelpin-go/internal/lease"
	"github.com/XploY04/reelpin-go/internal/metrics"
	"github.com/XploY04/reelpin-go/internal/notify"
	"github.com/XploY04/reelpin-go/internal/outbox"
	"github.com/XploY04/reelpin-go/internal/pipeline"
	"github.com/XploY04/reelpin-go/internal/postgres"
	"github.com/XploY04/reelpin-go/internal/pricetracks"
	"github.com/XploY04/reelpin-go/internal/queue"
	"github.com/XploY04/reelpin-go/internal/reminders"
	"github.com/XploY04/reelpin-go/internal/safehttp"
	"github.com/XploY04/reelpin-go/internal/spend"
	"github.com/XploY04/reelpin-go/internal/workerhealth"
	"github.com/jackc/pgx/v5/pgxpool"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	// This binary takes no arguments. Being handed some means something meant
	// to run a different command and silently consuming instead would hide it.
	if len(os.Args) > 1 {
		logger.Error("this command takes no arguments", "args", os.Args[1:])
		os.Exit(2)
	}

	if err := run(logger); err != nil {
		logger.Error("worker failed", "error", err)
		os.Exit(1)
	}
}

// handlers maps event types to their processing. An unknown type is poison by
// design — it dead-letters and waits for code that understands it, rather than
// being acknowledged into nothing.
func handlers(processor *pipeline.Pipeline, notifications, indexing queue.Handler, logger *slog.Logger) map[string]queue.Handler {
	return map[string]queue.Handler{
		queue.EventNotification: notifications,
		queue.EventProcessMedia: processor.Handle,
		queue.EventProcessLight: processor.Handle,
		"run.resume":            processor.Handle,
		"content.index":         indexing,
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.RabbitMQURL == "" {
		return errors.New("RABBITMQ_URL is required: a worker without a broker consumes nothing")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("db connect: %w", err)
	}
	defer pool.Close()

	connection, err := amqp.Dial(cfg.RabbitMQURL)
	if err != nil {
		return fmt.Errorf("rabbitmq connect: %w", err)
	}
	defer connection.Close()

	meters := metrics.New()

	publisher, err := queue.NewPublisher(connection, 5*time.Second)
	if err != nil {
		return err
	}
	publisher.Metrics = meters
	defer publisher.Close()

	// Heartbeats are how readiness sees the fleet. Redis being optional in
	// development means a worker without it simply is not counted.
	var (
		liveWorkers      metrics.WorkerCount
		reminderSchedule reminders.Schedule
		priceClaims      pricetracks.Claims
	)
	if cfg.RedisURL != "" {
		options, err := cfg.RedisOptions()
		if err != nil {
			return err
		}
		redisClient := redis.NewClient(options)
		defer redisClient.Close()
		if err := redisClient.Ping(ctx).Err(); err != nil {
			return fmt.Errorf("redis connect: %w", err)
		}
		go workerhealth.New(redisClient, cfg.RedisKeyPrefix, cfg.WorkerID, queue.WorkQueues).Run(ctx)
		liveWorkers = func(ctx context.Context) (int, error) {
			return workerhealth.Live(ctx, redisClient, cfg.RedisKeyPrefix)
		}
		reminderSchedule = reminders.NewRedisSchedule(redisClient, cfg.RedisKeyPrefix)
		priceClaims = pricetracks.NewRedisClaims(redisClient)
	} else {
		logger.Warn("no REDIS_URL: this worker sends no heartbeats and readiness cannot see it")
	}

	// Runs live under one directory this worker owns, so the disk gauge
	// measures this worker's leftovers rather than everything else in /tmp.
	tempRoot := filepath.Join(os.TempDir(), "reelpin-worker")
	if err := os.MkdirAll(tempRoot, 0o700); err != nil {
		return fmt.Errorf("worker temp directory: %w", err)
	}

	go metrics.Sample(ctx, meters, pool, liveWorkers)
	go metrics.SampleTempDisk(ctx, meters, tempRoot)

	// The worker is where the money is spent, so it is what writes the ledger.
	// Without configured prices the calls are still counted and stored; they
	// are stored at zero and surfaced as unpriced rather than guessed at.
	prices, err := spend.ParsePricesOrNone(cfg.CostGatePrices)
	if err != nil {
		return fmt.Errorf("COST_GATE_PRICES: %w", err)
	}
	if len(prices) == 0 {
		logger.Warn("no COST_GATE_PRICES: provider calls are counted but valued at zero")
	}
	ledger := spend.NewLedger(postgres.NewSpend(pool), prices, meters, logger)

	platforms, err := newRegistry(cfg, ledger, logger)
	if err != nil {
		return err
	}

	gemini := ai.NewGemini(ai.GeminiConfig{APIKey: cfg.GeminiAPIKey, Usage: ledger})
	processor := pipeline.New(pipeline.Deps{
		Pool:         pool,
		Handlers:     platforms,
		Transcriber:  gemini,
		ImageReader:  gemini,
		Extractor:    gemini,
		Categorizer:  gemini,
		ModelVersion: gemini.ModelVersion(),
		Logger:       logger,
		WorkerID:     cfg.WorkerID,
		TempRoot:     tempRoot,
		Metrics:      meters,
	})

	// Firebase is optional in development: without it a run still completes and
	// the notification is recorded as having nowhere to go.
	var sender notify.Sender = notify.NewFCM(cfg.FirebaseCredentialsJSON, cfg.FirebaseProjectID, 0)
	notifications := notify.NewService(pool, sender, logger, time.Now)
	notifications.Metrics = meters

	// The index is asynchronous on purpose: a search-index failure must never
	// turn a completed save into a failed job.
	embedder := embed.NewGemini(embed.GeminiConfig{
		APIKey:    cfg.GeminiAPIKey,
		Model:     cfg.EmbeddingModel,
		Dimension: cfg.EmbeddingDimension,
		Usage:     ledger,
	})
	if err := embed.AssertConfigured(cfg.EmbeddingModel, cfg.EmbeddingDimension); err != nil {
		return err
	}
	indexer := embed.NewIndexer(pool, embedder, logger)

	registry := handlers(processor,
		notificationHandler(pool, notifications, logger),
		indexHandler(pool, indexer, logger),
		logger)
	handle := func(ctx context.Context, message queue.Message) (queue.Outcome, error) {
		handler, ok := registry[message.EventType]
		if !ok {
			return queue.Outcome{Kind: queue.DeadLetter},
				fmt.Errorf("no handler for event type %q", message.EventType)
		}
		return handler(ctx, message)
	}

	// The daily price check is scheduled behind a Redis claim, so without
	// Redis no worker schedules it.
	if priceClaims != nil {
		checker := pricetracks.NewChecker(postgres.NewPriceTracks(pool),
			pricetracks.PageFetcher{Client: safehttp.New(safehttp.Config{})},
			notifications, logger, time.Now, pricetracks.CheckConcurrency)
		daily := pricetracks.NewDaily(checker, priceClaims, publisher, cfg.RedisKeyPrefix, logger, time.Now)
		registry[queue.EventPriceCheck] = daily.Handle
		if err := daily.Start(ctx); err != nil {
			return err
		}
	}

	done := make(chan error, len(queue.WorkQueues)+4)
	started := 0

	started++
	go func() { done <- serveMetrics(ctx, meters, cfg.MetricsPort, cfg.AdminKey, logger) }()

	started++
	go func() {
		done <- outbox.NewDispatcher(pool, publisher, logger, 100).Run(ctx, time.Second)
	}()

	started++
	go func() { done <- sweepLoop(ctx, logger, pool) }()

	// Reminders are fired from Redis's due set, so without Redis there is
	// nothing to read and the dispatcher does not run.
	if reminderSchedule != nil {
		dispatcher := reminders.NewDispatcher(postgres.NewReminders(pool), reminderSchedule, notifications, logger, time.Now)
		started++
		go func() { done <- dispatcher.Run(ctx, reminders.TickInterval) }()
	}

	for _, name := range queue.WorkQueues {
		started++
		go func(name string) {
			done <- queue.Consume(ctx, connection, queue.ConsumerConfig{
				Queue:       name,
				Prefetch:    1,
				Logger:      logger,
				ConsumerTag: cfg.WorkerID + ":" + name,
				Metrics:     meters,
			}, publisher, handle)
		}(name)
	}

	logger.Info("worker started", "worker_id", cfg.WorkerID, "queues", len(queue.WorkQueues))

	// A component exiting is fatal: the process must not keep looking alive
	// while nothing consumes. Whichever comes first starts the drain.
	var firstErr error
	select {
	case <-ctx.Done():
		logger.Info("worker draining", "budget", queue.DrainTimeout.String())
	case err := <-done:
		started--
		firstErr = err
		if firstErr == nil {
			firstErr = errors.New("a worker component exited before shutdown")
		}
		logger.Error("a worker component exited, shutting down", "error", firstErr)
		stop()
	}

	drain := time.NewTimer(queue.DrainTimeout)
	defer drain.Stop()
	for finished := 0; finished < started; {
		select {
		case err := <-done:
			finished++
			if err != nil && firstErr == nil {
				firstErr = err
			}
		case <-drain.C:
			logger.Warn("drain budget expired with work still running")
			return firstErr
		}
	}
	return firstErr
}

// sweepLoop returns expired-lease runs to their queues. It is deliberately
// slow: the sweep exists for crashed workers, not as a scheduler, and every
// resume it writes is idempotent per lease generation.
func sweepLoop(ctx context.Context, logger *slog.Logger, pool *pgxpool.Pool) error {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		swept, err := lease.SweepExpired(ctx, pool, queue.QueueLight, 100)
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("lease sweep failed", "error", err)
			continue
		}
		if swept > 0 {
			logger.Info("requeued runs from workers that went away", "count", swept)
		}
	}
}
