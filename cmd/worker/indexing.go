package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/XploY04/reelpin-go/internal/embed"
	"github.com/XploY04/reelpin-go/internal/queue"
	"github.com/jackc/pgx/v5/pgxpool"
)

// indexHandler embeds a run's content version after it is saved.
//
// Indexing is separate durable work, not a pipeline stage: a provider outage
// here must never turn a completed save into a failed job. The user has their
// reel; only its searchability is late.
func indexHandler(pool *pgxpool.Pool, indexer *embed.Indexer, logger *slog.Logger) queue.Handler {
	return func(ctx context.Context, message queue.Message) (queue.Outcome, error) {
		var versionID string
		err := pool.QueryRow(ctx, `
			SELECT c.current_version_id::text
			FROM reelpin.processing_runs r
			JOIN reelpin.contents c ON c.id = r.content_id
			WHERE r.id = $1 AND c.current_version_id IS NOT NULL`,
			message.RunID).Scan(&versionID)
		if err != nil {
			// No current version means the run did not finish, or finished and
			// was superseded. Either way there is nothing to index and nothing
			// to retry.
			logger.Debug("nothing to index for this run", "run_id", message.RunID)
			return queue.Outcome{Kind: queue.Done}, nil
		}

		switch err := indexer.IndexVersion(ctx, versionID); {
		case err == nil:
			return queue.Outcome{Kind: queue.Done}, nil
		case errors.Is(err, embed.ErrNotConfigured):
			// Development without a key: say so once and move on rather than
			// filling the retry queue.
			logger.Debug("indexing skipped: no embedding key configured", "run_id", message.RunID)
			return queue.Outcome{Kind: queue.Done}, nil
		case errors.Is(err, embed.ErrDimensionMismatch):
			// A configuration fault, not a transient one. Retrying cannot fix
			// it, and storing the vector would corrupt the set.
			logger.Error("indexing refused a mismatched vector", "run_id", message.RunID, "error", err)
			return queue.Outcome{Kind: queue.DeadLetter}, err
		default:
			return queue.Outcome{Kind: queue.Retry, Attempt: 1}, err
		}
	}
}
