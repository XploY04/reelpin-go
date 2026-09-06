package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/XploY04/reelpin-go/internal/enqueue"
	"github.com/XploY04/reelpin-go/internal/outbox"
	"github.com/XploY04/reelpin-go/internal/reels"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Enqueue implements the submission transaction against the canonical schema.
type Enqueue struct {
	pool *pgxpool.Pool
}

func NewEnqueue(pool *pgxpool.Pool) *Enqueue {
	return &Enqueue{pool: pool}
}

// requestHash pins an idempotency key to its body: the same key with different
// content is a client bug the contract answers with 409.
func requestHash(submission enqueue.Submission) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%v",
		submission.Endpoint, submission.URL, submission.RawPayloadText, submission.CollectionIDs)))
	return hex.EncodeToString(sum[:])
}

// storedResult is what an idempotency row replays. It carries just enough to
// rebuild the Result without rerunning anything.
type storedResult struct {
	Kind   enqueue.OutcomeKind `json:"kind"`
	SaveID string              `json:"save_id,omitempty"`
	JobID  string              `json:"job_id,omitempty"`
}

func (e *Enqueue) Submit(ctx context.Context, submission enqueue.Submission) (enqueue.Result, error) {
	transaction, err := e.pool.Begin(ctx)
	if err != nil {
		return enqueue.Result{}, fmt.Errorf("starting the submission: %w", err)
	}
	defer transaction.Rollback(ctx)

	// One submission per user at a time, for the length of this transaction.
	// The advisory lock is what makes the active-job count race-free.
	lockKey := fnv.New64a()
	lockKey.Write([]byte("enqueue:" + submission.UserID))
	if _, err := transaction.Exec(ctx,
		`SELECT pg_advisory_xact_lock($1)`, int64(lockKey.Sum64())); err != nil {
		return enqueue.Result{}, fmt.Errorf("taking the submission lock: %w", err)
	}

	// Idempotency: the same attempt answers the same way for 24 hours; the
	// same key with a different body is a conflict.
	hash := requestHash(submission)
	var storedHash string
	var stored storedResult
	var storedBody []byte
	err = transaction.QueryRow(ctx, `
		SELECT request_hash, response_body FROM reelpin.idempotency_keys
		WHERE user_id = $1 AND endpoint = $2 AND idempotency_key = $3 AND expires_at > now()`,
		submission.UserID, submission.Endpoint, submission.IdempotencyKey,
	).Scan(&storedHash, &storedBody)
	switch {
	case err == nil:
		if storedHash != hash {
			return enqueue.Result{}, enqueue.ErrIdempotencyMismatch
		}
		if err := json.Unmarshal(storedBody, &stored); err != nil {
			return enqueue.Result{}, fmt.Errorf("reading the stored response: %w", err)
		}
		result, err := e.replay(ctx, transaction, submission.UserID, stored)
		if err != nil {
			return enqueue.Result{}, err
		}
		if err := transaction.Commit(ctx); err != nil {
			return enqueue.Result{}, fmt.Errorf("committing the replay: %w", err)
		}
		return result, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return enqueue.Result{}, fmt.Errorf("checking the idempotency key: %w", err)
	}

	result, err := e.submitNew(ctx, transaction, submission, hash)
	if err != nil {
		return enqueue.Result{}, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return enqueue.Result{}, fmt.Errorf("committing the submission: %w", err)
	}
	return result, nil
}

