package lifecycle

import (
	"context"
	"fmt"
	"time"

	"github.com/XploY04/reelpin-go/internal/storage"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Retention windows. Each one is how long the data is still useful for
// answering a question, not how long it is interesting.
const (
	// A stage result's raw provider output is diagnostic. After a run has
	// finished for this long, nobody is debugging it any more.
	StageResultRetention = 14 * 24 * time.Hour
	// A published outbox event has already done its job; it is kept only long
	// enough to explain a delivery someone is asking about.
	PublishedEventRetention = 7 * 24 * time.Hour
	// A workspace older than this belongs to a worker that died: a live job
	// renews its lease every 30 seconds.
	AbandonedWorkspaceAge = time.Hour
)

// RetentionReport is what one sweep removed.
type RetentionReport struct {
	IdempotencyKeys    int `json:"idempotency_keys"`
	PublishedEvents    int `json:"published_events"`
	StageResults       int `json:"stage_results"`
	AbandonedWorkspace int `json:"abandoned_workspaces"`
}

// Retention removes temporary data that has outlived its purpose. It is
// deliberately a scheduled command rather than something the request path
// does: bulk deletes belong where they can be watched.
type Retention struct {
	pool *pgxpool.Pool
	// workspaceRoot is where the worker keeps per-job scratch space. Empty
	// means this process is not the one with the disk.
	workspaceRoot string
}

func NewRetention(pool *pgxpool.Pool, workspaceRoot string) *Retention {
	return &Retention{pool: pool, workspaceRoot: workspaceRoot}
}

// Sweep runs every retention rule once and reports what went.
func (r *Retention) Sweep(ctx context.Context, now time.Time) (RetentionReport, error) {
	report := RetentionReport{}

	// An expired idempotency key can no longer answer a retry, so it is only
	// taking up space.
	count, err := r.exec(ctx, `DELETE FROM reelpin.idempotency_keys WHERE expires_at < now()`)
	if err != nil {
		return report, fmt.Errorf("removing expired idempotency keys: %w", err)
	}
	report.IdempotencyKeys = count

	count, err = r.exec(ctx, `
		DELETE FROM reelpin.outbox_events
		WHERE published_at IS NOT NULL AND published_at < now() - $1::interval`,
		PublishedEventRetention.String())
	if err != nil {
		return report, fmt.Errorf("removing published events: %w", err)
	}
	report.PublishedEvents = count

	// Only results for runs that reached a terminal state: an unfinished run
	// still needs its checkpoints to resume.
	count, err = r.exec(ctx, `
		DELETE FROM reelpin.processing_stage_results sr
		USING reelpin.processing_runs r
		WHERE sr.run_id = r.id
		  AND r.status IN ('completed', 'failed')
		  AND r.updated_at < now() - $1::interval`,
		StageResultRetention.String())
	if err != nil {
		return report, fmt.Errorf("removing old stage results: %w", err)
	}
	report.StageResults = count

	if r.workspaceRoot != "" {
		swept, err := storage.Sweep(r.workspaceRoot, AbandonedWorkspaceAge, now)
		if err != nil {
			return report, fmt.Errorf("sweeping abandoned workspaces: %w", err)
		}
		report.AbandonedWorkspace = swept
	}

	return report, nil
}

func (r *Retention) exec(ctx context.Context, statement string, args ...any) (int, error) {
	tag, err := r.pool.Exec(ctx, statement, args...)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
