// Package usermigration records which users the Go API has served. During the
// dual run every user lives on exactly one backend, and a user moves the first
// time they reach this one. The legacy backfill reads the record and leaves a
// moved user's rows alone, because from that request on Go is where their data
// changes and the Python rows are frozen.
package usermigration

import (
	"context"
	"log/slog"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Recorder writes each user at most once per process. Every request after the
// first for a user is a map lookup and nothing else.
type Recorder struct {
	record func(ctx context.Context, userID string) error
	logger *slog.Logger
	seen   sync.Map
}

func New(pool *pgxpool.Pool, logger *slog.Logger) *Recorder {
	return &Recorder{
		logger: logger,
		record: func(ctx context.Context, userID string) error {
			_, err := pool.Exec(ctx, `
				INSERT INTO reelpin.user_migrations (user_id) VALUES ($1)
				ON CONFLICT (user_id) DO NOTHING`, userID)
			return err
		},
	}
}

// Seen records that this process served userID. A failed write is logged and
// forgotten, so the user's next request tries again instead of the process
// believing a move it never stored.
func (r *Recorder) Seen(ctx context.Context, userID string) {
	if _, recorded := r.seen.Load(userID); recorded {
		return
	}
	// Concurrent first requests race here; only the one that claims the user
	// writes.
	if _, claimed := r.seen.LoadOrStore(userID, struct{}{}); claimed {
		return
	}
	if err := r.record(ctx, userID); err != nil {
		r.seen.Delete(userID)
		r.logger.Warn("recording a user's first request failed; their next request retries", "error", err)
	}
}
