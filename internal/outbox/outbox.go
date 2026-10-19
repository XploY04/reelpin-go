// Package outbox publishes what a business transaction wrote. An event row is
// inserted in the same transaction as the state it describes, so the two
// commit or vanish together; a dispatcher then moves committed rows to the
// broker. A crash between publish and mark republishes the message, which is
// why every consumer is idempotent by event id.
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

// maxAttempts bounds how often one event is retried before a person looks at
// it. The row stays in the table either way; nothing is deleted by failure.
const maxAttempts = 25

// Event is one committed fact the broker should carry. The payload is
// identifiers only; a worker loads state from PostgreSQL by run id.
type Event struct {
	EventID    string
	EventType  string
	RoutingKey string
	RunID      string
	// DispatchGeneration is the run's lease generation at insert time. A
	// worker whose claim yields an older generation is fenced.
	DispatchGeneration int64
	AvailableAt        time.Time
}

// Insert writes an event inside the caller's transaction. It must never be
// called outside one: that is the whole point of the pattern. The event id is
// the idempotency key; reinserting the same id changes nothing.
func Insert(ctx context.Context, tx pgx.Tx, event Event) error {
	if event.EventID == "" || event.EventType == "" || event.RoutingKey == "" {
		return fmt.Errorf("outbox event is missing its identifiers")
	}

	payload, err := json.Marshal(map[string]any{
		"run_id":              event.RunID,
		"dispatch_generation": event.DispatchGeneration,
	})
	if err != nil {
		return fmt.Errorf("encoding the outbox payload: %w", err)
	}

	// The database clock decides when a row is due: an app host running ahead
	// of the database would otherwise write rows that are never due.
	var availableAt any
	if !event.AvailableAt.IsZero() {
		availableAt = event.AvailableAt
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO reelpin.outbox_events
			(event_id, event_type, routing_key, schema_version, payload, available_at)
		VALUES ($1, $2, $3, $4, $5, COALESCE($6::timestamptz, now()))
		ON CONFLICT (event_id) DO NOTHING`,
		event.EventID, event.EventType, event.RoutingKey, queue.SchemaVersion, payload, availableAt,
	)
	if err != nil {
		return fmt.Errorf("writing the outbox event: %w", err)
	}
	return nil
}

// Publisher is what the dispatcher needs from the broker side.
type Publisher interface {
	Publish(ctx context.Context, routingKey string, message queue.Message) error
}

type Dispatcher struct {
	pool      *pgxpool.Pool
	publisher Publisher
	logger    *slog.Logger
	batchSize int
	// signal is nil without Redis, and then Run polls PostgreSQL as it always
	// has.
	signal *Signal
	now    func() time.Time
	// dispatch is DispatchOnce plus how long until the next unpublished row
	// is due; tests replace it to count database round trips.
	dispatch func(ctx context.Context) (int, *time.Duration, error)
}

func NewDispatcher(pool *pgxpool.Pool, publisher Publisher, logger *slog.Logger, batchSize int) *Dispatcher {
	if batchSize <= 0 {
		batchSize = 100
	}
	d := &Dispatcher{pool: pool, publisher: publisher, logger: logger, batchSize: batchSize, now: time.Now}
	d.dispatch = d.dispatchOnce
	return d
}

// WithSignal makes Run wait for writers instead of polling. A nil signal
// leaves the polling in place.
func (d *Dispatcher) WithSignal(signal *Signal) *Dispatcher {
	d.signal = signal
	return d
}

// Run dispatches until the context is cancelled. With a signal it asks Redis
// every interval and touches PostgreSQL only when a writer marked the outbox
// dirty or a row it already knows about falls due; without one it polls.
func (d *Dispatcher) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Whatever was committed while no dispatcher was running goes out now.
	var nextDue time.Time
	run := true
	for {
		if run {
			nextDue = d.drain(ctx)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		run = d.due(ctx, nextDue)
	}
}

// drain dispatches batches until one comes back short, and returns when the
// earliest row still waiting falls due. A zero time means nothing is waiting.
func (d *Dispatcher) drain(ctx context.Context) time.Time {
	for {
		published, next, err := d.dispatch(ctx)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				d.logger.Error("outbox dispatch failed", "error", err)
			}
			// A failed dispatch learned nothing, so the next tick tries again.
			return d.now()
		}
		if published == d.batchSize {
			// A full batch means there is probably more; go again now.
			continue
		}
		if next == nil {
			return time.Time{}
		}
		return d.now().Add(*next)
	}
}

// due decides whether this tick touches PostgreSQL.
func (d *Dispatcher) due(ctx context.Context, nextDue time.Time) bool {
	if d.signal == nil {
		return true
	}
	dirty, err := d.signal.take(ctx)
	if err != nil {
		// Redis is the only thing standing between a write and its delivery,
		// so while it fails the dispatcher polls exactly as it did without it.
		if !errors.Is(err, context.Canceled) {
			d.logger.Warn("outbox signal unreadable, polling instead", "error", err)
		}
		return true
	}
	if dirty {
		return true
	}
	return !nextDue.IsZero() && !d.now().Before(nextDue)
}

// DispatchOnce claims one bounded batch, publishes it, and marks what the
// broker confirmed. Claims live inside the transaction, so a crashed
// dispatcher's claims release themselves; several dispatchers may run at once
// and duplicate publication stays safe.
func (d *Dispatcher) DispatchOnce(ctx context.Context) (int, error) {
	published, _, err := d.dispatchOnce(ctx)
	return published, err
}

// dispatchOnce also reports how long until the earliest row still waiting is
// due, measured on the database clock, or nil when nothing is waiting. Retries
// and publish backoff write a future available_at, and no writer signals when
// that moment arrives.
func (d *Dispatcher) dispatchOnce(ctx context.Context) (int, *time.Duration, error) {
	transaction, err := d.pool.Begin(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("starting the dispatch transaction: %w", err)
	}
	defer transaction.Rollback(ctx)

	rows, err := transaction.Query(ctx, `
		SELECT event_id, event_type, routing_key, payload, attempts, created_at
		FROM reelpin.outbox_events
		WHERE published_at IS NULL AND available_at <= now() AND attempts < $1
		ORDER BY available_at, event_id
		LIMIT $2
		FOR UPDATE SKIP LOCKED`,
		maxAttempts, d.batchSize,
	)
	if err != nil {
		return 0, nil, fmt.Errorf("claiming outbox rows: %w", err)
	}

	type claim struct {
		eventID    string
		eventType  string
		routingKey string
		payload    []byte
		attempts   int
		createdAt  time.Time
	}
	claims := []claim{}
	for rows.Next() {
		var c claim
		if err := rows.Scan(&c.eventID, &c.eventType, &c.routingKey, &c.payload, &c.attempts, &c.createdAt); err != nil {
			rows.Close()
			return 0, nil, fmt.Errorf("reading a claimed row: %w", err)
		}
		claims = append(claims, c)
	}
	rows.Close()
	if rows.Err() != nil {
		return 0, nil, fmt.Errorf("claiming outbox rows: %w", rows.Err())
	}

	published := 0
	for _, c := range claims {
		var payload struct {
			RunID              string `json:"run_id"`
			DispatchGeneration int64  `json:"dispatch_generation"`
		}
		if err := json.Unmarshal(c.payload, &payload); err != nil {
			// A row this service wrote and cannot read is a bug, not a retry.
			// Push it past the attempt cap so a person looks at it.
			d.logger.Error("outbox payload does not decode", "event_id", c.eventID, "error", err)
			if _, err := transaction.Exec(ctx,
				`UPDATE reelpin.outbox_events SET attempts = $2 WHERE event_id = $1`,
				c.eventID, maxAttempts); err != nil {
				return published, nil, fmt.Errorf("parking an undecodable event: %w", err)
			}
			continue
		}

		err := d.publisher.Publish(ctx, c.routingKey, queue.Message{
			EventID:            c.eventID,
			SchemaVersion:      queue.SchemaVersion,
			EventType:          c.eventType,
			RunID:              payload.RunID,
			DispatchGeneration: payload.DispatchGeneration,
			CreatedAt:          c.createdAt.UTC(),
		})
		if err != nil {
			// Record the attempt with backoff and move on: one broken event
			// must not hold the batch, and an unknown confirm after
			// connection loss stays unpublished, which is safe to resend.
			backoff := time.Duration(min(c.attempts+1, 10)) * 30 * time.Second
			d.logger.Warn("publish failed, backing off",
				"event_id", c.eventID, "attempt", c.attempts+1, "backoff", backoff.String(), "error", err)
			if _, err := transaction.Exec(ctx, `
				UPDATE reelpin.outbox_events
				SET attempts = attempts + 1, available_at = now() + $2::interval
				WHERE event_id = $1`,
				c.eventID, backoff.String()); err != nil {
				return published, nil, fmt.Errorf("recording a failed publish: %w", err)
			}
			continue
		}

		if _, err := transaction.Exec(ctx,
			`UPDATE reelpin.outbox_events SET published_at = now(), attempts = attempts + 1 WHERE event_id = $1`,
			c.eventID); err != nil {
			return published, nil, fmt.Errorf("marking a published event: %w", err)
		}
		published++
	}

	var seconds *float64
	if err := transaction.QueryRow(ctx, `
		SELECT extract(epoch FROM min(available_at) - now())::float8
		FROM reelpin.outbox_events
		WHERE published_at IS NULL AND attempts < $1`,
		maxAttempts,
	).Scan(&seconds); err != nil {
		return published, nil, fmt.Errorf("finding the next due event: %w", err)
	}

	if err := transaction.Commit(ctx); err != nil {
		return 0, nil, fmt.Errorf("committing the dispatch: %w", err)
	}
	if seconds == nil {
		return published, nil, nil
	}
	next := max(time.Duration(*seconds*float64(time.Second)), 0)
	return published, &next, nil
}
