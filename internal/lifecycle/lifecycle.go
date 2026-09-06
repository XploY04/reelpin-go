// Package lifecycle removes things: one saved reel, or a whole account.
//
// The rule that shapes all of it: a user's data is theirs to delete, and
// shared global content is not. Deleting a save never removes content another
// save still points at, and never removes an extraction other people are
// reading. Content nobody can reach any more is a different matter, and goes.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is a reel that is not this user's, which a caller cannot
// distinguish from one that never existed.
var ErrNotFound = errors.New("not found")

// ErrDeletionPending means this subject already asked to be deleted. Their
// data is going away, so nothing new should be accepted from them.
var ErrDeletionPending = errors.New("account deletion is already in progress")

// AuthDeleter removes the identity itself, in whatever system owns it. It runs
// last and outside the database transaction, because the two cannot commit
// together: that gap is the whole reason a durable request row exists.
type AuthDeleter interface {
	DeleteUser(ctx context.Context, userID string) error
}

// CacheInvalidator drops a user's cached responses once their data changes.
// Optional: a nil one simply means nothing is cached.
type CacheInvalidator interface {
	InvalidateUser(ctx context.Context, userID string) error
}

type Service struct {
	pool   *pgxpool.Pool
	auth   AuthDeleter
	cache  CacheInvalidator
	logger *slog.Logger
}

func New(pool *pgxpool.Pool, auth AuthDeleter, cache CacheInvalidator, logger *slog.Logger) *Service {
	return &Service{pool: pool, auth: auth, cache: cache, logger: logger}
}

// DeleteReel removes one user's save. The content it was made from is left
// alone unless nobody can reach it any more.
func (s *Service) DeleteReel(ctx context.Context, userID, reelID string) error {
	if _, err := uuid.Parse(reelID); err != nil {
		// A malformed id and someone else's id answer identically.
		return ErrNotFound
	}
	if pending, err := s.Pending(ctx, userID); err != nil {
		return err
	} else if pending {
		return ErrDeletionPending
	}

	transaction, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("starting the delete: %w", err)
	}
	defer transaction.Rollback(ctx)

	var contentID string
	err = transaction.QueryRow(ctx, `
		DELETE FROM reelpin.user_saves
		WHERE id = $1 AND user_id = $2
		RETURNING content_id::text`,
		reelID, userID,
	).Scan(&contentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("deleting the save: %w", err)
	}

	// Collection items and job pointers reference the save and are cleared by
	// their own foreign keys; nothing to do here.
	if _, err := deleteUnreachablePrivateContent(ctx, transaction, []string{contentID}); err != nil {
		return err
	}

	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf("committing the delete: %w", err)
	}

	s.invalidate(ctx, userID)
	return nil
}

