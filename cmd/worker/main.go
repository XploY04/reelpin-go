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
	"github.com/XploY04/reelpin-go/internal/embed"
	"github.com/XploY04/reelpin-go/internal/geo"
	"github.com/XploY04/reelpin-go/internal/lease"
	"github.com/XploY04/reelpin-go/internal/media"
	"github.com/XploY04/reelpin-go/internal/notify"
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

	// Housekeeping the queue cannot do for itself: runs whose worker vanished
	// come back, and published events stop accumulating.
	started++
	go func() { done <- runMaintenance(ctx, pool, logger) }()

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
	notifier := notify.NewService(pool,
		notify.NewFCM(cfg.FirebaseCredentialsJSON, cfg.FirebaseProjectID, 0), logger, time.Now)
	indexer := embed.NewIndexer(pool, embed.NewGemini(cfg.GeminiAPIKey, 0), logger)
	handler := dispatch(pool, logger, workerID, processor, collectionService, notifier, indexer)
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

// maintenanceInterval is how often the housekeeping sweep runs. It is slow on
// purpose: none of this is urgent, and all of it touches shared tables.
const maintenanceInterval = 5 * time.Minute

// runMaintenance reclaims abandoned runs and trims published events. Retention
// of user-facing data stays a deliberate command, not a background job.
func runMaintenance(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) error {
	ticker := time.NewTicker(maintenanceInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			reclaimed, err := lease.ReclaimExpired(ctx, pool, 200)
			if err != nil {
				logger.Warn("reclaiming expired leases failed", "error", err)
			} else if reclaimed > 0 {
				logger.Info("reclaimed runs from workers that went away", "count", reclaimed)
			}

			if _, err := pool.Exec(ctx, `
				DELETE FROM reelpin.outbox_events
				WHERE published_at IS NOT NULL AND published_at < now() - interval '14 days'`,
			); err != nil {
				logger.Warn("trimming published outbox events failed", "error", err)
			}
		}
	}
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
	notifier *notify.Service,
	indexer *embed.Indexer,
) queue.Handler {
	processRun := leasedHandler(pool, logger, workerID, processor)

	return func(ctx context.Context, message queue.Message) (queue.Outcome, error) {
		switch message.Type {
		case "reel.saved":
			return fileAndNotify(ctx, pool, logger, collectionService, notifier, message)
		case "collection.items_added":
			return notifyCollection(ctx, pool, logger, notifier, message)
		case "content.index":
			return indexContent(ctx, pool, logger, indexer, message)
		default:
			return processRun(ctx, message)
		}
	}
}

// fileAndNotify finishes a save: file it into the collections the share named,
// then tell the user it is ready. Filing runs first, because a notification
// that arrives before the reel is filed would send them to a half-finished
// state.
func fileAndNotify(
	ctx context.Context,
	pool *pgxpool.Pool,
	logger *slog.Logger,
	collectionService *collections.Service,
	notifier *notify.Service,
	message queue.Message,
) (queue.Outcome, error) {
	var payload struct {
		UserID        string   `json:"user_id"`
		ReelID        string   `json:"reel_id"`
		JobID         string   `json:"job_id"`
		CollectionIDs []string `json:"collection_ids"`
	}
	if err := readEventPayload(ctx, pool, message.EventID, &payload); err != nil {
		logger.Error("reading a saved-reel event failed", "event_id", message.EventID, "error", err)
		return queue.Retry, err
	}
	if payload.ReelID == "" {
		return queue.Done, nil
	}

	if len(payload.CollectionIDs) > 0 {
		filed, err := collectionService.FileReel(ctx, payload.UserID, payload.ReelID, payload.CollectionIDs)
		if err != nil {
			return queue.Retry, err
		}
		logger.Info("filed a saved reel", "reel_id", payload.ReelID, "collections", len(filed))
	}

	title, platformName, contentType := reelDisplay(ctx, pool, payload.ReelID)
	_, err := notifier.SendToUser(ctx, notify.ReelReady(
		payload.UserID, payload.ReelID, payload.JobID, title, platformName, contentType))
	switch {
	case err == nil:
		return queue.Done, nil
	case errors.Is(err, notify.ErrNoDeviceTokens):
		// The app usually registers its token seconds after the first share,
		// so this comes back rather than being lost.
		logger.Info("no device to notify yet, retrying", "reel_id", payload.ReelID)
		return queue.Retry, err
	default:
		return queue.Retry, err
	}
}

