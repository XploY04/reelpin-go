package enqueue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/XploY04/reelpin-go/internal/jobs"
	"github.com/XploY04/reelpin-go/internal/postgres"
	"github.com/XploY04/reelpin-go/internal/queue"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
	"github.com/jackc/pgx/v5"
)

// activeJobStatuses are the states that mean a share is still being worked on.
var activeJobStatuses = []string{jobs.StatusQueued, jobs.StatusProcessing}

// reuseJob answers a re-share from what already exists. A completed job with a
// saved reel, or an in-flight job, both mean there is nothing new to do; the
// collection targets are still merged so the finish files the reel correctly.
func (s *Service) reuseJob(
	ctx context.Context,
	userID string,
	identity sourceidentity.SourceIdentity,
	collectionIDs []string,
) (jobs.JobRecord, bool, error) {
	// Scoped like everything else: a job the same user created in the other
	// deployment is not an answer to a share made in this one.
	var (
		id           string
		status       string
		resultReelID *string
		existingIDs  []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, status, result_reel_id::text, collection_ids
		FROM public.processing_jobs
		WHERE user_id = $1 AND environment = $5
		  AND (normalized_url = $2 OR url = $2
		       OR ($3::text IS NOT NULL AND source_platform = $4 AND source_content_id = $3))
		  AND status IN ('queued', 'processing', 'completed')
		ORDER BY created_at DESC
		LIMIT 1`,
		userID, identity.NormalizedURL, nullableText(identity.ContentID), identity.Platform,
		s.environment,
	).Scan(&id, &status, &resultReelID, &existingIDs)
	if errors.Is(err, pgx.ErrNoRows) {
		return jobs.JobRecord{}, false, nil
	}
	if err != nil {
		return jobs.JobRecord{}, false, fmt.Errorf("looking for an existing job: %w", err)
	}

	// A completed job that never produced a reel is not a usable answer: the
	// user would poll a job that will never carry a save.
	if status == jobs.StatusCompleted && (resultReelID == nil || *resultReelID == "") {
		return jobs.JobRecord{}, false, nil
	}

	if len(collectionIDs) > 0 {
		if err := s.mergeCollectionIDs(ctx, id, existingIDs, collectionIDs); err != nil {
			return jobs.JobRecord{}, false, err
		}
	}

	record, err := s.readJob(ctx, id)
	if err != nil {
		return jobs.JobRecord{}, false, err
	}
	return record, true, nil
}

// mergeCollectionIDs unions the new targets onto the job. Re-sharing into a
// second collection must add to the first, never replace it.
func (s *Service) mergeCollectionIDs(ctx context.Context, jobID string, existingRaw []byte, added []string) error {
	var existing []string
	if len(existingRaw) > 0 && string(existingRaw) != "null" {
		if err := json.Unmarshal(existingRaw, &existing); err != nil {
			return fmt.Errorf("reading job collection ids: %w", err)
		}
	}

	seen := map[string]bool{}
	merged := make([]string, 0, len(existing)+len(added))
	for _, id := range append(existing, added...) {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		merged = append(merged, id)
	}

	encoded, err := json.Marshal(merged)
	if err != nil {
		return fmt.Errorf("encoding job collection ids: %w", err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE public.processing_jobs SET collection_ids = $2, updated_at = now() WHERE id = $1`,
		jobID, encoded,
	); err != nil {
		return fmt.Errorf("merging job collection ids: %w", err)
	}
	return nil
}

func (s *Service) checkSubmissionLimits(ctx context.Context, userID string) error {
	var active, recent int
	if err := s.pool.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE status = ANY($2)),
			count(*) FILTER (WHERE created_at >= now() - interval '1 hour')
		FROM public.processing_jobs
		WHERE user_id = $1`,
		userID, activeJobStatuses,
	).Scan(&active, &recent); err != nil {
		return fmt.Errorf("counting submissions: %w", err)
	}

	if active >= s.limits.ActiveJobs {
		return &LimitError{
			Code:    "too_many_active_jobs",
			Message: "You already have too many reels processing.",
			Detail: fmt.Sprintf("User already has %d active jobs, which meets the active job limit of %d.",
				active, s.limits.ActiveJobs),
		}
	}
	if recent >= s.limits.SubmissionsPerHour {
		return &LimitError{
			Code:    "submission_rate_limited",
			Message: "You have reached the current submission limit.",
			Detail: fmt.Sprintf("User already submitted %d jobs in the last hour, which meets the hourly submission limit of %d.",
				recent, s.limits.SubmissionsPerHour),
		}
	}
	return nil
}

// findOrCreateContent is the global dedup point: one row per identity per
// access scope, whoever shares it.
func findOrCreateContent(ctx context.Context, tx pgx.Tx, identity sourceidentity.SourceIdentity) (string, error) {
	hash := normalizedURLHash(identity)

	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO reelpin.contents
			(source_platform, source_content_type, source_content_id, normalized_url, normalized_url_hash)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT DO NOTHING
		RETURNING id::text`,
		identity.Platform, identity.ContentType, publicContentID(identity), identity.NormalizedURL, hash,
	).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("creating content: %w", err)
	}

	if publicContentID(identity) != nil {
		err = tx.QueryRow(ctx, `
			SELECT id::text FROM reelpin.contents
			WHERE source_platform = $1 AND source_content_type = $2
			  AND source_content_id = $3 AND access_scope_hash = 'public'`,
			identity.Platform, identity.ContentType, identity.ContentID,
		).Scan(&id)
	} else {
		err = tx.QueryRow(ctx, `
			SELECT id::text FROM reelpin.contents
			WHERE normalized_url_hash = $1 AND access_scope_hash = 'public' AND source_content_id IS NULL`,
			hash,
		).Scan(&id)
	}
	if err != nil {
		return "", fmt.Errorf("reading existing content: %w", err)
	}
	return id, nil
}

