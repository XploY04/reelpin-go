package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

func CheckMigrationIdle(ctx context.Context, pool *pgxpool.Pool, bootstrap bool) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	if bootstrap {
		var empty bool
		if err := pool.QueryRow(ctx, `SELECT to_regnamespace('reelpin') IS NULL
			AND to_regnamespace('auth') IS NULL AND to_regnamespace('legacy') IS NULL
			AND NOT EXISTS (
				SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
				WHERE c.relkind IN ('r', 'p') AND n.nspname NOT LIKE 'pg_%'
				AND n.nspname != 'information_schema'
				AND NOT EXISTS (SELECT 1 FROM pg_depend d
					WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e')
			)`).Scan(&empty); err != nil {
			return fmt.Errorf("checking bootstrap database: %w", err)
		}
		if !empty {
			return errors.New("migration blocked: empty broker requires an uninitialized database")
		}
		return nil
	}

	var tables int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'reelpin' AND c.relname IN
		('processing_jobs', 'processing_runs', 'outbox_events') AND c.relkind = 'r'`).Scan(&tables); err != nil {
		return fmt.Errorf("checking migration tables: %w", err)
	}
	if tables != 3 {
		return errors.New("migration requires a complete work schema")
	}
	var jobs, runs, events int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM reelpin.processing_jobs WHERE status IN ('queued', 'processing')),
		(SELECT count(*) FROM reelpin.processing_runs WHERE status IN ('queued', 'processing', 'retry_scheduled')),
		(SELECT count(*) FROM reelpin.outbox_events WHERE published_at IS NULL)`).Scan(&jobs, &runs, &events); err != nil {
		return fmt.Errorf("checking unfinished work: %w", err)
	}
	if jobs != 0 || runs != 0 || events != 0 {
		return fmt.Errorf("migration blocked: unfinished jobs=%d runs=%d unpublished_events=%d", jobs, runs, events)
	}
	return nil
}
