package collections

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/postgres"
	"github.com/XploY04/reelpin-go/internal/reels"
	"github.com/jackc/pgx/v5"
)

// EnableLink mints a view-link token. Only its hash is stored, so the link
// itself exists once, in the response.
func (s *Service) EnableLink(ctx context.Context, collectionID, userID string) (string, string, error) {
	role, err := s.Role(ctx, collectionID, userID)
	if err != nil {
		return "", "", err
	}
	if role != RoleOwner {
		return "", "", ErrForbidden
	}

	token, err := newToken()
	if err != nil {
		return "", "", fmt.Errorf("minting a link token: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE public.collections
		SET visibility = 'link', link_token_hash = $2, updated_at = now()
		WHERE id = $1`, collectionID, hashToken(token),
	); err != nil {
		return "", "", fmt.Errorf("enabling the link: %w", err)
	}
	return s.shareBase + "/c/" + token, token, nil
}

// DisableLink revokes the capability by forgetting its hash.
func (s *Service) DisableLink(ctx context.Context, collectionID, userID string) error {
	role, err := s.Role(ctx, collectionID, userID)
	if err != nil {
		return err
	}
	if role != RoleOwner {
		return ErrForbidden
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE public.collections
		SET visibility = 'private', link_token_hash = NULL, updated_at = now()
		WHERE id = $1`, collectionID,
	); err != nil {
		return fmt.Errorf("disabling the link: %w", err)
	}
	return nil
}

// SharedByToken serves an anonymous reader. The token is the whole capability,
// so the response deliberately carries no owner id and no member count.
func (s *Service) SharedByToken(ctx context.Context, token string, offset, limit int, now time.Time) (Shared, error) {
	var collectionID string
	err := s.pool.QueryRow(ctx, `
		SELECT id::text FROM public.collections
		WHERE link_token_hash = $1 AND visibility = 'link'`, hashToken(token),
	).Scan(&collectionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Shared{}, ErrNotFound
	}
	if err != nil {
		return Shared{}, fmt.Errorf("reading the shared collection: %w", err)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT `+collectionColumns+`, 'viewer'::text,
		       (SELECT count(*) FROM public.collection_items i WHERE i.collection_id = c.id), 0
		FROM public.collections c WHERE c.id = $1`, collectionID)
	if err != nil {
		return Shared{}, fmt.Errorf("reading the shared collection: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return Shared{}, ErrNotFound
	}
	collection, err := scanCollection(rows)
	if err != nil {
		return Shared{}, err
	}
	rows.Close()

	items, pagination, err := s.items(ctx, collectionID, offset, limit, collection.ItemCount, now)
	if err != nil {
		return Shared{}, err
	}

	// An anonymous reader learns what is in the collection, not who is in it.
	collection.OwnerID = ""
	collection.MemberCount = 0
	ownerName, err := s.ownerName(ctx, collectionID)
	if err != nil {
		return Shared{}, err
	}

	return Shared{Collection: collection, Reels: items, Pagination: pagination, OwnerName: ownerName}, nil
}

// Detail is the signed-in view: the same reels, plus who owns it and whether
// this user may change it.
func (s *Service) Detail(ctx context.Context, collectionID, userID string, offset, limit int, now time.Time) (Detail, error) {
	collection, err := s.Get(ctx, collectionID, userID)
	if err != nil {
		return Detail{}, err
	}

	items, pagination, err := s.items(ctx, collectionID, offset, limit, collection.ItemCount, now)
	if err != nil {
		return Detail{}, err
	}
	ownerName, err := s.ownerName(ctx, collectionID)
	if err != nil {
		return Detail{}, err
	}

	return Detail{
		Collection: collection,
		Reels:      items,
		Pagination: pagination,
		CanEdit:    CanEdit(collection.Role),
		OwnerName:  ownerName,
	}, nil
}

// items loads a page of reels in one bulk query rather than one per item.
func (s *Service) items(ctx context.Context, collectionID string, offset, limit, total int, now time.Time) ([]reels.DisplayReel, Pagination, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+postgres.ReelListColumns+`
		FROM public.collection_items i
		JOIN public.reels r ON r.id = i.reel_id
		WHERE i.collection_id = $1
		ORDER BY i.created_at DESC
		LIMIT $2 OFFSET $3`, collectionID, limit, offset)
	if err != nil {
		return nil, Pagination{}, fmt.Errorf("reading collection items: %w", err)
	}
	defer rows.Close()

	display := []reels.DisplayReel{}
	for rows.Next() {
		record, err := postgres.ScanReelList(rows)
		if err != nil {
			return nil, Pagination{}, err
		}
		display = append(display, reels.BuildDisplayReel(record, now))
	}
	if err := rows.Err(); err != nil {
		return nil, Pagination{}, fmt.Errorf("reading collection items: %w", err)
	}

	pagination := Pagination{TotalCount: total, Limit: limit, Offset: offset}
	nextOffset := offset + len(display)
	if nextOffset < total {
		cursor := fmt.Sprint(nextOffset)
		pagination.HasMore = true
		pagination.NextOffset = &nextOffset
		pagination.NextCursor = &cursor
	}
	return display, pagination, nil
}

// ownerName is looked up once per response, not once per member.
func (s *Service) ownerName(ctx context.Context, collectionID string) (*string, error) {
	var name *string
	err := s.pool.QueryRow(ctx, `
		SELECT p.display_name
		FROM public.collections c
		LEFT JOIN public.profiles p ON p.id::text = c.owner_id
		WHERE c.id = $1`, collectionID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		// Profiles are optional; a missing name is not a failure.
		return nil, nil
	}
	if name != nil && strings.TrimSpace(*name) == "" {
		return nil, nil
	}
	return name, nil
}

// Members lists collaborators with their names resolved in one query.
func (s *Service) Members(ctx context.Context, collectionID, userID string) (string, []Member, error) {
	role, err := s.Role(ctx, collectionID, userID)
	if err != nil {
		return "", nil, err
	}
	_ = role

	var ownerID string
	if err := s.pool.QueryRow(ctx,
		`SELECT owner_id FROM public.collections WHERE id = $1`, collectionID).Scan(&ownerID); err != nil {
		return "", nil, ErrNotFound
	}

	rows, err := s.pool.Query(ctx, `
		SELECT m.user_id, m.role, m.created_at, p.display_name
		FROM public.collection_members m
		LEFT JOIN public.profiles p ON p.id::text = m.user_id
		WHERE m.collection_id = $1
		ORDER BY m.created_at`, collectionID)
	if err != nil {
		return "", nil, fmt.Errorf("reading members: %w", err)
	}
	defer rows.Close()

	members := []Member{}
	for rows.Next() {
		var member Member
		var createdAt *time.Time
		if err := rows.Scan(&member.UserID, &member.Role, &createdAt, &member.DisplayName); err != nil {
			return "", nil, fmt.Errorf("reading members: %w", err)
		}
		member.CreatedAt = isoTime(createdAt)
		members = append(members, member)
	}
	return ownerID, members, rows.Err()
}

func (s *Service) RemoveMember(ctx context.Context, collectionID, userID, memberUserID string) error {
	role, err := s.Role(ctx, collectionID, userID)
	if err != nil {
		return err
	}
	if role != RoleOwner {
		return ErrForbidden
	}
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM public.collection_members WHERE collection_id = $1 AND user_id = $2`,
		collectionID, memberUserID,
	); err != nil {
		return fmt.Errorf("removing the member: %w", err)
	}
	return nil
}

// Leave is a member removing themselves. An owner cannot leave their own
// collection: they delete it or hand it over.
func (s *Service) Leave(ctx context.Context, collectionID, userID string) error {
	role, err := s.Role(ctx, collectionID, userID)
	if err != nil {
		return err
	}
	if role == RoleOwner {
		return ErrForbidden
	}
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM public.collection_members WHERE collection_id = $1 AND user_id = $2`,
		collectionID, userID,
	); err != nil {
		return fmt.Errorf("leaving the collection: %w", err)
	}
	return nil
}

// CreateInvite mints a single capability with an expiry. Only the hash is kept.
func (s *Service) CreateInvite(ctx context.Context, collectionID, userID, role string) (string, string, string, time.Time, error) {
	actorRole, err := s.Role(ctx, collectionID, userID)
	if err != nil {
		return "", "", "", time.Time{}, err
	}
	if actorRole != RoleOwner {
		return "", "", "", time.Time{}, ErrForbidden
	}
	if role != RoleEditor {
		role = RoleViewer
	}

	token, err := newToken()
	if err != nil {
		return "", "", "", time.Time{}, fmt.Errorf("minting an invite: %w", err)
	}
	expiresAt := s.now().UTC().Add(InviteExpiry)

	if _, err := s.pool.Exec(ctx, `
		INSERT INTO public.collection_invites (collection_id, token_hash, role, created_by, expires_at)
		VALUES ($1, $2, $3, $4, $5)`,
		collectionID, hashToken(token), role, userID, expiresAt,
	); err != nil {
		return "", "", "", time.Time{}, fmt.Errorf("creating the invite: %w", err)
	}
	return s.shareBase + "/c/invite/" + token, token, role, expiresAt, nil
}

// AcceptInvite turns a capability into membership. It is idempotent: accepting
// twice leaves one membership and does not spend a second use.
func (s *Service) AcceptInvite(ctx context.Context, token, userID string) (Collection, error) {
	transaction, err := s.pool.Begin(ctx)
	if err != nil {
		return Collection{}, fmt.Errorf("starting the accept: %w", err)
	}
	defer transaction.Rollback(ctx)

	var (
		inviteID     string
		collectionID string
		role         string
		expiresAt    *time.Time
		maxUses      *int
		uses         int
		revoked      bool
		ownerID      string
	)
	err = transaction.QueryRow(ctx, `
		SELECT i.id::text, i.collection_id::text, i.role, i.expires_at, i.max_uses, i.uses, i.revoked, c.owner_id
		FROM public.collection_invites i
		JOIN public.collections c ON c.id = i.collection_id
		WHERE i.token_hash = $1
		FOR UPDATE OF i`, hashToken(token),
	).Scan(&inviteID, &collectionID, &role, &expiresAt, &maxUses, &uses, &revoked, &ownerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Collection{}, ErrInviteInvalid
	}
	if err != nil {
		return Collection{}, fmt.Errorf("reading the invite: %w", err)
	}

	now := s.now().UTC()
	switch {
	case revoked:
		return Collection{}, ErrInviteInvalid
	case expiresAt != nil && now.After(*expiresAt):
		return Collection{}, ErrInviteInvalid
	case maxUses != nil && uses >= *maxUses:
		return Collection{}, ErrInviteInvalid
	}

	// The owner accepting their own invite is a no-op, not a membership.
	if ownerID != userID {
		tag, err := transaction.Exec(ctx, `
			INSERT INTO public.collection_members (collection_id, user_id, role, invited_by)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (collection_id, user_id) DO NOTHING`,
			collectionID, userID, role, ownerID)
		if err != nil {
			return Collection{}, fmt.Errorf("adding the member: %w", err)
		}
		if tag.RowsAffected() > 0 {
			if _, err := transaction.Exec(ctx,
				`UPDATE public.collection_invites SET uses = uses + 1 WHERE id = $1`, inviteID,
			); err != nil {
				return Collection{}, fmt.Errorf("recording the invite use: %w", err)
			}
		}
	}

	if err := transaction.Commit(ctx); err != nil {
		return Collection{}, fmt.Errorf("committing the accept: %w", err)
	}
	return s.Get(ctx, collectionID, userID)
}

// FileReel adds a finished reel to the collections its share named. It runs
// after the save, so it can never undo one: a target that disappeared, or that
// the user may no longer edit, is skipped.
func (s *Service) FileReel(ctx context.Context, userID, reelID string, collectionIDs []string) ([]string, error) {
	filed := []string{}
	for _, collectionID := range collectionIDs {
		role, err := s.Role(ctx, collectionID, userID)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return filed, err
		}
		if !CanEdit(role) {
			continue
		}

		transaction, err := s.pool.Begin(ctx)
		if err != nil {
			return filed, fmt.Errorf("starting the filing: %w", err)
		}
		added, err := addItems(ctx, transaction, collectionID, userID, []string{reelID})
		if err != nil {
			transaction.Rollback(ctx)
			return filed, err
		}
		if added > 0 {
			if err := emitCollectionEvent(ctx, transaction, collectionID, userID, added); err != nil {
				transaction.Rollback(ctx)
				return filed, err
			}
			filed = append(filed, collectionID)
		}
		if err := transaction.Commit(ctx); err != nil {
			return filed, fmt.Errorf("committing the filing: %w", err)
		}
	}
	return filed, nil
}
