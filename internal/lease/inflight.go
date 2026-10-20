package lease

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// MarkerStore is the slice of a Redis client the in-flight marker needs.
type MarkerStore interface {
	Set(ctx context.Context, key string, value any, expiration time.Duration) *redis.StatusCmd
	Exists(ctx context.Context, keys ...string) *redis.IntCmd
}

// Inflight is a Redis key that exists while any worker holds a lease. It
// outlives the newest lease by one more Duration, so a lease that expires
// because its worker died is still inside the window the sweeper looks in.
// When no work is in flight the sweeper has nothing to find and leaves
// PostgreSQL alone.
//
// A nil Inflight is valid: nothing is marked and the sweeper sweeps on every
// tick, as it did before the marker existed.
type Inflight struct {
	store  MarkerStore
	key    string
	logger *slog.Logger
}

func NewInflight(store MarkerStore, prefix string, logger *slog.Logger) *Inflight {
	if store == nil {
		return nil
	}
	if prefix == "" {
		prefix = "reelpin"
	}
	return &Inflight{store: store, key: prefix + ":lease:inflight", logger: logger}
}

func (i *Inflight) mark(ctx context.Context) {
	if i == nil {
		return
	}
	if err := i.store.Set(ctx, i.key, 1, 2*Duration).Err(); err != nil {
		// The lease itself is in PostgreSQL and is unaffected; what is lost is
		// the sweeper's hint, and the sweeper treats an unreadable hint as
		// a reason to sweep.
		i.logger.Warn("could not mark a lease in flight", "error", err)
	}
}

// active reports whether any lease was taken or renewed recently.
func (i *Inflight) active(ctx context.Context) (bool, error) {
	count, err := i.store.Exists(ctx, i.key).Result()
	if err != nil {
		return false, err
	}
	return count > 0, nil
}
