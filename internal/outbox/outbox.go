// Package outbox turns rows written inside a business transaction into
// published messages. Nothing is enqueued directly: a row and its event commit
// together, so work is never lost and never published for a transaction that
// rolled back.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/XploY04/reelpin-go/internal/queue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// maxAttempts is how often one event is retried before it is parked. Parking
// keeps a poisoned row from blocking the queue behind it.
const maxAttempts = 12

type Publisher interface {
	Publish(ctx context.Context, queue string, message queue.Message) error
}

// Event is what a business transaction writes.
type Event struct {
	EventID     string
	EventType   string
	RoutingKey  string
	Payload     any
	AvailableAt time.Time
	// Environment scopes the row. Dev and production share one database, so an
	// event without it can be claimed and published by the other deployment.
	Environment string
}

// Insert writes an event inside the caller's transaction. It must never be
// called outside one: that is the whole point of the pattern.
func Insert(ctx context.Context, tx pgx.Tx, event Event) error {
	payload, err := json.Marshal(event.Payload)
	if err != nil {
		return fmt.Errorf("encoding the outbox payload: %w", err)
	}
	// The database's clock decides when a row is due. Taking "now" from this
	// process would make a row undispatchable whenever the app host runs even
	// slightly ahead of the database.
	var availableAt any
	if !event.AvailableAt.IsZero() {
		availableAt = event.AvailableAt
	}

	environment := event.Environment
	if environment == "" {
		return fmt.Errorf("the outbox event has no environment: it could be claimed by another deployment")
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO reelpin.outbox_events
			(event_id, event_type, routing_key, schema_version, payload, available_at, environment)
		VALUES ($1, $2, $3, $4, $5, COALESCE($6::timestamptz, now()), $7)
		ON CONFLICT (event_id) DO NOTHING`,
		event.EventID, event.EventType, event.RoutingKey, queue.SchemaVersion, payload, availableAt, environment,
	)
	if err != nil {
		return fmt.Errorf("writing the outbox event: %w", err)
	}
	return nil
}

type Dispatcher struct {
	pool      *pgxpool.Pool
	publisher Publisher
	logger    *slog.Logger
	batchSize int
	// environment is the only rows this dispatcher will ever claim.
	environment string
}

func NewDispatcher(pool *pgxpool.Pool, publisher Publisher, logger *slog.Logger, batchSize int, environment string) *Dispatcher {
	if batchSize <= 0 {
		batchSize = 100
	}
	if environment == "" {
		environment = DefaultEnvironment
	}
	return &Dispatcher{
		pool:        pool,
		publisher:   publisher,
		logger:      logger,
		batchSize:   batchSize,
		environment: environment,
	}
}

// Run dispatches until the context is cancelled.
func (d *Dispatcher) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			published, err := d.DispatchOnce(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				d.logger.Error("outbox dispatch failed", "error", err)
			}
			if published == d.batchSize {
				// A full batch means there is probably more waiting.
				ticker.Reset(time.Millisecond)
			} else {
				ticker.Reset(interval)
			}
		}
	}
}

// DispatchOnce claims a batch, publishes it, and marks what the broker
// confirmed. A crash between publish and mark republishes the message, which is
// why consumers must be idempotent.
func (d *Dispatcher) DispatchOnce(ctx context.Context) (int, error) {
	transaction, err := d.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("starting the dispatch transaction: %w", err)
	}
	defer transaction.Rollback(ctx)

	// SKIP LOCKED is what lets several dispatchers run without coordinating.
	rows, err := transaction.Query(ctx, `
		SELECT event_id, event_type, routing_key, payload, attempts
		FROM reelpin.outbox_events
		WHERE environment = $1
		  AND published_at IS NULL AND available_at <= now() AND attempts < $2
		ORDER BY available_at, event_id
		LIMIT $3
		FOR UPDATE SKIP LOCKED`,
		d.environment, maxAttempts, d.batchSize,
	)
	if err != nil {
		return 0, fmt.Errorf("claiming outbox rows: %w", err)
	}

	type claim struct {
		eventID    string
		eventType  string
		routingKey string
		payload    []byte
		attempts   int
	}
	var claimed []claim
	for rows.Next() {
		var row claim
		if err := rows.Scan(&row.eventID, &row.eventType, &row.routingKey, &row.payload, &row.attempts); err != nil {
			rows.Close()
			return 0, fmt.Errorf("reading outbox rows: %w", err)
		}
		claimed = append(claimed, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("reading outbox rows: %w", err)
	}

	published := 0
	for _, row := range claimed {
		message, err := messageFor(row.eventID, row.eventType, row.payload, row.attempts)
		if err == nil {
			err = d.publisher.Publish(ctx, row.routingKey, message)
		}
		if err != nil {
			// The row stays unpublished with a recorded attempt and a backoff.
			if _, markErr := transaction.Exec(ctx, `
				UPDATE reelpin.outbox_events
				SET attempts = attempts + 1,
				    available_at = now() + make_interval(secs => least(300, power(2, attempts)::int)),
				    last_error = $2,
				    updated_at = now()
				WHERE event_id = $1`,
				row.eventID, truncate(err.Error()),
			); markErr != nil {
				return published, fmt.Errorf("recording a failed publish: %w", markErr)
			}
			d.logger.Warn("publishing an outbox event failed",
				"event_id", row.eventID, "attempts", row.attempts+1, "error", err)
			continue
		}

		if _, err := transaction.Exec(ctx,
			`UPDATE reelpin.outbox_events SET published_at = now(), updated_at = now() WHERE event_id = $1`,
			row.eventID,
		); err != nil {
			return published, fmt.Errorf("marking an outbox event published: %w", err)
		}
		published++
	}

	if err := transaction.Commit(ctx); err != nil {
		return 0, fmt.Errorf("committing the dispatch: %w", err)
	}
	return published, nil
}

func messageFor(eventID, eventType string, payload []byte, attempts int) (queue.Message, error) {
	var body struct {
		RunID    string `json:"run_id"`
		Platform string `json:"platform"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return queue.Message{}, fmt.Errorf("decoding the outbox payload: %w", err)
	}

	message := queue.Message{
		EventID:       eventID,
		RunID:         body.RunID,
		Platform:      body.Platform,
		Attempt:       attempts,
		SchemaVersion: queue.SchemaVersion,
		Type:          eventType,
	}
	if err := message.Validate(); err != nil {
		return queue.Message{}, err
	}
	return message, nil
}

// truncate keeps a broker error from filling the row, and keeps whatever the
// broker echoed out of the database beyond a readable prefix.
func truncate(value string) string {
	const limit = 300
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
