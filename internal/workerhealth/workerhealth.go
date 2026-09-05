// Package workerhealth answers one question: is anything consuming the queues?
// Each worker writes a short-lived key to Redis, and one of them periodically
// writes the fleet count to PostgreSQL so the answer survives a Redis outage.
package workerhealth

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

const (
	// Interval is how often a worker beats; TTL is how long a beat counts.
	// TTL is three intervals, so one missed beat is not an outage.
	Interval = 15 * time.Second
	TTL      = 45 * time.Second

	// AggregateInterval keeps the database write rare.
	AggregateInterval = time.Minute

	// Component is the row this fleet writes in reelpin.service_health.
	Component = "worker-fleet"
)

type Reporter struct {
	client   *redis.Client
	pool     *pgxpool.Pool
	prefix   string
	workerID string
	queues   []string
}

func New(client *redis.Client, pool *pgxpool.Pool, prefix, workerID string, queues []string) *Reporter {
	if prefix == "" {
		prefix = "reelpin:worker"
	}
	return &Reporter{client: client, pool: pool, prefix: prefix, workerID: workerID, queues: queues}
}

// Run beats until the context is cancelled, then clears this worker's key so
// the fleet count drops immediately instead of waiting for the TTL.
func (r *Reporter) Run(ctx context.Context) {
	beat := time.NewTicker(Interval)
	defer beat.Stop()
	aggregate := time.NewTicker(AggregateInterval)
	defer aggregate.Stop()

	r.beat(ctx)
	for {
		select {
		case <-ctx.Done():
			r.clear()
			return
		case <-beat.C:
			r.beat(ctx)
		case <-aggregate.C:
			r.writeAggregate(ctx)
		}
	}
}

func (r *Reporter) key() string { return r.prefix + ":heartbeat:" + r.workerID }

func (r *Reporter) beat(ctx context.Context) {
	if r.client == nil {
		return
	}
	payload, err := json.Marshal(map[string]any{
		"worker_id": r.workerID,
		"queues":    r.queues,
		"beat_at":   time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return
	}
	r.client.Set(ctx, r.key(), payload, TTL)
}

func (r *Reporter) clear() {
	if r.client == nil {
		return
	}
	// A shutdown must not be mistaken for a live worker for another 45 seconds.
	r.client.Del(context.WithoutCancel(context.Background()), r.key())
}

func (r *Reporter) writeAggregate(ctx context.Context) {
	if r.pool == nil {
		return
	}
	count, err := Live(ctx, r.client, r.prefix)
	if err != nil {
		return
	}
	detail, err := json.Marshal(map[string]any{"live_workers": count})
	if err != nil {
		return
	}
	_, _ = r.pool.Exec(ctx, `
		INSERT INTO reelpin.service_health (component, healthy, detail, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (component)
		DO UPDATE SET healthy = EXCLUDED.healthy, detail = EXCLUDED.detail, updated_at = now()`,
		Component, count > 0, detail,
	)
}

// Live counts the workers whose heartbeat has not expired.
func Live(ctx context.Context, client *redis.Client, prefix string) (int, error) {
	if client == nil {
		return 0, fmt.Errorf("no redis client")
	}
	if prefix == "" {
		prefix = "reelpin:worker"
	}

	var cursor uint64
	count := 0
	for {
		keys, next, err := client.Scan(ctx, cursor, prefix+":heartbeat:*", 100).Result()
		if err != nil {
			return 0, fmt.Errorf("counting worker heartbeats: %w", err)
		}
		count += len(keys)
		if next == 0 {
			return count, nil
		}
		cursor = next
	}
}
