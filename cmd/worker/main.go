// Command worker consumes the job queues and dispatches the outbox. It shares
// the API's configuration and database, and nothing else: an API replica never
// runs work, and a worker never serves traffic.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/XploY04/reelpin-go/internal/ai"
	"github.com/XploY04/reelpin-go/internal/apify"
	"github.com/XploY04/reelpin-go/internal/collections"
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
	"github.com/XploY04/reelpin-go/internal/platform/social"
	"github.com/XploY04/reelpin-go/internal/platform/web"
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

	redditClient := social.NewRedditClient(
		cfg.RedditClientID, cfg.RedditClientSecret, cfg.RedditUserAgent, safeClient)
	socialDeps := social.Deps{
		HTTP:    safeClient,
		Apify:   actors,
		Storage: thumbnails,
		Reddit:  redditClient,
		Logger:  logger,
	}

	webHandler := web.New(web.Deps{
		HTTP:       safeClient,
		Downloader: media.NewYTDLP(runner),
		Audio:      media.NewFFmpeg(runner),
		Apify:      actors,
		Cookies:    cookieJar,
		Storage:    thumbnails,
		Logger:     logger,
	})

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
		// Registration order is priority order: the dedicated handlers first,
		// then the web handler as the fallback for every other link.
		Handlers: platform.NewRegistry(
			instagramHandler,
			social.NewX(socialDeps),
			social.NewLinkedIn(socialDeps),
			social.NewReddit(socialDeps),
			webHandler,
		),
		Transcriber: gemini,
		ImageReader: gemini,
		Extractor:   gemini,
		Categorizer: gemini,
		Geocoder:    geo.NewCached(pool, geo.NewGoogle(cfg.GoogleMapsAPIKey, 0)),
		Logger:      logger,
		TempRoot:    cfg.WorkerTempRoot,
	})

	collectionService := collections.New(pool, cfg.CollectionShareBaseURL, time.Now)
	handler := dispatch(pool, logger, workerID, processor, collectionService)
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

// dispatch routes a message by what it describes. Processing a run, filing a
// finished reel and indexing it are separate work with separate failure modes,
// and only the first one takes a run lease.
func dispatch(
	pool *pgxpool.Pool,
	logger *slog.Logger,
	workerID string,
	processor *pipeline.Pipeline,
	collectionService *collections.Service,
) queue.Handler {
	processRun := leasedHandler(pool, logger, workerID, processor)

	return func(ctx context.Context, message queue.Message) (queue.Outcome, error) {
		switch message.Type {
		case "reel.saved":
			return fileSavedReel(ctx, pool, logger, collectionService, message)
		case "content.index", "content.notify", "collection.items_added":
			// Task 16 and Task 18 give these real consumers. Until then they
			// are acknowledged rather than retried forever.
			logger.Info("event acknowledged without a consumer yet",
				"type", message.Type, "event_id", message.EventID)
			return queue.Done, nil
		default:
			return processRun(ctx, message)
		}
	}
}

// fileSavedReel adds a finished reel to the collections its share named. It
// runs after the save and can never undo one: a target that disappeared, or
// that the user may no longer edit, is skipped.
func fileSavedReel(
	ctx context.Context,
	pool *pgxpool.Pool,
	logger *slog.Logger,
	collectionService *collections.Service,
	message queue.Message,
) (queue.Outcome, error) {
	var payload struct {
		UserID        string   `json:"user_id"`
		ReelID        string   `json:"reel_id"`
		CollectionIDs []string `json:"collection_ids"`
	}
	if err := readEventPayload(ctx, pool, message.EventID, &payload); err != nil {
		logger.Error("reading a saved-reel event failed", "event_id", message.EventID, "error", err)
		return queue.Retry, err
	}
	if payload.ReelID == "" || len(payload.CollectionIDs) == 0 {
		return queue.Done, nil
	}

	filed, err := collectionService.FileReel(ctx, payload.UserID, payload.ReelID, payload.CollectionIDs)
	if err != nil {
		return queue.Retry, err
	}
	logger.Info("filed a saved reel", "reel_id", payload.ReelID, "collections", len(filed))
	return queue.Done, nil
}

// readEventPayload loads what the publisher wrote. The message on the broker
// carries identifiers only, so the payload is read back from the outbox row.
func readEventPayload(ctx context.Context, pool *pgxpool.Pool, eventID string, target any) error {
	var payload []byte
	if err := pool.QueryRow(ctx,
		`SELECT payload FROM reelpin.outbox_events WHERE event_id = $1`, eventID,
	).Scan(&payload); err != nil {
		return fmt.Errorf("reading the event payload: %w", err)
	}
	return json.Unmarshal(payload, target)
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
