package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"

	"github.com/XploY04/reelpin-go/internal/config"
	"github.com/XploY04/reelpin-go/internal/db"
	"github.com/XploY04/reelpin-go/internal/queue"
	"github.com/google/uuid"
)

// rebuildNamespace matches the sweeper's derivation, so a rebuild and a sweep
// can never double-insert a resume for the same lease generation.
var rebuildNamespace = uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8")

// runRebuildQueue reconstructs broker work from PostgreSQL after broker loss.
// Everything a worker needs lives in the database — envelopes carry
// identifiers only — so an empty broker is an inconvenience, not data loss:
// every unfinished run gets one uniquely keyed resume event, and unpublished
// outbox rows republish on their own. Raw dead-letter payloads are never
// replayed.
func runRebuildQueue(ctx context.Context, logger *slog.Logger, cfg config.Config, args []string) error {
	flags := flag.NewFlagSet("rebuild-queue", flag.ContinueOnError)
	brokerEmpty := flags.Bool("broker-empty", false,
		"assert the broker lost its state; required, because rebuilding against a live broker duplicates deliveries")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !*brokerEmpty {
		return errors.New("rebuild-queue refuses to run without --broker-empty: against a live broker it duplicates every delivery")
	}

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("db connect: %w", err)
	}
	defer pool.Close()

	transaction, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("starting the rebuild: %w", err)
	}
	defer transaction.Rollback(ctx)

	// Every unfinished run, whatever its stage, gets one resume event keyed by
	// run and lease generation. Runs mid-lease are included: their worker died
	// with the broker.
	rows, err := transaction.Query(ctx, `
		SELECT id::text, lease_generation
		FROM reelpin.processing_runs
		WHERE status IN ('queued', 'processing', 'retry_scheduled')
		FOR UPDATE`)
	if err != nil {
		return fmt.Errorf("finding unfinished runs: %w", err)
	}
	type unfinished struct {
		runID      string
		generation int64
	}
	runs := []unfinished{}
	for rows.Next() {
		var u unfinished
		if err := rows.Scan(&u.runID, &u.generation); err != nil {
			rows.Close()
			return fmt.Errorf("reading a run: %w", err)
		}
		runs = append(runs, u)
	}
	rows.Close()
	if rows.Err() != nil {
		return fmt.Errorf("finding unfinished runs: %w", rows.Err())
	}

	rebuilt := 0
	for _, u := range runs {
		eventID := uuid.NewSHA1(rebuildNamespace,
			[]byte(fmt.Sprintf("resume:%s:%d", u.runID, u.generation))).String()
		payload := fmt.Sprintf(`{"run_id":%q,"dispatch_generation":%d}`, u.runID, u.generation)
		tag, err := transaction.Exec(ctx, `
			INSERT INTO reelpin.outbox_events
				(event_id, event_type, routing_key, schema_version, payload)
			SELECT $1, 'run.resume',
			       COALESCE(
			           (SELECT routing_key FROM reelpin.outbox_events
			            WHERE payload->>'run_id' = $2 AND event_type != 'run.resume'
			            ORDER BY created_at DESC LIMIT 1),
			           $3),
			       1, $4::jsonb
			ON CONFLICT (event_id) DO NOTHING`,
			eventID, u.runID, queue.QueueLight, payload)
		if err != nil {
			return fmt.Errorf("writing the resume for run %s: %w", u.runID, err)
		}
		rebuilt += int(tag.RowsAffected())
	}

	// Events published before the loss describe deliveries the broker no
	// longer holds. Ones belonging to unfinished runs are covered by the
	// resumes above; everything else described work that completed.
	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf("committing the rebuild: %w", err)
	}

	var pending int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM reelpin.outbox_events WHERE published_at IS NULL`).Scan(&pending); err != nil {
		return fmt.Errorf("counting pending events: %w", err)
	}

	logger.Info("queue rebuilt from postgres",
		"unfinished_runs", len(runs),
		"resume_events_written", rebuilt,
		"events_pending_publish", pending,
	)
	return nil
}
