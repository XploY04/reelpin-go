package postgres

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/XploY04/reelpin-go/internal/jobs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const jobColumns = `id, user_id, url, normalized_url, source_platform, source_content_type,
	source_content_id, processing_version, ingestion_method, transcript_source, status,
	current_step, progress_percent, failure_code, error_message, result_reel_id,
	attempt_count, max_attempts, next_retry_at, step_durations, collection_ids, created_at,
	updated_at, started_at, completed_at`

type Jobs struct {
	db    Querier
	table string
}

// CombinedJobs reads jobs created by Go first and the legacy Python jobs
// second. During coexistence both writers are live, and returning only one
// table makes an accepted job disappear from polling immediately.
type CombinedJobs struct {
	canonical *CanonicalJobs
	legacy    *Jobs
}

type CanonicalJobs struct {
	db Querier
}

func NewJobs(db Querier) *Jobs {
	return newJobs(db, "public.processing_jobs")
}

func newJobs(db Querier, table string) *Jobs { return &Jobs{db: db, table: table} }

func NewCombinedJobs(db Querier) *CombinedJobs {
	return &CombinedJobs{canonical: &CanonicalJobs{db: db}, legacy: NewJobs(db)}
}

const canonicalJobColumns = `j.id::text, j.user_id::text, j.url, NULLIF(j.normalized_url, ''),
	NULLIF(j.source_platform, ''), NULL::text, NULL::text, r.processor_version,
	NULL::text, NULL::text, j.status, j.current_step, j.progress_percent,
	j.failure_code, r.failure_message, j.user_save_id::text,
	COALESCE(r.attempt_count, 0), COALESCE(r.max_attempts, 3), NULL::timestamptz,
	'{}'::jsonb, to_jsonb(j.collection_ids), j.created_at, j.updated_at,
	NULL::timestamptz, j.completed_at`

func (j *CanonicalJobs) List(ctx context.Context, userID string, activeOnly bool, limit int) ([]jobs.JobRecord, error) {
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

func (j *CanonicalJobs) Get(ctx context.Context, userID string, id uuid.UUID) (jobs.JobRecord, error) {
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

func (j *CombinedJobs) List(ctx context.Context, userID string, activeOnly bool, limit int) ([]jobs.JobRecord, error) {
	canonical, err := j.canonical.List(ctx, userID, activeOnly, limit)
	if err != nil && !undefinedTable(err) {
		return nil, err
	}
	legacy, err := j.legacy.List(ctx, userID, activeOnly, limit)
	if err != nil && !undefinedTable(err) {
		return nil, err
	}

	seen := make(map[string]bool, len(canonical))
	combined := append([]jobs.JobRecord{}, canonical...)
	for _, record := range canonical {
		seen[record.ID] = true
	}
	for _, record := range legacy {
		if !seen[record.ID] {
			combined = append(combined, record)
		}
	}
	sort.SliceStable(combined, func(a, b int) bool {
		if combined[a].CreatedAt == nil {
			return false
		}
		if combined[b].CreatedAt == nil {
			return true
		}
		return combined[a].CreatedAt.After(*combined[b].CreatedAt)
	})
	if len(combined) > limit {
		combined = combined[:limit]
	}
	return combined, nil
}

func (j *CombinedJobs) Get(ctx context.Context, userID string, id uuid.UUID) (jobs.JobRecord, error) {
	record, err := j.canonical.Get(ctx, userID, id)
	if err == nil {
		return record, nil
	}
	if !errors.Is(err, jobs.ErrNotFound) && !undefinedTable(err) {
		return jobs.JobRecord{}, err
	}
	return j.legacy.Get(ctx, userID, id)
}

func undefinedTable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42P01"
}

func (j *Jobs) List(ctx context.Context, userID string, activeOnly bool, limit int) ([]jobs.JobRecord, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	query := "SELECT " + jobColumns + " FROM " + j.table + " WHERE user_id = $1"
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

	rows, err := j.db.Query(ctx,
		"SELECT "+jobColumns+" FROM "+j.table+" WHERE id = $1 AND user_id = $2",
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
