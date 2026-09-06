// Package lease is the database's answer to at-least-once delivery: a run may
// only be worked on while its lease is held, and every commit is fenced by the
// lease generation, so a worker that lost its lease cannot write a stale
// result no matter how long it was paused.
package lease

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotAcquired means someone else holds a live lease, or the run is not in a
// workable state.
var ErrNotAcquired = errors.New("lease not acquired")

// ErrFenced means this owner's generation is no longer current: renewal or a
// commit matched zero rows. The worker cancels its work and discards the
// result; a newer claim owns the run now.
var ErrFenced = errors.New("lease fenced")

const (
	// Duration comfortably exceeds the renewal interval so one slow renewal
	// does not lose the run.
	Duration        = 2 * time.Minute
	RenewalInterval = 30 * time.Second
)

// Lease is one claim on one run. Generation only ever grows, and it grows on
// PostgreSQL's clock and PostgreSQL's row, never in process memory.
type Lease struct {
	RunID      string
	Owner      string
	Generation int64
	ExpiresAt  time.Time
}

// Acquire claims a run. Claiming increments the generation, including when the
// same owner re-claims after a crash: the increment is what fences the past.
func Acquire(ctx context.Context, pool *pgxpool.Pool, runID, owner string) (Lease, error) {
	var lease Lease
	err := pool.QueryRow(ctx, `
		UPDATE reelpin.processing_runs
		SET lease_owner = $2,
		    lease_expires_at = now() + $3::interval,
		    lease_generation = lease_generation + 1,
		    status = 'processing',
		    updated_at = now()
		WHERE id = $1
		  AND status IN ('queued', 'processing', 'retry_scheduled')
		  AND (lease_owner IS NULL OR lease_expires_at < now())
		RETURNING id::text, lease_owner, lease_generation, lease_expires_at`,
		runID, owner, Duration.String(),
	).Scan(&lease.RunID, &lease.Owner, &lease.Generation, &lease.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, ErrNotAcquired
	}
	if err != nil {
		return Lease{}, fmt.Errorf("acquiring the run lease: %w", err)
	}
	return lease, nil
}

// Renew extends this exact claim. Zero rows means the generation moved on:
// the worker must stop and discard, never retry the renewal.
func Renew(ctx context.Context, pool *pgxpool.Pool, lease Lease) (Lease, error) {
	err := pool.QueryRow(ctx, `
		UPDATE reelpin.processing_runs
		SET lease_expires_at = now() + $4::interval, updated_at = now()
		WHERE id = $1 AND lease_owner = $2 AND lease_generation = $3
		  AND status = 'processing'
		RETURNING lease_expires_at`,
		lease.RunID, lease.Owner, lease.Generation, Duration.String(),
	).Scan(&lease.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, ErrFenced
	}
	if err != nil {
		return Lease{}, fmt.Errorf("renewing the run lease: %w", err)
	}
	return lease, nil
}

// GuardedExec runs one statement inside the caller's transaction with the
// fencing predicate appended via a run-row lock: the statement only proceeds
// if this claim is still current. Use it for every state commit a worker makes.
func GuardedExec(ctx context.Context, tx pgx.Tx, lease Lease, do func(pgx.Tx) error) error {
	tag, err := tx.Exec(ctx, `
		UPDATE reelpin.processing_runs
		SET updated_at = now()
		WHERE id = $1 AND lease_owner = $2 AND lease_generation = $3`,
		lease.RunID, lease.Owner, lease.Generation,
	)
	if err != nil {
		return fmt.Errorf("checking the lease before a commit: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrFenced
	}
	return do(tx)
}

// Release gives the lease up without finishing the run, for a clean shutdown.
// It only releases this exact claim.
func Release(ctx context.Context, pool *pgxpool.Pool, lease Lease) error {
	_, err := pool.Exec(ctx, `
		UPDATE reelpin.processing_runs
		SET lease_owner = NULL, lease_expires_at = NULL, status = 'queued', updated_at = now()
		WHERE id = $1 AND lease_owner = $2 AND lease_generation = $3 AND status = 'processing'`,
		lease.RunID, lease.Owner, lease.Generation,
	)
	if err != nil {
		return fmt.Errorf("releasing the run lease: %w", err)
	}
	return nil
}

// resumeNamespace makes sweeper event ids deterministic: one expired
// generation produces exactly one resume event however often the sweeper runs.
var resumeNamespace = uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8")

// SweepExpired requeues runs whose lease expired without a terminal state. The
// old worker stays fenced (its generation is stale after the next Acquire);
// the resume event id is derived from run and generation, so a repeated sweep
// inserts nothing new. The resume takes the routing key of the run's most
// recent outbox event, so media work resumes on the media queue; a run that
// somehow has none falls back to the light queue for inspection.
func SweepExpired(ctx context.Context, pool *pgxpool.Pool, fallbackRoutingKey string, limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}

	transaction, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("starting the sweep: %w", err)
	}
	defer transaction.Rollback(ctx)

	rows, err := transaction.Query(ctx, `
		SELECT id::text, lease_generation
		FROM reelpin.processing_runs
		WHERE status = 'processing' AND lease_expires_at < now()
		ORDER BY lease_expires_at
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return 0, fmt.Errorf("finding expired leases: %w", err)
	}

	type expired struct {
		runID      string
		generation int64
	}
	found := []expired{}
	for rows.Next() {
		var e expired
		if err := rows.Scan(&e.runID, &e.generation); err != nil {
			rows.Close()
			return 0, fmt.Errorf("reading an expired lease: %w", err)
		}
		found = append(found, e)
	}
	rows.Close()
	if rows.Err() != nil {
		return 0, fmt.Errorf("finding expired leases: %w", rows.Err())
	}

	swept := 0
	for _, e := range found {
		if _, err := transaction.Exec(ctx, `
			UPDATE reelpin.processing_runs
			SET status = 'queued', lease_owner = NULL, lease_expires_at = NULL, updated_at = now()
			WHERE id = $1`, e.runID); err != nil {
			return swept, fmt.Errorf("requeueing run %s: %w", e.runID, err)
		}

		eventID := uuid.NewSHA1(resumeNamespace,
			[]byte(fmt.Sprintf("resume:%s:%d", e.runID, e.generation))).String()
		payload := fmt.Sprintf(`{"run_id":%q,"dispatch_generation":%d}`, e.runID, e.generation)
		if _, err := transaction.Exec(ctx, `
			INSERT INTO reelpin.outbox_events
				(event_id, event_type, routing_key, schema_version, payload)
			SELECT $1, $2,
			       COALESCE(
			           (SELECT routing_key FROM reelpin.outbox_events
			            WHERE payload->>'run_id' = $3 AND event_type != $2
			            ORDER BY created_at DESC LIMIT 1),
			           $4),
			       1, $5::jsonb
			ON CONFLICT (event_id) DO NOTHING`,
			eventID, "run.resume", e.runID, fallbackRoutingKey, payload); err != nil {
			return swept, fmt.Errorf("writing the resume event for run %s: %w", e.runID, err)
		}
		swept++
	}

	if err := transaction.Commit(ctx); err != nil {
		return 0, fmt.Errorf("committing the sweep: %w", err)
	}
	return swept, nil
}
