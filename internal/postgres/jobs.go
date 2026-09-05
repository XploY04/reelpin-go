package postgres

import (
	"context"
	"fmt"

	"github.com/XploY04/reelpin-go/internal/jobs"
	"github.com/XploY04/reelpin-go/internal/uuid"
	"github.com/jackc/pgx/v5"
)

// JobColumns is the shared column list, so anything that reads a job row reads
// the same shape.
const JobColumns = `id, user_id, url, normalized_url, source_platform, source_content_type,
	source_content_id, processing_version, ingestion_method, transcript_source, status,
	current_step, progress_percent, failure_code, error_message, result_reel_id,
	attempt_count, max_attempts, next_retry_at, step_durations, collection_ids, created_at,
	updated_at, started_at, completed_at`

type Jobs struct {
	db Querier
}

func NewJobs(db Querier) *Jobs {
	return &Jobs{db: db}
}

func (j *Jobs) List(ctx context.Context, userID string, activeOnly bool, limit int) ([]jobs.JobRecord, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	query := "SELECT " + JobColumns + " FROM processing_jobs WHERE user_id = $1"
	args := []any{userID}
	if activeOnly {
		args = append(args, []string{jobs.StatusQueued, jobs.StatusProcessing})
		query += " AND status = ANY($2)"
	}
	args = append(args, limit)
	query += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d", len(args))

	rows, err := j.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	records := []jobs.JobRecord{}
	for rows.Next() {
		record, err := ScanJob(rows)
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

	rows, err := j.db.Query(ctx,
		"SELECT "+JobColumns+" FROM processing_jobs WHERE id = $1 AND user_id = $2",
		id.String(), userID,
	)
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
	record, err := ScanJob(rows)
	if err != nil {
		return jobs.JobRecord{}, err
	}
	return record, rows.Err()
}

// ScanJob reads one row selected with JobColumns.
func ScanJob(rows pgx.Rows) (jobs.JobRecord, error) {
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