func (e *Enqueue) submitNew(ctx context.Context, tx pgx.Tx, submission enqueue.Submission, hash string) (enqueue.Result, error) {
	if err := requireFilableCollections(ctx, tx, submission.UserID, submission.CollectionIDs); err != nil {
		return enqueue.Result{}, err
	}

	// The cap counts live jobs under the advisory lock, so the third
	// concurrent submission is always the one refused.
	var active int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM reelpin.processing_jobs
		WHERE user_id = $1 AND status IN ('queued', 'processing')`,
		submission.UserID,
	).Scan(&active); err != nil {
		return enqueue.Result{}, fmt.Errorf("counting active jobs: %w", err)
	}
	if active >= enqueue.MaxActiveJobs {
		return enqueue.Result{}, enqueue.ErrActiveJobLimit
	}

	contentID, err := findOrCreateContent(ctx, tx, submission)
	if err != nil {
		return enqueue.Result{}, err
	}

	// Already saved: the contract's 200. No new state at all.
	var saveID string
	var hasVersion bool
	err = tx.QueryRow(ctx, `
		SELECT s.id::text, c.current_version_id IS NOT NULL
		FROM reelpin.user_saves s
		JOIN reelpin.contents c ON c.id = s.content_id
		WHERE s.user_id = $1 AND s.content_id = $2`,
		submission.UserID, contentID,
	).Scan(&saveID, &hasVersion)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return enqueue.Result{}, fmt.Errorf("checking for an existing save: %w", err)
	}
	if err == nil && hasVersion {
		if err := fileSubmission(ctx, tx, submission, saveID); err != nil {
			return enqueue.Result{}, err
		}
		return e.finish(ctx, tx, submission, hash, storedResult{Kind: enqueue.AlreadySaved, SaveID: saveID})
	}

	// Completed public content saved by someone else: a save and an already
	// completed job, with no provider spend.
	var versionID *string
	if err := tx.QueryRow(ctx, `
		SELECT current_version_id::text FROM reelpin.contents WHERE id = $1`,
		contentID,
	).Scan(&versionID); err != nil {
		return enqueue.Result{}, fmt.Errorf("reading the content version: %w", err)
	}
	if versionID != nil {
		if saveID == "" {
			if err := tx.QueryRow(ctx, `
				INSERT INTO reelpin.user_saves (user_id, content_id)
				VALUES ($1, $2)
				ON CONFLICT (user_id, content_id) DO UPDATE SET user_id = EXCLUDED.user_id
				RETURNING id::text`,
				submission.UserID, contentID,
			).Scan(&saveID); err != nil {
				return enqueue.Result{}, fmt.Errorf("creating the save: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO reelpin.processing_jobs
				(user_id, user_save_id, url, normalized_url, source_platform,
				 status, current_step, progress_percent, collection_ids, completed_at)
			VALUES ($1, $2, $3, $4, $5, 'completed', 'completed', 100,
			        COALESCE($6::uuid[], '{}'), now())`,
			submission.UserID, saveID, submission.Identity.OriginalURL,
			submission.Identity.NormalizedURL, submission.Identity.Platform,
			submission.CollectionIDs); err != nil {
			return enqueue.Result{}, fmt.Errorf("recording the reuse job: %w", err)
		}
		if err := fileSubmission(ctx, tx, submission, saveID); err != nil {
			return enqueue.Result{}, err
		}
		return e.finish(ctx, tx, submission, hash, storedResult{Kind: enqueue.AlreadySaved, SaveID: saveID})
	}

	// Unprocessed content: join the live run if one exists, start one if not.
	runID, created, err := findOrCreateRun(ctx, tx, contentID)
	if err != nil {
		return enqueue.Result{}, err
	}

	// One private job per user per run; a duplicate submission reuses it.
	var jobID string
	err = tx.QueryRow(ctx, `
		SELECT id::text FROM reelpin.processing_jobs
		WHERE user_id = $1 AND run_id = $2`,
		submission.UserID, runID,
	).Scan(&jobID)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.QueryRow(ctx, `
			INSERT INTO reelpin.processing_jobs
				(user_id, run_id, url, normalized_url, source_platform, current_step, collection_ids)
			VALUES ($1, $2, $3, $4, $5, 'queued', COALESCE($6::uuid[], '{}'))
			RETURNING id::text`,
			submission.UserID, runID, submission.Identity.OriginalURL,
			submission.Identity.NormalizedURL, submission.Identity.Platform,
			submission.CollectionIDs,
		).Scan(&jobID); err != nil {
			return enqueue.Result{}, fmt.Errorf("creating the job: %w", err)
		}
	} else if err != nil {
		return enqueue.Result{}, fmt.Errorf("finding the existing job: %w", err)
	} else if len(submission.CollectionIDs) > 0 {
		// A second submission of the same link under a new key may name other
		// collections. The job's intent is the union, so neither list is lost.
		if _, err := tx.Exec(ctx, `
			UPDATE reelpin.processing_jobs
			SET collection_ids = ARRAY(SELECT DISTINCT unnest(collection_ids || $2::uuid[])),
			    updated_at = now()
			WHERE id = $1`, jobID, submission.CollectionIDs); err != nil {
			return enqueue.Result{}, fmt.Errorf("recording the collections on the job: %w", err)
		}
	}

	// Only the submission that created the run publishes work. Joining an
	// active run must not publish a second processing event.
	if created {
		if err := outbox.Insert(ctx, tx, outbox.Event{
			EventID:    submission.EventID,
			EventType:  submission.EventType,
			RoutingKey: submission.RoutingKey,
			RunID:      runID,
		}); err != nil {
			return enqueue.Result{}, err
		}
	}

	return e.finish(ctx, tx, submission, hash, storedResult{Kind: enqueue.Accepted, JobID: jobID})
}

