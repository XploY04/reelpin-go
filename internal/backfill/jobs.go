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
	ID            string
	UserID        string
	URL           string
	NormalizedURL *string
	Status        string
	FailureCode   *string
	AttemptCount  int
	MaxAttempts   int
	CreatedAt     *time.Time
	CompletedAt   *time.Time
}

// copyJobs recreates recent legacy jobs against a reconstructed global run, so
// a job id the app is still holding keeps answering. A job whose identity or
// content is ambiguous is left alone and counted, never guessed at.
func (b *Backfiller) copyJobs(ctx context.Context, options Options, report *Report) error {
	cursor, err := b.cursor(ctx, sourceJobs)
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-jobWindow)

	batch := int64(0)
	for {
		rows, err := b.readJobs(ctx, cursor, cutoff, options.BatchSize)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return b.finishCursor(ctx, options, sourceJobs)
		}
		batch++

		for _, row := range rows {
			cursor = row.ID
			report.JobsScanned++
			if err := b.copyOneJob(ctx, options, batch, row, report); err != nil {
				return err
			}
		}

		if err := b.saveCursor(ctx, options, sourceJobs, cursor, report.JobsScanned); err != nil {
			return err
		}
	}
}

func (b *Backfiller) copyOneJob(ctx context.Context, options Options, batch int64, row jobRow, report *Report) error {
	done, err := b.exists(ctx, "reelpin.processing_jobs", row.ID)
	if err != nil {
		return err
	}
	if done {
		report.JobsAlreadyThere++
		return nil
	}

	// A job still queued or processing belongs to the Python worker that is
	// still running it. Reconstructing a terminal run for it would misreport
	// live work, and copying it would leave a job nothing will ever finish.
	if !isTerminal(row.Status) {
		report.JobsUncertain++
		return b.audit(ctx, options, batch, sourceJobs, row.ID, "skipped_active", nil, nil, nil,
			"the legacy job is still running")
	}

	identity, scopeHash, err := identify(ctx, b.resolver, row.URL, row.NormalizedURL, row.UserID)
	if err != nil {
		report.JobsUncertain++
		return b.audit(ctx, options, batch, sourceJobs, row.ID, "skipped_ambiguous", nil, nil, nil,
			"identity could not be derived")
	}

	if !options.Execute {
		report.JobsCreated++
		return nil
	}

	transaction, err := b.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("starting the job transaction: %w", err)
	}
	defer transaction.Rollback(ctx)

	// The content has to exist from the reel pass already. If it does not, this
	// job describes something no save survived, and creating content for it
	// would invent a global row nobody asked for.
	contentID, err := findContent(ctx, transaction, identity, scopeHash)
	if errors.Is(err, pgx.ErrNoRows) {
		_ = transaction.Rollback(ctx)
		report.JobsUncertain++
		return b.audit(ctx, options, batch, sourceJobs, row.ID, "skipped_ambiguous", nil, nil, nil,
			"no global content for this job")
	}
	if err != nil {
		return err
	}

	runID, created, err := reconstructRun(ctx, transaction, contentID, row)
	if err != nil {
		report.Failures++
		return err
	}
	if created {
		report.RunsCreated++
	}

	var saveID *string
	if err := transaction.QueryRow(ctx, `
		SELECT id::text FROM reelpin.user_saves WHERE user_id = $1 AND content_id = $2`,
		row.UserID, contentID,
	).Scan(&saveID); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("finding the save for a job: %w", err)
	}

	status := jobStatus(row.Status)
	progress := 0
	if status == "completed" {
		progress = 100
	}
	tag, err := transaction.Exec(ctx, `
		INSERT INTO reelpin.processing_jobs
			(id, user_id, run_id, user_save_id, url, normalized_url, source_platform,
			 status, current_step, progress_percent, failure_code,
			 created_at, updated_at, completed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8, $9, $10,
		        coalesce($11, now()), now(), $12)
		ON CONFLICT DO NOTHING`,
		row.ID, row.UserID, runID, saveID, row.URL, identity.NormalizedURL, identity.Platform,
		status, progress, row.FailureCode, row.CreatedAt, row.CompletedAt)
	if err != nil {
		report.Failures++
		return fmt.Errorf("creating the job: %w", err)
	}
	if tag.RowsAffected() == 1 {
		report.JobsCreated++
	} else {
		report.Conflicts++
	}

	if err := auditTx(ctx, transaction, batch, sourceJobs, row.ID, "copied", &contentID, nil, &runID, ""); err != nil {
		return err
	}
	return transaction.Commit(ctx)
}