func currentContentVersion(ctx context.Context, tx pgx.Tx, contentID string) (*string, bool, error) {
	var versionID *string
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(c.current_content_version_id, v.id)::text
		FROM reelpin.contents c
		LEFT JOIN LATERAL (
			SELECT id FROM reelpin.content_versions
			WHERE content_id = c.id AND processor_version = $2
			ORDER BY created_at DESC LIMIT 1
		) v ON true
		WHERE c.id = $1`,
		contentID, ProcessorVersion,
	).Scan(&versionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("reading the current content version: %w", err)
	}
	return versionID, versionID != nil, nil
}

// findOrCreateRun returns the single live run for this content, creating it
// when there is none. Two users sharing at the same moment both land on one
// run: the partial unique index makes the second insert lose, and the loser
// reads the winner's row.
// findOrCreateRun scopes both halves by environment. Without it a dev run
// makes production believe the work is already in flight, and a production
// share silently attaches to a dev run.
func findOrCreateRun(
	ctx context.Context,
	tx pgx.Tx,
	contentID string,
	identity sourceidentity.SourceIdentity,
	hasVersion bool,
	environment string,
) (string, string, error) {
	routing := routingQueue(identity.Platform)
	if hasVersion {
		// Nothing to download: the per-user half is cheap and has its own queue.
		routing = queue.QueuePersonalize
	}

	var runID string
	err := tx.QueryRow(ctx, `
		INSERT INTO reelpin.processing_runs
			(content_id, processor_version, platform, status, stage, max_attempts, environment)
		VALUES ($1, $2, $3, 'queued', 'prepare', $4, $5)
		ON CONFLICT DO NOTHING
		RETURNING id::text`,
		contentID, ProcessorVersion, identity.Platform, defaultMaxAttempts, environment,
	).Scan(&runID)
	if err == nil {
		return runID, routing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", "", fmt.Errorf("creating a processing run: %w", err)
	}

	if err := tx.QueryRow(ctx, `
		SELECT id::text FROM reelpin.processing_runs
		WHERE content_id = $1 AND processor_version = $2 AND environment = $3
		  AND status IN ('queued', 'processing', 'retry_scheduled')
		ORDER BY created_at
		LIMIT 1`,
		contentID, ProcessorVersion, environment,
	).Scan(&runID); err != nil {
		return "", "", fmt.Errorf("reading the live processing run: %w", err)
	}
	return runID, routing, nil
}

func createJob(
	ctx context.Context,
	tx pgx.Tx,
	request Request,
	identity sourceidentity.SourceIdentity,
	runID string,
	collectionIDs []string,
	environment string,
) (jobs.JobRecord, error) {
	encoded, err := json.Marshal(collectionIDs)
	if err != nil {
		return jobs.JobRecord{}, fmt.Errorf("encoding collection ids: %w", err)
	}

	ingestion := request.IngestionMethod
	if ingestion == "" {
		ingestion = "url_share"
	}

	var jobID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO public.processing_jobs
			(user_id, url, normalized_url, source_platform, source_content_type, source_content_id,
			 processing_version, ingestion_method, status, current_step, max_attempts,
			 collection_ids, processing_run_id, environment)
		VALUES ($1, $2, $2, $3, $4, $5, $6, $7, 'queued', 'queued', $8, $9, $10, $11)
		RETURNING id::text`,
		request.UserID, identity.NormalizedURL, identity.Platform, identity.ContentType,
		nullableText(identity.ContentID), ProcessorVersion, ingestion, defaultMaxAttempts,
		encoded, runID, environment,
	).Scan(&jobID); err != nil {
		return jobs.JobRecord{}, fmt.Errorf("creating the processing job: %w", err)
	}

	return readJobTx(ctx, tx, jobID)
}

func routingQueue(platform string) string {
	switch platform {
	case "instagram":
		return queue.QueueInstagram
	case "youtube":
		return queue.QueueYouTube
	case "tiktok":
		return queue.QueueTikTok
	case "linkedin":
		return queue.QueueLinkedIn
	case "reddit":
		return queue.QueueReddit
	default:
		// Everything else is a page fetch, which is what the web queue is for.
		return queue.QueueWeb
	}
}

func publicContentID(identity sourceidentity.SourceIdentity) *string {
	if identity.ContentType == "link" || identity.ContentID == "" {
		return nil
	}
	id := identity.ContentID
	return &id
}

func normalizedURLHash(identity sourceidentity.SourceIdentity) string {
	sum := sha256.Sum256([]byte(identity.NormalizedURL))
	return hex.EncodeToString(sum[:])
}

func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// readJob and readJobTx return the same shape the job endpoints serve, so an
// enqueue response and a later poll cannot disagree.
func (s *Service) readJob(ctx context.Context, jobID string) (jobs.JobRecord, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT "+postgres.JobColumns+" FROM public.processing_jobs WHERE id = $1", jobID)
	if err != nil {
		return jobs.JobRecord{}, fmt.Errorf("reading the processing job: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return jobs.JobRecord{}, jobs.ErrNotFound
	}
	return postgres.ScanJob(rows)
}

func readJobTx(ctx context.Context, tx pgx.Tx, jobID string) (jobs.JobRecord, error) {
	rows, err := tx.Query(ctx,
		"SELECT "+postgres.JobColumns+" FROM public.processing_jobs WHERE id = $1", jobID)
	if err != nil {
		return jobs.JobRecord{}, fmt.Errorf("reading the processing job: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return jobs.JobRecord{}, jobs.ErrNotFound
	}
	return postgres.ScanJob(rows)
}