// finish stores the idempotent response and loads the wire shapes, inside the
// same transaction as the state they describe.
func (e *Enqueue) finish(ctx context.Context, tx pgx.Tx, submission enqueue.Submission, hash string, stored storedResult) (enqueue.Result, error) {
	body, err := json.Marshal(stored)
	if err != nil {
		return enqueue.Result{}, fmt.Errorf("encoding the stored response: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO reelpin.idempotency_keys
			(user_id, endpoint, idempotency_key, request_hash, response_status, response_body, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, now() + $7::interval)`,
		submission.UserID, submission.Endpoint, submission.IdempotencyKey, hash,
		statusFor(stored.Kind), body, enqueue.IdempotencyLifetime.String()); err != nil {
		return enqueue.Result{}, fmt.Errorf("storing the idempotent response: %w", err)
	}
	return e.replay(ctx, tx, submission.UserID, stored)
}

func statusFor(kind enqueue.OutcomeKind) int {
	if kind == enqueue.AlreadySaved {
		return 200
	}
	return 202
}

// replay rebuilds a Result from its stored identifiers, used both for the
// first response and for an idempotent retry.
func (e *Enqueue) replay(ctx context.Context, tx pgx.Tx, userID string, stored storedResult) (enqueue.Result, error) {
	if stored.Kind == enqueue.AlreadySaved {
		record, err := loadSavedReel(ctx, tx, userID, stored.SaveID)
		if err != nil {
			return enqueue.Result{}, err
		}
		return enqueue.Result{Kind: enqueue.AlreadySaved, Reel: &record}, nil
	}

	job, err := loadJob(ctx, tx, userID, stored.JobID)
	if err != nil {
		return enqueue.Result{}, err
	}
	return enqueue.Result{Kind: enqueue.Accepted, Job: &job}, nil
}

// requireFilableCollections refuses the whole submission when any named id is
// not a collection this user may add to. A collection that does not exist and
// one that belongs to a stranger answer identically, so a submission cannot be
// used to probe for other people's collections.
func requireFilableCollections(ctx context.Context, tx pgx.Tx, userID string, collectionIDs []string) error {
	if len(collectionIDs) == 0 {
		return nil
	}
	var unreachable int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM unnest($1::uuid[]) AS wanted(id)
		WHERE NOT EXISTS (
			SELECT 1 FROM reelpin.collections c
			WHERE c.id = wanted.id AND (c.owner_id = $2::uuid OR EXISTS (
				SELECT 1 FROM reelpin.collection_members m
				WHERE m.collection_id = c.id AND m.user_id = $2::uuid AND m.role = 'editor')))`,
		collectionIDs, userID,
	).Scan(&unreachable); err != nil {
		return fmt.Errorf("checking the named collections: %w", err)
	}
	if unreachable > 0 {
		return enqueue.ErrCollectionUnreachable
	}
	return nil
}

// fileSubmission files a save into the submission's collections immediately,
// which is only right on the 200 path: there the save already exists. The 202
// path has no save yet, so its ids ride the job until the run completes.
func fileSubmission(ctx context.Context, tx pgx.Tx, submission enqueue.Submission, saveID string) error {
	for _, collectionID := range submission.CollectionIDs {
		if _, err := fileSaves(ctx, tx, submission.UserID, collectionID, []string{saveID}); err != nil {
			return err
		}
	}
	return nil
}

func findOrCreateContent(ctx context.Context, tx pgx.Tx, submission enqueue.Submission) (string, error) {
	identity := submission.Identity
	normalizedHash := sourceidentity.URLHash(identity.NormalizedURL)

	var contentID string
	var err error
	if identity.ContentID != "" {
		err = tx.QueryRow(ctx, `
			SELECT id::text FROM reelpin.contents
			WHERE source_platform = $1 AND source_content_type = $2
			  AND source_content_id = $3 AND access_scope_hash = $4`,
			identity.Platform, identity.ContentType, identity.ContentID, submission.ScopeHash,
		).Scan(&contentID)
	} else {
		err = tx.QueryRow(ctx, `
			SELECT id::text FROM reelpin.contents
			WHERE normalized_url_hash = $1 AND access_scope_hash = $2 AND source_content_id IS NULL`,
			normalizedHash, submission.ScopeHash,
		).Scan(&contentID)
	}
	if err == nil {
		return contentID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("finding the content: %w", err)
	}

	var sourceID any
	if identity.ContentID != "" {
		sourceID = identity.ContentID
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO reelpin.contents
			(source_platform, source_content_type, source_content_id,
			 normalized_url, normalized_url_hash, access_scope_hash)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id::text`,
		identity.Platform, identity.ContentType, sourceID,
		identity.NormalizedURL, normalizedHash, submission.ScopeHash,
	).Scan(&contentID)
	if isUniqueViolation(err) {
		// Another submission created it between our select and insert. The
		// advisory lock is per user, so two users can race here; re-read wins.
		return findOrCreateContent(ctx, tx, submission)
	}
	if err != nil {
		return "", fmt.Errorf("creating the content: %w", err)
	}
	return contentID, nil
}

func findOrCreateRun(ctx context.Context, tx pgx.Tx, contentID string) (runID string, created bool, err error) {
	err = tx.QueryRow(ctx, `
		INSERT INTO reelpin.processing_runs (content_id, processor_version)
		VALUES ($1, $2)
		ON CONFLICT DO NOTHING
		RETURNING id::text`,
		contentID, enqueue.ProcessorVersion,
	).Scan(&runID)
	if err == nil {
		return runID, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, fmt.Errorf("creating the run: %w", err)
	}

	err = tx.QueryRow(ctx, `
		SELECT id::text FROM reelpin.processing_runs
		WHERE content_id = $1 AND processor_version = $2
		  AND status IN ('queued', 'processing', 'retry_scheduled')
		ORDER BY created_at LIMIT 1`,
		contentID, enqueue.ProcessorVersion,
	).Scan(&runID)
	if err != nil {
		return "", false, fmt.Errorf("finding the live run: %w", err)
	}
	return runID, false, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// loadSavedReel reads the canonical save + current version into the wire's
// reel shape. During production coexistence the legacy adapter serves reads;
// this is the response for content the canonical pipeline owns.
func loadSavedReel(ctx context.Context, tx pgx.Tx, userID, saveID string) (reels.ReelRecord, error) {
	var record reels.ReelRecord
	var tags, facts []string
	var savedAt time.Time
	err := tx.QueryRow(ctx, `
		SELECT s.id::text, s.user_id::text, c.normalized_url,
		       c.source_platform, c.source_content_type,
		       COALESCE(v.title, ''), COALESCE(v.summary, ''), COALESCE(v.transcript, ''),
		       COALESCE(v.tags, '{}'), COALESCE(v.key_facts, '{}'),
		       s.saved_at
		FROM reelpin.user_saves s
		JOIN reelpin.contents c ON c.id = s.content_id
		LEFT JOIN reelpin.content_versions v ON v.id = c.current_version_id
		WHERE s.id = $1 AND s.user_id = $2`,
		saveID, userID,
	).Scan(&record.ID, &record.UserID, &record.URL,
		&record.SourcePlatform, &record.SourceContentType,
		&record.Title, &record.Summary, &record.Transcript,
		&tags, &facts, &savedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return reels.ReelRecord{}, reels.ErrNotFound
	}
	if err != nil {
		return reels.ReelRecord{}, fmt.Errorf("loading the saved reel: %w", err)
	}
	record.SecondaryCategories = tags
	record.KeyFacts = facts
	record.CreatedAt = &savedAt
	return record, nil
}

func loadJob(ctx context.Context, tx pgx.Tx, userID, jobID string) (enqueue.Job, error) {
	var job enqueue.Job
	var collectionIDs []string
	var createdAt, updatedAt time.Time
	err := tx.QueryRow(ctx, `
		SELECT id::text, status, url, source_platform, current_step,
		       progress_percent, failure_code, user_save_id::text,
		       collection_ids::text[], created_at, updated_at
		FROM reelpin.processing_jobs
		WHERE id = $1 AND user_id = $2`,
		jobID, userID,
	).Scan(&job.ID, &job.Status, &job.URL, &job.SourcePlatform, &job.CurrentStep,
		&job.ProgressPercent, &job.FailureCode, &job.ResultReelID,
		&collectionIDs, &createdAt, &updatedAt)
	if err != nil {
		return enqueue.Job{}, fmt.Errorf("loading the job: %w", err)
	}
	job.CollectionIDs = collectionIDs
	if job.CollectionIDs == nil {
		job.CollectionIDs = []string{}
	}
	job.CreatedAt = &createdAt
	job.UpdatedAt = &updatedAt
	return job, nil
}