func findContent(ctx context.Context, tx pgx.Tx, identity sourceidentity.SourceIdentity, scopeHash string) (string, error) {
	var contentID string
	var err error
	if identity.ContentID != "" {
		err = tx.QueryRow(ctx, `
			SELECT id::text FROM reelpin.contents
			WHERE source_platform = $1 AND source_content_type = $2
			  AND source_content_id = $3 AND access_scope_hash = $4`,
			identity.Platform, identity.ContentType, identity.ContentID, scopeHash,
		).Scan(&contentID)
	} else {
		err = tx.QueryRow(ctx, `
			SELECT id::text FROM reelpin.contents
			WHERE normalized_url_hash = $1 AND access_scope_hash = $2 AND source_content_id IS NULL`,
			sourceidentity.URLHash(identity.NormalizedURL), scopeHash,
		).Scan(&contentID)
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("finding the content for a job: %w", err)
	}
	return contentID, err
}

// reconstructRun records history, not work to do. Every reconstructed run is
// terminal, so no worker can lease one and none of them collides with the
// partial unique index that keeps a live run singular.
func reconstructRun(ctx context.Context, tx pgx.Tx, contentID string, row jobRow) (string, bool, error) {
	var existing string
	err := tx.QueryRow(ctx, `
		SELECT id::text FROM reelpin.processing_runs
		WHERE content_id = $1 AND processor_version = $2
		ORDER BY created_at LIMIT 1`,
		contentID, legacyVersion,
	).Scan(&existing)
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, fmt.Errorf("finding an existing legacy run: %w", err)
	}

	status := runStatus(row.Status)
	stage := "prepare"
	if status == "completed" {
		stage = "persist"
	}

	var runID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO reelpin.processing_runs
			(content_id, processor_version, status, stage, attempt_count, max_attempts,
			 failure_code, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, coalesce($8, now()), now())
		RETURNING id::text`,
		contentID, legacyVersion, status, stage, row.AttemptCount, row.MaxAttempts,
		row.FailureCode, row.CreatedAt,
	).Scan(&runID); err != nil {
		return "", false, fmt.Errorf("reconstructing a run: %w", err)
	}
	return runID, true, nil
}

// jobStatus keeps a finished legacy job's own outcome, which the job table
// still spells the same way.
func jobStatus(status string) string {
	switch normalize(status) {
	case "completed":
		return "completed"
	case "dead_lettered":
		return "dead_lettered"
	default:
		return "failed"
	}
}

// runStatus maps that outcome onto the run states, which have no dead-letter of
// their own: a dead-lettered job is a run that failed.
func runStatus(status string) string {
	if normalize(status) == "completed" {
		return "completed"
	}
	return "failed"
}

func isTerminal(status string) bool {
	switch normalize(status) {
	case "completed", "failed", "dead_lettered":
		return true
	}
	return false
}

func normalize(status string) string { return strings.ToLower(strings.TrimSpace(status)) }

func (b *Backfiller) readJobs(ctx context.Context, cursor string, cutoff time.Time, limit int) ([]jobRow, error) {
	rows, err := b.pool.Query(ctx, `
		SELECT id::text, user_id::text, url, normalized_url, status, failure_code,
		       attempt_count, max_attempts, created_at, completed_at
		FROM public.processing_jobs
		WHERE ($1::uuid IS NULL OR id > $1::uuid) AND created_at >= $2
		ORDER BY id
		LIMIT $3`,
		nullableUUID(cursor), cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("reading legacy processing jobs: %w", err)
	}
	defer rows.Close()

	collected := []jobRow{}
	for rows.Next() {
		var row jobRow
		var status *string
		var attempts, maxAttempts *int
		if err := rows.Scan(
			&row.ID, &row.UserID, &row.URL, &row.NormalizedURL, &status, &row.FailureCode,
			&attempts, &maxAttempts, &row.CreatedAt, &row.CompletedAt,
		); err != nil {
			return nil, fmt.Errorf("reading legacy processing jobs: %w", err)
		}
		row.Status = text(status)
		row.AttemptCount, row.MaxAttempts = number(attempts), number(maxAttempts)
		collected = append(collected, row)
	}
	return collected, rows.Err()
}
