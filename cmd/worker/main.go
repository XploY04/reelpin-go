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

	"github.com/XploY04/reelpin-go/internal/config"
	"github.com/XploY04/reelpin-go/internal/db"
	"github.com/XploY04/reelpin-go/internal/lease"
	"github.com/XploY04/reelpin-go/internal/outbox"
	"github.com/XploY04/reelpin-go/internal/queue"
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

	handler := stubHandler(pool, logger, workerID)
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

// stubHandler proves the delivery, lease and shutdown paths end to end. The
// real pipeline replaces it in Task 10, and this file is the only thing that
// changes when it does.
func stubHandler(pool *pgxpool.Pool, logger *slog.Logger, workerID string) queue.Handler {
	return func(ctx context.Context, message queue.Message) (queue.Outcome, error) {
		held, err := lease.Acquire(ctx, pool, message.RunID, workerID)
		if errors.Is(err, lease.ErrNotAcquired) {
			// Another worker owns this run, or it already finished. A duplicate
			// delivery ends here.
			logger.Info("run already taken", "run_id", message.RunID)
			return queue.Done, nil
		}
		if err != nil {
			return queue.Retry, err
		}

		work, cancel := lease.KeepAlive(ctx, pool, held.RunID, workerID)
		defer cancel()

		if _, err := pool.Exec(work, `
			UPDATE reelpin.processing_runs
			SET status = 'completed', stage = 'complete', progress_percent = 100,
			    lease_owner = NULL, lease_expires_at = NULL,
			    completed_at = now(), updated_at = now()
			WHERE id = $1 AND lease_owner = $2`,
			held.RunID, workerID,
		); err != nil {
			return queue.Retry, err
		}
		return queue.Done, nil
	}
}
