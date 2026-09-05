// Package workerhealth answers one question: is anything consuming the queues?
// Each worker writes a short-lived key to Redis. The state is disposable: a
// Redis restart forgets the fleet for at most one heartbeat interval, and
// nothing durable depends on it.
package workerhealth

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// Interval is how often a worker beats. Stale is when readiness stops
	// counting a worker: six missed beats, per the implementation plan, so a
	// paused process or a slow scheduler does not read as an outage.
	Interval = 15 * time.Second
	Stale    = 90 * time.Second
)

type Reporter struct {
	client   *redis.Client
	prefix   string
	workerID string
	queues   []string
}

func New(client *redis.Client, prefix, workerID string, queues []string) *Reporter {
	if prefix == "" {
		prefix = "reelpin:worker"
	}
	return &Reporter{client: client, prefix: prefix, workerID: workerID, queues: queues}
}

// Run beats until the context is cancelled, then clears this worker's key so
// the fleet count drops immediately instead of waiting out the stale window.
func (r *Reporter) Run(ctx context.Context) {
	beat := time.NewTicker(Interval)
	defer beat.Stop()

	r.beat(ctx)
	for {
		select {
		case <-ctx.Done():
			r.clear()
			return
		case <-beat.C:
			r.beat(ctx)
		}
	}
}

func (r *Reporter) key() string { return r.prefix + ":heartbeat:" + r.workerID }

func (r *Reporter) beat(ctx context.Context) {
	if r.client == nil {
		return
	}
	// A worker id is infrastructure, not user data, so it is stored as is.
	payload, err := json.Marshal(map[string]any{
		"worker_id": r.workerID,
		"queues":    r.queues,
		"beat_at":   time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return
	}
	// The key expires at the stale threshold, so Live never counts a worker
	// readiness would already call dead.
	r.client.Set(ctx, r.key(), payload, Stale)
}

func (r *Reporter) clear() {
	if r.client == nil {
		return
	}
	// A shutdown must not be mistaken for a live worker for another 90 seconds.
	r.client.Del(context.WithoutCancel(context.Background()), r.key())
}

// Live counts the workers whose heartbeat has not gone stale.
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
