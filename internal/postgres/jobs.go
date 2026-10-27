package postgres

import (
	"context"
	"fmt"

	"github.com/XploY04/reelpin-go/internal/jobs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Jobs reads reelpin.processing_jobs. The legacy backfill carries Python's
// jobs into the same table under their original ids, so there is no second
// source to consult.
type Jobs struct {
	db Querier
}

func NewJobs(db Querier) *Jobs {
	return &Jobs{db: db}
}

const retryWaiting = `(r.status = 'retry_scheduled' AND j.status IN ('queued', 'processing'))`

// canonicalJobColumns present a live job whose run is waiting to retry the way
// the jobs package reads a retry: queued, at step retry_scheduled, due when the
// run.resume event pipeline.scheduleRetry wrote becomes available. The job row
// itself stays processing while the run waits.
const canonicalJobColumns = `j.id::text, j.user_id::text, j.url, NULLIF(j.normalized_url, ''),
	NULLIF(j.source_platform, ''), NULL::text, NULL::text, r.processor_version,
	NULL::text, NULL::text,
	CASE WHEN ` + retryWaiting + ` THEN 'queued' ELSE j.status END,
	CASE WHEN ` + retryWaiting + ` THEN 'retry_scheduled' ELSE j.current_step END,
	j.progress_percent, j.failure_code, r.failure_message, j.user_save_id::text,
	COALESCE(r.attempt_count, 0), COALESCE(r.max_attempts, 3),
	CASE WHEN ` + retryWaiting + ` THEN (
		SELECT max(o.available_at) FROM reelpin.outbox_events o
		WHERE o.event_type = 'run.resume' AND o.payload->>'run_id' = r.id::text
	) END,
	'{}'::jsonb, to_jsonb(j.collection_ids), j.created_at, j.updated_at,
	NULL::timestamptz, j.completed_at`

func (j *Jobs) List(ctx context.Context, userID string, activeOnly bool, limit int) ([]jobs.JobRecord, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	query := `SELECT ` + canonicalJobColumns + `
		FROM reelpin.processing_jobs j
		LEFT JOIN reelpin.processing_runs r ON r.id = j.run_id
		WHERE j.user_id = $1`
	args := []any{userID}
	if activeOnly {
		args = append(args, []string{jobs.StatusQueued, jobs.StatusProcessing})
		query += " AND j.status = ANY($2)"
	}
	args = append(args, limit)
	query += fmt.Sprintf(" ORDER BY j.created_at DESC LIMIT $%d", len(args))

	rows, err := j.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	records := []jobs.JobRecord{}
	for rows.Next() {
		record, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (j *Jobs) Get(ctx context.Context, userID string, id uuid.UUID) (jobs.JobRecord, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	rows, err := j.db.Query(ctx, `SELECT `+canonicalJobColumns+`
		FROM reelpin.processing_jobs j
		LEFT JOIN reelpin.processing_runs r ON r.id = j.run_id
		WHERE j.id = $1 AND j.user_id = $2`, id.String(), userID)
	if err != nil {
		return jobs.JobRecord{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return jobs.JobRecord{}, err
		}
		return jobs.JobRecord{}, jobs.ErrNotFound
	}
	record, err := scanJob(rows)
	if err != nil {
		return jobs.JobRecord{}, err
	}
	return record, rows.Err()
}

func scanJob(rows pgx.Rows) (jobs.JobRecord, error) {
	var (
		record         jobs.JobRecord
		status         *string
		progress       *int
		attempts       *int
		maxAttempts    *int
		durationsRaw   []byte
		collectionsRaw []byte
	)

	if err := rows.Scan(
		&record.ID, &record.UserID, &record.URL, &record.NormalizedURL, &record.SourcePlatform,
		&record.SourceContentType, &record.SourceContentID, &record.ProcessingVersion,
		&record.IngestionMethod, &record.TranscriptSource, &status, &record.CurrentStep,
		&progress, &record.FailureCode, &record.ErrorMessage, &record.ResultReelID,
		&attempts, &maxAttempts, &record.NextRetryAt, &durationsRaw, &collectionsRaw,
		&record.CreatedAt, &record.UpdatedAt, &record.StartedAt, &record.CompletedAt,
	); err != nil {
		return jobs.JobRecord{}, err
	}

	record.Status = text(status)
	record.ProgressPercent = number(progress)
	record.AttemptCount = number(attempts)
	record.MaxAttempts = number(maxAttempts)
	if err := decodeJSON(durationsRaw, &record.StepDurations); err != nil {
		return jobs.JobRecord{}, err
	}
	if err := decodeJSON(collectionsRaw, &record.CollectionIDs); err != nil {
		return jobs.JobRecord{}, err
	}
	return record, nil
}

func number(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}