// notifyCollection tells the other members that something was added.
func notifyCollection(
	ctx context.Context,
	pool *pgxpool.Pool,
	logger *slog.Logger,
	notifier *notify.Service,
	message queue.Message,
) (queue.Outcome, error) {
	var payload struct {
		CollectionID string `json:"collection_id"`
		ActorUserID  string `json:"actor_user_id"`
		Added        int    `json:"added"`
	}
	if err := readEventPayload(ctx, pool, message.EventID, &payload); err != nil {
		logger.Error("reading a collection event failed", "event_id", message.EventID, "error", err)
		return queue.Retry, err
	}
	if payload.CollectionID == "" {
		return queue.Done, nil
	}

	rows, err := pool.Query(ctx, `
		SELECT c.owner_id FROM public.collections c WHERE c.id = $1 AND c.owner_id <> $2
		UNION
		SELECT m.user_id FROM public.collection_members m
		WHERE m.collection_id = $1 AND m.user_id <> $2`,
		payload.CollectionID, payload.ActorUserID)
	if err != nil {
		return queue.Retry, err
	}
	defer rows.Close()

	recipients := []string{}
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			return queue.Retry, err
		}
		recipients = append(recipients, userID)
	}
	if err := rows.Err(); err != nil {
		return queue.Retry, err
	}

	for _, userID := range recipients {
		notification := notify.CollectionUpdated(userID, payload.CollectionID, "", payload.Added)
		// One notification per member per event, so a redelivery is silent.
		notification.EventKey = "collection_updated:" + userID + ":" + message.EventID
		if _, err := notifier.SendToUser(ctx, notification); err != nil &&
			!errors.Is(err, notify.ErrNoDeviceTokens) {
			return queue.Retry, err
		}
	}
	return queue.Done, nil
}

// indexContent embeds a finished content version so it can be searched.
// Indexing is deliberately separate from saving: a reel is worth keeping even
// if the search index is behind, and retrying this must never repeat a
// download.
func indexContent(
	ctx context.Context,
	pool *pgxpool.Pool,
	logger *slog.Logger,
	indexer *embed.Indexer,
	message queue.Message,
) (queue.Outcome, error) {
	var payload struct {
		ContentVersionID string `json:"content_version_id"`
	}
	if err := readEventPayload(ctx, pool, message.EventID, &payload); err != nil {
		logger.Error("reading an index event failed", "event_id", message.EventID, "error", err)
		return queue.Retry, err
	}
	if payload.ContentVersionID == "" {
		return queue.Done, nil
	}

	indexed, chunks, err := indexer.IndexVersion(ctx, payload.ContentVersionID)
	if err != nil {
		return queue.Retry, err
	}
	logger.Info("indexed content for search",
		"content_version_id", payload.ContentVersionID, "reindexed", indexed, "chunks", chunks)
	return queue.Done, nil
}

// reelDisplay reads just enough to write a notification a person understands.
func reelDisplay(ctx context.Context, pool *pgxpool.Pool, reelID string) (string, string, string) {
	var title string
	var platformName, contentType *string
	if err := pool.QueryRow(ctx,
		`SELECT title, source_platform, source_content_type FROM public.reels WHERE id = $1`, reelID,
	).Scan(&title, &platformName, &contentType); err != nil {
		return "", "", ""
	}
	return title, text(platformName), text(contentType)
}

func text(value *string) string {
	if value == nil {
		return ""
	}
	return *value
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
