// Command worker consumes the job queues and dispatches the outbox. It shares
// the API's configuration and database, and nothing else: an API replica never
// runs work, and a worker never serves traffic.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/XploY04/reelpin-go/internal/ai"
	"github.com/XploY04/reelpin-go/internal/apify"
	"github.com/XploY04/reelpin-go/internal/config"
	"github.com/XploY04/reelpin-go/internal/cookies"
	"github.com/XploY04/reelpin-go/internal/db"
	"github.com/XploY04/reelpin-go/internal/geo"
	"github.com/XploY04/reelpin-go/internal/lease"
	"github.com/XploY04/reelpin-go/internal/media"
	"github.com/XploY04/reelpin-go/internal/outbox"
	"github.com/XploY04/reelpin-go/internal/pipeline"
	"github.com/XploY04/reelpin-go/internal/platform"
	"github.com/XploY04/reelpin-go/internal/platform/instagram"
	"github.com/XploY04/reelpin-go/internal/queue"
	"github.com/XploY04/reelpin-go/internal/safehttp"
	"github.com/XploY04/reelpin-go/internal/storage"
	"github.com/XploY04/reelpin-go/internal/workerhealth"
	"github.com/jackc/pgx/v5/pgxpool"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if err := run(logger); err != nil {
		logger.Error("worker failed", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.RabbitMQURL == "" {
		return errors.New("RABBITMQ_URL is required to run the worker")
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

	publisher, err := queue.NewPublisher(connection, 5*time.Second)
	if err != nil {
		return err
	}
	defer publisher.Close()

	var redisClient *redis.Client
	if cfg.RedisURL != "" {
		options, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			return fmt.Errorf("redis url: %w", err)
		}
		redisClient = redis.NewClient(options)
		defer redisClient.Close()
	}

	workerID := cfg.WorkerID
	logger = logger.With("worker_id", workerID)

	// One bounded pool across every queue, so a burst on one platform cannot
	// use the whole process.
	global := make(chan struct{}, cfg.WorkerGlobalConcurrency)

	done := make(chan error, len(queue.WorkQueues)+1)
	started := 0

	go workerhealth.New(redisClient, pool, cfg.WorkerPrefix(), workerID, queue.WorkQueues).Run(ctx)

	dispatcher := outbox.NewDispatcher(pool, publisher, logger, cfg.OutboxBatchSize)
	started++
	go func() { done <- dispatcher.Run(ctx, time.Second) }()

	// A crashed worker cannot clean up after itself, so leftovers are swept
	// before this one starts making more.
	if swept, err := pipeline.SweepTempDirectories(cfg.WorkerTempRoot, time.Hour, time.Now()); err != nil {
		logger.Warn("sweeping old run directories failed", "error", err)
	} else if swept > 0 {
		logger.Info("swept abandoned run directories", "count", swept)
	}
	if err := os.MkdirAll(cfg.WorkerTempRoot, 0o700); err != nil {
		return fmt.Errorf("creating the worker temp root: %w", err)
	}

	gemini := ai.NewGemini(ai.GeminiConfig{APIKey: cfg.GeminiAPIKey})

	// One safe client, one command runner and one cookie jar are shared by
	// every platform handler.
	safeClient := safehttp.New(safehttp.Config{})
	runner := media.ExecRunner{}
	cookieJar := cookies.New(cfg.InstagramCookies)
	actors := apify.New(apify.Config{Token: cfg.ApifyToken, Actors: cfg.ApifyActors})
	thumbnails := storage.NewSupabase(cfg.SupabaseURL, cfg.StorageBucket, cfg.SupabaseServiceKey, 0)

	instagramHandler := instagram.New(instagram.Deps{
		HTTP:       safeClient,
		Downloader: media.NewYTDLP(runner),
		Audio:      media.NewFFmpeg(runner),
		Apify:      actors,
		Cookies:    cookieJar,
		Storage:    thumbnails,
		Logger:     logger,
	})
	processor := pipeline.New(pipeline.Deps{
		Pool: pool,
		// The remaining platforms arrive with their own tasks; until then a
		// share of an unhandled source fails as unsupported rather than
		// silently.
		Handlers:    platform.NewRegistry(instagramHandler),
		Transcriber: gemini,
		ImageReader: gemini,
		Extractor:   gemini,
		Categorizer: gemini,
		Geocoder:    geo.NewCached(pool, geo.NewGoogle(cfg.GoogleMapsAPIKey, 0)),
		Logger:      logger,
		TempRoot:    cfg.WorkerTempRoot,
	})

	handler := leasedHandler(pool, logger, workerID, processor)
	for _, name := range queue.WorkQueues {
		started++
		go func(name string) {
			done <- queue.Consume(ctx, connection, queue.ConsumerConfig{
				Queue:       name,
				Concurrency: cfg.WorkerQueueConcurrency,
				Global:      global,
				Logger:      logger,
				ConsumerTag: workerID + ":" + name,
			}, publisher, handler)
		}(name)
	}

	logger.Info("cookie slots configured", "slots", len(cookieJar.Slots()))
	logger.Info("worker started",
		"queues", len(queue.WorkQueues),
		"queue_concurrency", cfg.WorkerQueueConcurrency,
		"global_concurrency", cfg.WorkerGlobalConcurrency,
	)

	<-ctx.Done()
	logger.Info("worker draining", "budget", queue.DrainTimeout.String())

	drain := time.NewTimer(queue.DrainTimeout)
	defer drain.Stop()

	var firstErr error
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

// leasedHandler is the delivery path: take the run's lease, keep it alive while
// the pipeline works, and hand back what the consumer should do with the
// message. A duplicate delivery finds the run taken and stops here.
func leasedHandler(pool *pgxpool.Pool, logger *slog.Logger, workerID string, processor *pipeline.Pipeline) queue.Handler {
	return func(ctx context.Context, message queue.Message) (queue.Outcome, error) {
		held, err := lease.Acquire(ctx, pool, message.RunID, workerID)
		if errors.Is(err, lease.ErrNotAcquired) {
			logger.Info("run already taken or finished", "run_id", message.RunID)
			return queue.Done, nil
		}
		if err != nil {
			return queue.Retry, err
		}

		// Losing the lease cancels the work, so two workers never write the
		// same results.
		work, cancel := lease.KeepAlive(ctx, pool, held.RunID, workerID)
		defer cancel()

		return processor.Handle(work, message)
	}
}
