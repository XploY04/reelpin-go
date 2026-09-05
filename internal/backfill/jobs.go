package backfill

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/sourceidentity"
	"github.com/jackc/pgx/v5"
)

type jobRow struct {
	ID              string
	UserID          string
	URL             string
	NormalizedURL   *string
	Status          string
	FailureCode     *string
	ResultReelID    *string
	AttemptCount    int
	MaxAttempts     int
	ProcessingRunID *string
	CreatedAt       *time.Time
	CompletedAt     *time.Time
}

// linkJobs attaches recent private jobs to a reconstructed global run. A job
// whose identity or processor version is ambiguous is left alone and counted,
// never guessed at.
func (b *Backfiller) linkJobs(ctx context.Context, options Options, report *Report) error {
	cursor, err := b.cursor(ctx, "processing_jobs")
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-jobLinkWindow)

	batch := int64(0)
	for {
		rows, err := b.readJobs(ctx, cursor, cutoff, options.BatchSize)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return b.finishCursor(ctx, options, "processing_jobs")
		}
		batch++

		for _, row := range rows {
			cursor = row.ID
			report.JobsScanned++
			if err := b.linkOneJob(ctx, options, batch, row, report); err != nil {
				return err
			}
		}

		if err := b.saveCursor(ctx, options, "processing_jobs", cursor, report.JobsScanned); err != nil {
			return err
		}
	}
}

func (b *Backfiller) linkOneJob(ctx context.Context, options Options, batch int64, row jobRow, report *Report) error {
	if row.ProcessingRunID != nil {
		return nil
	}

	// A job that is still queued or processing belongs to the Python worker
	// that is still running it. Reconstructing a terminal run for it would
	// misreport live work.
	if !isTerminalJob(row.Status) {
		report.JobsUncertain++
		return b.audit(ctx, options, batch, "processing_jobs", row.ID, "skipped_active", nil, nil, nil,
			"job is still active")
	}

	sourceURL := strings.TrimSpace(row.URL)
	if sourceURL == "" && row.NormalizedURL != nil {
		sourceURL = strings.TrimSpace(*row.NormalizedURL)
	}
	identity, err := b.resolver.Resolve(ctx, sourceURL)
	if err != nil {
		report.JobsUncertain++
		return b.audit(ctx, options, batch, "processing_jobs", row.ID, "skipped_ambiguous", nil, nil, nil,
			"identity could not be derived")
	}

	if !options.Execute {
		report.JobsLinked++
		return nil
	}

	transaction, err := b.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("starting the job transaction: %w", err)
	}
	defer transaction.Rollback(ctx)

	// The content must already exist from the reel pass. If it does not, this
	// job describes something no save survived, and linking it would invent
	// global content nobody asked for.
	var contentID string
	if publicContentID(identity) != nil {
		err = transaction.QueryRow(ctx, `
			SELECT id FROM reelpin.contents
			WHERE source_platform = $1 AND source_content_type = $2
			  AND source_content_id = $3 AND access_scope_hash = 'public'`,
			identity.Platform, identity.ContentType, identity.ContentID,
		).Scan(&contentID)
	} else {
		err = transaction.QueryRow(ctx, `
			SELECT id FROM reelpin.contents
			WHERE normalized_url_hash = $1 AND access_scope_hash = 'public' AND source_content_id IS NULL`,
			urlHash(identity),
		).Scan(&contentID)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		report.JobsUncertain++
		return b.audit(ctx, options, batch, "processing_jobs", row.ID, "skipped_ambiguous", nil, nil, nil,
			"no global content for this job")
	}
	if err != nil {
		return fmt.Errorf("reading content for a job: %w", err)
	}

	runID, created, err := b.reconstructRun(ctx, transaction, contentID, identity, row)
	if err != nil {
		return err
	}
	if created {
		report.RunsCreated++
	}

	tag, err := transaction.Exec(ctx,
		`UPDATE public.processing_jobs SET processing_run_id = $1
		 WHERE id = $2 AND processing_run_id IS NULL`,
		runID, row.ID,
	)
	if err != nil {
		return fmt.Errorf("linking job: %w", err)
	}
	if tag.RowsAffected() == 1 {
		report.JobsLinked++
	} else {
		report.Conflicts++
	}

	if err := auditTx(ctx, transaction, batch, "processing_jobs", row.ID, "linked", &contentID, nil, &runID, ""); err != nil {
		return err
	}
	return transaction.Commit(ctx)
}

// reconstructRun records history, not work to do. Every reconstructed run is
// terminal, so a worker can never pick one up and it can never block a live run.
func (b *Backfiller) reconstructRun(
	ctx context.Context,
	tx pgx.Tx,
	contentID string,
	identity sourceidentity.SourceIdentity,
	row jobRow,
) (string, bool, error) {
	var existing string
	err := tx.QueryRow(ctx,
		`SELECT id FROM reelpin.processing_runs
		 WHERE content_id = $1 AND processor_version = $2
		 ORDER BY created_at LIMIT 1`,
		contentID, processorVersion,
	).Scan(&existing)
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, fmt.Errorf("reading an existing run: %w", err)
	}

	var id string
	if err := tx.QueryRow(ctx, `
		INSERT INTO reelpin.processing_runs
			(content_id, processor_version, platform, status, stage, progress_percent,
			 attempt_count, max_attempts, failure_code, created_at, completed_at)
		VALUES ($1, $2, $3, $4, $5, 100, $6, $7, $8, $9, $10)
		RETURNING id`,
		contentID, processorVersion, identity.Platform, terminalStatus(row.Status),
		terminalStatus(row.Status), row.AttemptCount, row.MaxAttempts, row.FailureCode,
		row.CreatedAt, row.CompletedAt,
	).Scan(&id); err != nil {
		return "", false, fmt.Errorf("reconstructing a run: %w", err)
	}
	return id, true, nil
}

// terminalStatus maps a finished private job onto the run state that describes
// it. Only terminal jobs reach here.
func terminalStatus(jobStatus string) string {
	switch strings.ToLower(strings.TrimSpace(jobStatus)) {
	case "completed":
		return "completed"
	case "dead_lettered":
		return "dead_lettered"
	default:
		return "failed"
	}
}

func isTerminalJob(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "failed", "dead_lettered":
		return true
	}
	return false
}

func (b *Backfiller) readJobs(ctx context.Context, cursor string, cutoff time.Time, limit int) ([]jobRow, error) {
	rows, err := b.pool.Query(ctx, `
		SELECT id, user_id, url, normalized_url, status, failure_code, result_reel_id,
		       attempt_count, max_attempts, processing_run_id, created_at, completed_at
		FROM public.processing_jobs
		WHERE ($1::uuid IS NULL OR id > $1::uuid) AND created_at >= $2
		ORDER BY id
		LIMIT $3`,
		nullableUUID(cursor), cutoff, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("reading processing jobs: %w", err)
	}
	defer rows.Close()

	var collected []jobRow
	for rows.Next() {
		var row jobRow
		var status *string
		var attempts, maxAttempts *int
		if err := rows.Scan(
			&row.ID, &row.UserID, &row.URL, &row.NormalizedURL, &status, &row.FailureCode,
			&row.ResultReelID, &attempts, &maxAttempts, &row.ProcessingRunID,
			&row.CreatedAt, &row.CompletedAt,
		); err != nil {
			return nil, fmt.Errorf("reading processing jobs: %w", err)
		}
		row.Status = text(status)
		row.AttemptCount, row.MaxAttempts = number(attempts), number(maxAttempts)
		collected = append(collected, row)
	}
	return collected, rows.Err()
}
