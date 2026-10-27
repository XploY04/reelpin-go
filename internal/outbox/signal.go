package outbox

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// FlagStore is the slice of a Redis client the signal needs.
type FlagStore interface {
	Set(ctx context.Context, key string, value any, expiration time.Duration) *redis.StatusCmd
	GetDel(ctx context.Context, key string) *redis.StringCmd
}

// Signal tells the dispatcher that a transaction committed outbox rows. It
// exists so the dispatcher can ask Redis every second instead of PostgreSQL:
// a database that is asked nothing is allowed to suspend.
//
// A nil Signal is valid and does nothing, which is what a process without
// Redis has.
type Signal struct {
	store  FlagStore
	key    string
	logger *slog.Logger
}

func NewSignal(store FlagStore, prefix string, logger *slog.Logger) *Signal {
	if store == nil {
		return nil
	}
	if prefix == "" {
		prefix = "reelpin"
	}
	return &Signal{store: store, key: prefix + ":outbox:dirty", logger: logger}
}

// Notify marks the outbox dirty. Call it after the commit, never before: a
// dispatcher woken early finds nothing and goes back to sleep.
func (s *Signal) Notify(ctx context.Context) {
	if s == nil {
		return
	}
	if err := s.store.Set(ctx, s.key, 1, 0).Err(); err != nil {
		// The row is committed and will go out on the next signal or the next
		// worker start; this line is how a stuck event gets explained.
		s.logger.Error("could not signal the outbox dispatcher", "error", err)
	}
}

// take reports whether the outbox was marked dirty and clears the mark in the
// same command, so a write that lands during the dispatch marks it again.
func (s *Signal) take(ctx context.Context) (bool, error) {
	err := s.store.GetDel(ctx, s.key).Err()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