// deleteUnreachablePrivateContent removes content that no save points at and
// that was never public, along with its versions and runs. Public content is
// kept whatever happens: another user may share the same link tomorrow, and
// reusing it is the point of the global model.
func deleteUnreachablePrivateContent(ctx context.Context, tx pgx.Tx, contentIDs []string) (int, error) {
	if len(contentIDs) == 0 {
		return 0, nil
	}

	// Runs and versions cascade from contents; jobs already lost their
	// pointers. The scope hash is the literal "public" for public identities.
	tag, err := tx.Exec(ctx, `
		DELETE FROM reelpin.contents c
		WHERE c.id = ANY($1::uuid[])
		  AND c.access_scope_hash <> 'public'
		  AND NOT EXISTS (SELECT 1 FROM reelpin.user_saves s WHERE s.content_id = c.id)`,
		contentIDs,
	)
	if err != nil {
		return 0, fmt.Errorf("removing unreachable private content: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// Report says what an account deletion removed. It is what the operator log
// and the API response are built from, so it never claims more than happened.
type Report struct {
	UserID          string `json:"user_id"`
	Saves           int    `json:"saves"`
	ProcessingJobs  int    `json:"processing_jobs"`
	IdempotencyKeys int    `json:"idempotency_keys"`
	PrivateContent  int    `json:"private_content"`
	OutboxEvents    int    `json:"outbox_events"`
	// DatabaseCleaned and IdentityDeleted are reported separately because they
	// are two commits with a gap between them, and the gap is survivable only
	// if the caller is told the truth about which half is done.
	DatabaseCleaned bool `json:"database_cleaned"`
	IdentityDeleted bool `json:"identity_deleted"`
	// Pending is true while any half is unfinished. A pending subject stays
	// blocked and the request is retried.
	Pending bool `json:"pending"`
}

// Pending reports whether this subject has an unfinished deletion request.
func (s *Service) Pending(ctx context.Context, userID string) (bool, error) {
	var pending bool
	err := s.pool.QueryRow(ctx, `
		SELECT database_cleanup_state <> 'done' OR identity_cleanup_state <> 'done'
		FROM reelpin.account_deletion_requests WHERE user_id = $1`,
		userID,
	).Scan(&pending)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking for a deletion request: %w", err)
	}
	return pending, nil
}

// DeleteAccount records the request, then works it. It is safe to call again
// after a crash at any point: the request row says what is left to do.
func (s *Service) DeleteAccount(ctx context.Context, userID string) (Report, error) {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO reelpin.account_deletion_requests (user_id)
		VALUES ($1) ON CONFLICT (user_id) DO NOTHING`,
		userID,
	); err != nil {
		return Report{}, fmt.Errorf("recording the deletion request: %w", err)
	}
	return s.ResumeAccountDeletion(ctx, userID)
}

// ResumeAccountDeletion finishes whatever the request row still says is
// outstanding. The maintenance command calls this for every pending request;
// the API calls it through DeleteAccount.
func (s *Service) ResumeAccountDeletion(ctx context.Context, userID string) (Report, error) {
	report := Report{UserID: userID}

	var databaseState, identityState string
	if err := s.pool.QueryRow(ctx, `
		SELECT database_cleanup_state, identity_cleanup_state
		FROM reelpin.account_deletion_requests WHERE user_id = $1`,
		userID,
	).Scan(&databaseState, &identityState); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Report{}, ErrNotFound
		}
		return Report{}, fmt.Errorf("reading the deletion request: %w", err)
	}

	if databaseState != "done" {
		cleaned, err := s.cleanDatabase(ctx, userID)
		if err != nil {
			s.recordFailure(ctx, userID, "database")
			return Report{}, err
		}
		report = cleaned
		report.UserID = userID
	}
	report.DatabaseCleaned = true

	// The identity is last: everything above can be retried while the account
	// still exists, and nothing above depends on the identity being gone.
	if identityState == "done" {
		report.IdentityDeleted = true
		return report, nil
	}

	if s.auth == nil {
		// No adapter configured. Saying "deleted" here would be a lie the
		// operator could not see, so the request stays pending and is retried
		// when one is wired.
		// The user id stays out of the line: the request row already has it,
		// and a log is the wrong place to keep one.
		s.logger.Error("no identity deleter configured; the account's data is gone but its sign-in still works")
		s.recordFailure(ctx, userID, "identity_deleter_missing")
		report.Pending = true
		return report, nil
	}

	if err := s.auth.DeleteUser(ctx, userID); err != nil {
		s.logger.Error("deleting the identity failed; the request stays pending", "error", err)
		s.recordFailure(ctx, userID, "identity")
		report.Pending = true
		return report, nil
	}

	if _, err := s.pool.Exec(ctx, `
		UPDATE reelpin.account_deletion_requests
		SET identity_cleanup_state = 'done', updated_at = now()
		WHERE user_id = $1`, userID,
	); err != nil {
		// The identity is gone but the record still says pending. A retry
		// calls DeleteUser again, which is why that call must tolerate an
		// already-deleted user.
		return report, fmt.Errorf("recording identity deletion: %w", err)
	}

	report.IdentityDeleted = true
	s.invalidate(ctx, userID)
	return report, nil
}

// cleanDatabase removes every row this user owns, in one transaction, and
// marks the database half done in the same commit. A crash before the commit
// leaves the request pending and the work repeatable.
func (s *Service) cleanDatabase(ctx context.Context, userID string) (Report, error) {
	transaction, err := s.pool.Begin(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("starting the account delete: %w", err)
	}
	defer transaction.Rollback(ctx)

	if _, err := transaction.Exec(ctx, `
		UPDATE reelpin.account_deletion_requests
		SET database_cleanup_state = 'running', attempts = attempts + 1, updated_at = now()
		WHERE user_id = $1`, userID,
	); err != nil {
		return Report{}, fmt.Errorf("marking the cleanup running: %w", err)
	}

	// The content this user saved, remembered before the saves go, so the
	// unreachable ones can be found afterwards.
	rows, err := transaction.Query(ctx,
		`SELECT content_id::text FROM reelpin.user_saves WHERE user_id = $1`, userID)
	if err != nil {
		return Report{}, fmt.Errorf("reading the user's content: %w", err)
	}
	contentIDs := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return Report{}, fmt.Errorf("reading a content id: %w", err)
		}
		contentIDs = append(contentIDs, id)
	}
	rows.Close()
	if rows.Err() != nil {
		return Report{}, fmt.Errorf("reading the user's content: %w", rows.Err())
	}

	report := Report{UserID: userID}

	// Events describing this user's runs, before the runs go. An event that
	// survived would name a run that no longer exists.
	if report.OutboxEvents, err = execCount(ctx, transaction, `
		DELETE FROM reelpin.outbox_events e
		WHERE (e.payload->>'run_id')::uuid IN (
			SELECT run_id FROM reelpin.processing_jobs
			WHERE user_id = $1 AND run_id IS NOT NULL
		)
		AND NOT EXISTS (
			SELECT 1 FROM reelpin.processing_jobs other
			WHERE other.run_id = (e.payload->>'run_id')::uuid AND other.user_id <> $1
		)`, userID); err != nil {
		return Report{}, err
	}

	for _, step := range []struct {
		name      string
		statement string
		target    *int
	}{
		{"processing jobs", `DELETE FROM reelpin.processing_jobs WHERE user_id = $1`, &report.ProcessingJobs},
		{"idempotency keys", `DELETE FROM reelpin.idempotency_keys WHERE user_id = $1`, &report.IdempotencyKeys},
		{"saves", `DELETE FROM reelpin.user_saves WHERE user_id = $1`, &report.Saves},
	} {
		count, err := execCount(ctx, transaction, step.statement, userID)
		if err != nil {
			return Report{}, fmt.Errorf("deleting %s: %w", step.name, err)
		}
		*step.target = count
	}

	// Collections, device tokens, notifications and profiles are deleted by
	// their own foreign keys to auth.users when the identity goes, and by
	// their own branches' cleanup where they need more than a cascade.

	if report.PrivateContent, err = deleteUnreachablePrivateContent(ctx, transaction, contentIDs); err != nil {
		return Report{}, err
	}

	// Runs nobody subscribes to any more carry only derived data.
	if _, err := transaction.Exec(ctx, `
		DELETE FROM reelpin.processing_runs r
		WHERE NOT EXISTS (SELECT 1 FROM reelpin.processing_jobs j WHERE j.run_id = r.id)
		  AND NOT EXISTS (SELECT 1 FROM reelpin.user_saves s WHERE s.content_id = r.content_id)`,
	); err != nil {
		return Report{}, fmt.Errorf("removing unsubscribed runs: %w", err)
	}

	if _, err := transaction.Exec(ctx, `
		UPDATE reelpin.account_deletion_requests
		SET database_cleanup_state = 'done', updated_at = now()
		WHERE user_id = $1`, userID,
	); err != nil {
		return Report{}, fmt.Errorf("marking the cleanup done: %w", err)
	}

	if err := transaction.Commit(ctx); err != nil {
		return Report{}, fmt.Errorf("committing the account delete: %w", err)
	}

	s.invalidate(ctx, userID)
	return report, nil
}

func execCount(ctx context.Context, tx pgx.Tx, statement string, args ...any) (int, error) {
	tag, err := tx.Exec(ctx, statement, args...)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// recordFailure counts the attempt so a request that never succeeds is
// visible, rather than being retried silently forever.
func (s *Service) recordFailure(ctx context.Context, userID, class string) {
	if _, err := s.pool.Exec(ctx, `
		UPDATE reelpin.account_deletion_requests
		SET attempts = attempts + 1, last_error_class = $2, updated_at = now()
		WHERE user_id = $1`, userID, class,
	); err != nil {
		s.logger.Error("recording a deletion failure failed", "error", err)
	}
}

// PendingRequests lists every unfinished deletion, oldest first, for the
// maintenance command that retries them.
func (s *Service) PendingRequests(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT user_id::text FROM reelpin.account_deletion_requests
		WHERE database_cleanup_state <> 'done' OR identity_cleanup_state <> 'done'
		ORDER BY requested_at
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("listing pending deletions: %w", err)
	}
	defer rows.Close()

	users := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("reading a pending deletion: %w", err)
		}
		users = append(users, id)
	}
	return users, rows.Err()
}

func (s *Service) invalidate(ctx context.Context, userID string) {
	if s.cache == nil {
		return
	}
	if err := s.cache.InvalidateUser(ctx, userID); err != nil {
		// A stale cache entry is a display problem, not a deletion failure.
		s.logger.Warn("invalidating the user's cache failed", "error", err)
	}
}
