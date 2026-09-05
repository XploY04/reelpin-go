// Package lease is the database's answer to at-least-once delivery: a run may
// only be worked on while its lease is held, so a duplicate message finds the
// run taken and stops.
package lease

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotAcquired means someone else holds the lease, or the run is no longer
// in a state that can be worked on.
var ErrNotAcquired = errors.New("lease not acquired")

// Duration is how long a lease is valid without renewal. It must comfortably
// exceed the renewal interval so a slow renewal does not lose the run.
const (
	Duration        = 2 * time.Minute
	RenewalInterval = 30 * time.Second
)

type Lease struct {
	RunID     string
	Owner     string
	ExpiresAt time.Time
}

// Acquire takes a run for this owner. A run whose lease has expired is
// reclaimed, which is how a crashed worker's runs come back.
func Acquire(ctx context.Context, pool *pgxpool.Pool, runID, owner string) (Lease, error) {
	var lease Lease
	err := pool.QueryRow(ctx, `
		UPDATE reelpin.processing_runs
		SET lease_owner = $2,
		    lease_expires_at = now() + $3::interval,
		    status = 'processing',
		    started_at = COALESCE(started_at, now()),
		    attempt_count = attempt_count + 1,
		    updated_at = now()
		WHERE id = $1
		  AND status IN ('queued', 'processing', 'retry_scheduled')
		  AND (lease_owner IS NULL OR lease_owner = $2 OR lease_expires_at < now())
		RETURNING id, lease_owner, lease_expires_at`,
		runID, owner, Duration.String(),
	).Scan(&lease.RunID, &lease.Owner, &lease.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, ErrNotAcquired
	}
	if err != nil {
		return Lease{}, fmt.Errorf("acquiring the run lease: %w", err)
	}
	return lease, nil
}

// Renew extends a lease this owner still holds. A renewal that affects no row
// means the lease was lost, and the caller must stop working.
func Renew(ctx context.Context, pool *pgxpool.Pool, runID, owner string) error {
	tag, err := pool.Exec(ctx, `
		UPDATE reelpin.processing_runs
		SET lease_expires_at = now() + $3::interval, updated_at = now()
		WHERE id = $1 AND lease_owner = $2 AND status = 'processing'`,
		runID, owner, Duration.String(),
	)
	if err != nil {
		return fmt.Errorf("renewing the run lease: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotAcquired
	}
	return nil
}

// Release hands a run back without finishing it, so another worker can pick it
// up immediately instead of waiting for the lease to expire.
func Release(ctx context.Context, pool *pgxpool.Pool, runID, owner string) error {
	_, err := pool.Exec(ctx, `
		UPDATE reelpin.processing_runs
		SET lease_owner = NULL, lease_expires_at = NULL, status = 'queued', updated_at = now()
		WHERE id = $1 AND lease_owner = $2 AND status = 'processing'`,
		runID, owner,
	)
	if err != nil {
		return fmt.Errorf("releasing the run lease: %w", err)
	}
	return nil
}

// KeepAlive renews until the work finishes or the lease is lost. Losing it
// cancels the returned context, so the handler stops instead of writing results
// another worker is already producing.
func KeepAlive(ctx context.Context, pool *pgxpool.Pool, runID, owner string) (context.Context, context.CancelFunc) {
	work, cancel := context.WithCancel(ctx)

	go func() {
		ticker := time.NewTicker(RenewalInterval)
		defer ticker.Stop()

		for {
			select {
			case <-work.Done():
				return
			case <-ticker.C:
				if err := Renew(work, pool, runID, owner); err != nil {
					// Either the lease was taken or the database is unreachable.
					// Both mean this worker must stop touching the run.
					cancel()
					return
				}
			}
		}
	}()

	return work, cancel
}

// ExpiredRuns lists runs whose worker went away, for the maintenance sweep.
func ExpiredRuns(ctx context.Context, pool *pgxpool.Pool, limit int) ([]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT id::text FROM reelpin.processing_runs
		WHERE status = 'processing' AND lease_expires_at < now()
		ORDER BY lease_expires_at
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("reading expired leases: %w", err)
	}
	defer rows.Close()

	var runs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("reading expired leases: %w", err)
		}
		runs = append(runs, id)
	}
	return runs, rows.Err()
}

// ReclaimExpired puts runs whose lease lapsed back on the queue.
func ReclaimExpired(ctx context.Context, pool *pgxpool.Pool, limit int) (int, error) {
	tag, err := pool.Exec(ctx, `
		UPDATE reelpin.processing_runs
		SET status = 'queued', lease_owner = NULL, lease_expires_at = NULL, updated_at = now()
		WHERE id IN (
			SELECT id FROM reelpin.processing_runs
			WHERE status = 'processing' AND lease_expires_at < now()
			ORDER BY lease_expires_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)`, limit)
	if err != nil {
		return 0, fmt.Errorf("reclaiming expired leases: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
