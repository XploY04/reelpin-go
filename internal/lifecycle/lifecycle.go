// Package lifecycle removes things: one saved reel, or a whole account.
//
// The rule that shapes it: a user's data is theirs to delete, and shared global
// content is not. Deleting a save never removes content another save still
// points at, and never removes the extraction two other people are reading.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/XploY04/reelpin-go/internal/outbox"
	"github.com/XploY04/reelpin-go/internal/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is a reel that is not this user's, which is indistinguishable
// from one that does not exist.
var ErrNotFound = errors.New("not found")

// AuthDeleter removes the identity itself. It is last, because everything else
// can be retried while the account still exists.
type AuthDeleter interface {
	DeleteUser(ctx context.Context, userID string) error
}

// CacheInvalidator drops a user's cached responses after their data changes.
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

// DeleteReel removes one user's save and everything that pointed at it. The
// global content it was made from is left alone: other people may still be
// reading it.
func (s *Service) DeleteReel(ctx context.Context, userID, reelID string) error {
	if _, err := uuid.Parse(reelID); err != nil {
		return ErrNotFound
	}

	transaction, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("starting the delete: %w", err)
	}
	defer transaction.Rollback(ctx)

	var contentVersionID *string
	err = transaction.QueryRow(ctx,
		`SELECT content_version_id::text FROM public.reels WHERE id = $1 AND user_id = $2 FOR UPDATE`,
		reelID, userID,
	).Scan(&contentVersionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("reading the reel: %w", err)
	}

	// Collection items and map pins reference the save, not the content.
	for _, statement := range []string{
		`DELETE FROM public.collection_items WHERE reel_id = $1`,
		`DELETE FROM public.hidden_reel_map_pins WHERE reel_id = $1`,
		`UPDATE public.collections SET cover_reel_id = NULL WHERE cover_reel_id = $1`,
		`UPDATE public.processing_jobs SET result_reel_id = NULL WHERE result_reel_id = $1`,
	} {
		if _, err := transaction.Exec(ctx, statement, reelID); err != nil {
			return fmt.Errorf("clearing references to the reel: %w", err)
		}
	}

	if _, err := transaction.Exec(ctx,
		`DELETE FROM public.reels WHERE id = $1 AND user_id = $2`, reelID, userID,
	); err != nil {
		return fmt.Errorf("deleting the reel: %w", err)
	}

	// Search indexes and stored thumbnails live outside this database, so they
	// are cleaned by an event rather than in this transaction.
	if err := outbox.Insert(ctx, transaction, outbox.Event{
		EventID:    newEventID(),
		EventType:  "reel.deleted",
		RoutingKey: "reelpin.notifications",
		Payload: map[string]any{
			"run_id":             reelID,
			"platform":           "lifecycle",
			"reel_id":            reelID,
			"user_id":            userID,
			"content_version_id": contentVersionID,
		},
	}); err != nil {
		return err
	}

	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf("committing the delete: %w", err)
	}

	s.invalidate(ctx, userID)
	return nil
}

// DeleteAccountReport says what was removed, for the operator log and for the
// caller to see the work was real.
type DeleteAccountReport struct {
	Reels           int  `json:"reels"`
	ProcessingJobs  int  `json:"processing_jobs"`
	Collections     int  `json:"collections"`
	Memberships     int  `json:"memberships"`
	MapPins         int  `json:"map_pins"`
	DeviceTokens    int  `json:"device_tokens"`
	ShareTokens     int  `json:"share_tokens"`
	Notifications   int  `json:"notifications"`
	AuthUserDeleted bool `json:"auth_user_deleted"`
}

// DeleteAccount removes everything one account owns, in an order that is safe
// to repeat. Relational data goes first in one transaction, cleanup events are
// committed with it, and the identity is deleted last: while it still exists,
// the whole thing can be run again.
func (s *Service) DeleteAccount(ctx context.Context, userID string) (DeleteAccountReport, error) {
	report := DeleteAccountReport{}

	transaction, err := s.pool.Begin(ctx)
	if err != nil {
		return report, fmt.Errorf("starting the account delete: %w", err)
	}
	defer transaction.Rollback(ctx)

	// Collections this user owns go entirely; memberships elsewhere are just
	// removed, because those collections belong to other people.
	steps := []struct {
		target    *int
		statement string
	}{
		{&report.Memberships, `DELETE FROM public.collection_members WHERE user_id = $1`},
		{nil, `DELETE FROM public.collection_items WHERE added_by = $1`},
		{nil, `DELETE FROM public.collection_invites WHERE created_by = $1`},
		{&report.Collections, `DELETE FROM public.collections WHERE owner_id = $1`},
		{&report.MapPins, `DELETE FROM public.manual_map_pins WHERE user_id = $1`},
		{nil, `DELETE FROM public.hidden_reel_map_pins WHERE user_id = $1`},
		{&report.Reels, `DELETE FROM public.reels WHERE user_id = $1`},
		{&report.ProcessingJobs, `DELETE FROM public.processing_jobs WHERE user_id = $1`},
		{&report.DeviceTokens, `DELETE FROM public.device_push_tokens WHERE user_id = $1`},
		{&report.ShareTokens, `DELETE FROM public.device_share_tokens WHERE user_id = $1`},
		{&report.Notifications, `DELETE FROM public.notifications WHERE user_id = $1`},
		{nil, `DELETE FROM reelpin.campaign_targets WHERE user_id = $1`},
	}

	for _, step := range steps {
		tag, err := transaction.Exec(ctx, step.statement, userID)
		if err != nil {
			// A table that does not exist in this deployment is not a failure:
			// the goal is that nothing of theirs is left.
			if isMissingTable(err) {
				continue
			}
			return report, fmt.Errorf("deleting account data: %w", err)
		}
		if step.target != nil {
			*step.target = int(tag.RowsAffected())
		}
	}

	// Profiles are app-owned and must go with the account.
	if _, err := transaction.Exec(ctx, `DELETE FROM public.profiles WHERE id::text = $1`, userID); err != nil &&
		!isMissingTable(err) {
		return report, fmt.Errorf("deleting the profile: %w", err)
	}

	// Storage objects and search vectors live elsewhere; the event is committed
	// with the deletions so the cleanup cannot be lost.
	if err := outbox.Insert(ctx, transaction, outbox.Event{
		EventID:    newEventID(),
		EventType:  "account.deleted",
		RoutingKey: "reelpin.notifications",
		Payload:    map[string]any{"run_id": userID, "platform": "lifecycle", "user_id": userID},
	}); err != nil {
		return report, err
	}

	if err := transaction.Commit(ctx); err != nil {
		return report, fmt.Errorf("committing the account delete: %w", err)
	}

	s.invalidate(ctx, userID)

	// Last, and only if everything above committed. If this fails the caller
	// sees a failure and can run the whole thing again safely.
	if s.auth != nil {
		if err := s.auth.DeleteUser(ctx, userID); err != nil {
			return report, fmt.Errorf("deleting the auth user: %w", err)
		}
		report.AuthUserDeleted = true
	}
	return report, nil
}

func (s *Service) invalidate(ctx context.Context, userID string) {
	if s.cache == nil {
		return
	}
	if err := s.cache.InvalidateUser(ctx, userID); err != nil {
		// A stale cache entry is a cosmetic problem for at most one TTL.
		s.logger.Warn("invalidating cached responses failed", "error", err)
	}
}

func isMissingTable(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "42P01"
	}
	return false
}

func newEventID() string {
	return uuid.NewString()
}
