package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/collections"
	"github.com/XploY04/reelpin-go/internal/reels"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const collectionColumns = `c.id::text, c.owner_id::text, c.name, c.description, c.cover_save_id::text,
	c.visibility, c.created_at, c.updated_at`

// Collections is the pgx implementation of every collection operation. All of
// it is user-scoped: access is decided once, in Role, and a stranger gets the
// same not-found a missing collection gets.
type Collections struct {
	pool      *pgxpool.Pool
	shareBase string
	now       func() time.Time
}

func NewCollections(pool *pgxpool.Pool, shareBaseURL string, now func() time.Time) *Collections {
	if now == nil {
		now = time.Now
	}
	return &Collections{pool: pool, shareBase: strings.TrimSuffix(shareBaseURL, "/"), now: now}
}

// Role answers what a user may do. A user with no relationship gets
// ErrNotFound rather than a 403: a stranger must not learn that a collection
// exists.
func (c *Collections) Role(ctx context.Context, collectionID, userID string) (string, error) {
	var ownerID string
	var memberRole *string
	err := c.pool.QueryRow(ctx, `
		SELECT co.owner_id::text, m.role
		FROM reelpin.collections co
		LEFT JOIN reelpin.collection_members m ON m.collection_id = co.id AND m.user_id::text = $2
		WHERE co.id = $1`, collectionID, userID,
	).Scan(&ownerID, &memberRole)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", collections.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("reading collection access: %w", err)
	}

	switch {
	case ownerID == userID:
		return collections.RoleOwner, nil
	case memberRole != nil && *memberRole != "":
		return *memberRole, nil
	default:
		return "", collections.ErrNotFound
	}
}

// List returns everything a user owns or belongs to, newest first. The list is
// bounded rather than paged: a person curates tens of collections, not
// thousands, and a bound keeps one query per screen.
func (c *Collections) List(ctx context.Context, userID string) ([]collections.Collection, error) {
	rows, err := c.pool.Query(ctx, `
		SELECT `+collectionColumns+`,
		       CASE WHEN c.owner_id::text = $1 THEN 'owner' ELSE COALESCE(m.role, 'viewer') END AS role,
		       (SELECT count(*) FROM reelpin.collection_items i WHERE i.collection_id = c.id) AS item_count,
		       (SELECT count(*) FROM reelpin.collection_members mm WHERE mm.collection_id = c.id) AS member_count
		FROM reelpin.collections c
		LEFT JOIN reelpin.collection_members m ON m.collection_id = c.id AND m.user_id::text = $1
		WHERE c.owner_id::text = $1 OR m.user_id::text = $1
		ORDER BY c.updated_at DESC
		LIMIT 200`, userID)
	if err != nil {
		return nil, fmt.Errorf("listing collections: %w", err)
	}
	defer rows.Close()

	list := []collections.Collection{}
	for rows.Next() {
		collection, err := scanCollection(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, collection)
	}
	return list, rows.Err()
}

func (c *Collections) Create(ctx context.Context, userID, name, description string, saveIDs []string) (collections.Collection, int, error) {
	transaction, err := c.pool.Begin(ctx)
	if err != nil {
		return collections.Collection{}, 0, fmt.Errorf("starting the create: %w", err)
	}
	defer transaction.Rollback(ctx)

	var collectionID string
	if err := transaction.QueryRow(ctx, `
		INSERT INTO reelpin.collections (owner_id, name, description)
		VALUES ($1, $2, $3) RETURNING id::text`,
		userID, strings.TrimSpace(name), strings.TrimSpace(description),
	).Scan(&collectionID); err != nil {
		return collections.Collection{}, 0, fmt.Errorf("creating the collection: %w", err)
	}

	added, err := c.FileSave(ctx, transaction, userID, collectionID, saveIDs)
	if err != nil {
		return collections.Collection{}, 0, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return collections.Collection{}, 0, fmt.Errorf("committing the create: %w", err)
	}

	collection, err := c.Get(ctx, collectionID, userID)
	return collection, added, err
}

func (c *Collections) Get(ctx context.Context, collectionID, userID string) (collections.Collection, error) {
	role, err := c.Role(ctx, collectionID, userID)
	if err != nil {
		return collections.Collection{}, err
	}
	return c.get(ctx, collectionID, role)
}

func (c *Collections) get(ctx context.Context, collectionID, role string) (collections.Collection, error) {
	rows, err := c.pool.Query(ctx, `
		SELECT `+collectionColumns+`, $2::text AS role,
		       (SELECT count(*) FROM reelpin.collection_items i WHERE i.collection_id = c.id),
		       (SELECT count(*) FROM reelpin.collection_members m WHERE m.collection_id = c.id)
		FROM reelpin.collections c WHERE c.id = $1`, collectionID, role)
	if err != nil {
		return collections.Collection{}, fmt.Errorf("reading the collection: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return collections.Collection{}, collections.ErrNotFound
	}
	return scanCollection(rows)
}

func (c *Collections) Update(ctx context.Context, collectionID, userID string, name, description, coverReelID *string) (collections.Collection, error) {
	role, err := c.Role(ctx, collectionID, userID)
	if err != nil {
		return collections.Collection{}, err
	}
	if role != collections.RoleOwner {
		return collections.Collection{}, collections.ErrForbidden
	}

	// The cover must be a save this collection could actually show; pointing
	// it at an arbitrary save id would leak another user's reel onto a cover.
	if _, err := c.pool.Exec(ctx, `
		UPDATE reelpin.collections
		SET name = COALESCE(NULLIF(TRIM($2), ''), name),
		    description = COALESCE($3, description),
		    cover_save_id = COALESCE(
		        (SELECT i.save_id FROM reelpin.collection_items i
		         WHERE i.collection_id = $1 AND i.save_id = $4::uuid),
		        cover_save_id),
		    updated_at = now()
		WHERE id = $1`,
		collectionID, optionalText(name), optionalText(description), optionalText(coverReelID),
	); err != nil {
		return collections.Collection{}, fmt.Errorf("updating the collection: %w", err)
	}
	return c.Get(ctx, collectionID, userID)
}

func (c *Collections) Delete(ctx context.Context, collectionID, userID string) error {
	role, err := c.Role(ctx, collectionID, userID)
	if err != nil {
		return err
	}
	if role != collections.RoleOwner {
		return collections.ErrForbidden
	}
	// Items, members and invites cascade; the saves themselves are untouched.
	if _, err := c.pool.Exec(ctx, `DELETE FROM reelpin.collections WHERE id = $1`, collectionID); err != nil {
		return fmt.Errorf("deleting the collection: %w", err)
	}
	return nil
}

// AddItems files saves into a collection after checking the caller may edit
// it. Adding the same save twice is not an error and does not duplicate a row.
func (c *Collections) AddItems(ctx context.Context, collectionID, userID string, saveIDs []string) (int, error) {
	role, err := c.Role(ctx, collectionID, userID)
	if err != nil {
		return 0, err
	}
	if !collections.CanEdit(role) {
		return 0, collections.ErrForbidden
	}

	transaction, err := c.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("starting the add: %w", err)
	}
	defer transaction.Rollback(ctx)

	added, err := c.FileSave(ctx, transaction, userID, collectionID, saveIDs)
	if err != nil {
		return 0, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return 0, fmt.Errorf("committing the add: %w", err)
	}
	return added, nil
}

// FileSave inserts items inside the caller's transaction. It exists for the
// enqueue task, which files a new save into its named collections in the same
// transaction as the save itself; AddItems and Create route through it too, so
// there is exactly one filing rule. It is idempotent under duplicate delivery:
// the primary key absorbs re-inserts, and only saves that exist are filed, so
// a stale picker cannot fail a batch.
func (c *Collections) FileSave(ctx context.Context, tx pgx.Tx, userID, collectionID string, saveIDs []string) (int, error) {
	return fileSaves(ctx, tx, userID, collectionID, saveIDs)
}

// fileSaves is FileSave without a receiver, so the submission transaction in
// enqueue.go files by the same rule without owning a *Collections.
func fileSaves(ctx context.Context, tx pgx.Tx, userID, collectionID string, saveIDs []string) (int, error) {
	cleaned := make([]string, 0, len(saveIDs))
	seen := map[string]bool{}
	for _, raw := range saveIDs {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] {
			continue
		}
		if _, err := uuid.Parse(id); err != nil {
			continue
		}
		seen[id] = true
		cleaned = append(cleaned, id)
		if len(cleaned) >= collections.MaxSaveIDsPerRequest {
			break
		}
	}
	if len(cleaned) == 0 {
		return 0, nil
	}

	tag, err := tx.Exec(ctx, `
		INSERT INTO reelpin.collection_items (collection_id, save_id, added_by)
		SELECT $1, s.id, $2 FROM reelpin.user_saves s WHERE s.id = ANY($3::uuid[])
		ON CONFLICT (collection_id, save_id) DO NOTHING`,
		collectionID, userID, cleaned)
	if err != nil {
		return 0, fmt.Errorf("adding collection items: %w", err)
	}

	if tag.RowsAffected() > 0 {
		if _, err := tx.Exec(ctx,
			`UPDATE reelpin.collections SET updated_at = now() WHERE id = $1`, collectionID); err != nil {
			return 0, fmt.Errorf("touching the collection: %w", err)
		}
	}
	return int(tag.RowsAffected()), nil
}

func (c *Collections) RemoveItem(ctx context.Context, collectionID, userID, saveID string) error {
	role, err := c.Role(ctx, collectionID, userID)
	if err != nil {
		return err
	}
	if !collections.CanEdit(role) {
		return collections.ErrForbidden
	}
	if _, err := c.pool.Exec(ctx,
		`DELETE FROM reelpin.collection_items WHERE collection_id = $1 AND save_id = $2`,
		collectionID, saveID,
	); err != nil {
		return fmt.Errorf("removing the item: %w", err)
	}
	return nil
}

// Detail is the signed-in view: the collection, one cursor page of items, and
// whether this user may change it.
func (c *Collections) Detail(ctx context.Context, collectionID, userID string, after *reels.Cursor, limit int) (collections.Detail, error) {
	collection, err := c.Get(ctx, collectionID, userID)
	if err != nil {
		return collections.Detail{}, err
	}

	items, page, err := c.items(ctx, collectionID, after, limit)
	if err != nil {
		return collections.Detail{}, err
	}
	return collections.Detail{
		Collection: collection,
		Items:      items,
		Page:       page,
		CanEdit:    collections.CanEdit(collection.Role),
	}, nil
}

// SharedByToken serves an anonymous reader. The token is the whole capability;
// an unknown, expired or revoked one answers the same not-found, so a token
// guesser learns nothing.
func (c *Collections) SharedByToken(ctx context.Context, token string, after *reels.Cursor, limit int) (collections.Shared, error) {
	var collectionID string
	err := c.pool.QueryRow(ctx, `
		SELECT id::text FROM reelpin.collections
		WHERE link_token_hash = $1 AND visibility = 'link'
		  AND (link_expires_at IS NULL OR link_expires_at > now())`,
		collections.HashToken(token),
	).Scan(&collectionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return collections.Shared{}, collections.ErrNotFound
	}
	if err != nil {
		return collections.Shared{}, fmt.Errorf("reading the shared collection: %w", err)
	}

	collection, err := c.get(ctx, collectionID, collections.RoleViewer)
	if err != nil {
		return collections.Shared{}, err
	}
	items, page, err := c.items(ctx, collectionID, after, limit)
	if err != nil {
		return collections.Shared{}, err
	}

	// An anonymous reader learns what is in the collection, not who is in it.
	collection.OwnerID = ""
	collection.MemberCount = 0
	return collections.Shared{Collection: collection, Items: items, Page: page}, nil
}

// items loads one keyset page of item cards from the canonical tables in one
// query. The cursor is (added_at, save_id), the same opaque shape the reel
// list uses.
func (c *Collections) items(ctx context.Context, collectionID string, after *reels.Cursor, limit int) ([]collections.Item, collections.Page, error) {
	where := "i.collection_id = $1"
	args := []any{collectionID}
	if after != nil {
		args = append(args, after.SavedAt, after.ID)
		where += fmt.Sprintf(" AND (i.added_at, i.save_id) < ($%d, $%d)", len(args)-1, len(args))
	}
	args = append(args, limit+1)

	rows, err := c.pool.Query(ctx, fmt.Sprintf(`
		SELECT s.id::text, v.title, v.summary, co.normalized_url,
		       NULLIF(v.media->>'thumbnail_url', ''),
		       co.source_platform, s.saved_at, i.added_at, i.added_by::text
		FROM reelpin.collection_items i
		JOIN reelpin.user_saves s ON s.id = i.save_id
		JOIN reelpin.contents co ON co.id = s.content_id
		LEFT JOIN reelpin.content_versions v ON v.id = co.current_version_id
		WHERE %s
		ORDER BY i.added_at DESC, i.save_id DESC
		LIMIT $%d`, where, len(args)), args...)
	if err != nil {
		return nil, collections.Page{}, fmt.Errorf("reading collection items: %w", err)
	}
	defer rows.Close()

	items := []collections.Item{}
	cursors := []reels.Cursor{}
	for rows.Next() {
		var (
			item             collections.Item
			title, summary   *string
			savedAt, addedAt *time.Time
		)
		if err := rows.Scan(&item.ReelID, &title, &summary, &item.URL, &item.ThumbnailURL,
			&item.SourcePlatform, &savedAt, &addedAt, &item.AddedBy); err != nil {
			return nil, collections.Page{}, fmt.Errorf("reading a collection item: %w", err)
		}
		item.Title = text(title)
		item.Summary = text(summary)
		item.SavedAt = collections.ISOTime(savedAt)
		item.AddedAt = collections.ISOTime(addedAt)
		items = append(items, item)
		if addedAt != nil {
			cursors = append(cursors, reels.Cursor{SavedAt: *addedAt, ID: item.ReelID})
		} else {
			cursors = append(cursors, reels.Cursor{})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, collections.Page{}, fmt.Errorf("reading collection items: %w", err)
	}

	page := collections.Page{Limit: limit}
	if len(items) > limit {
		items = items[:limit]
		last := cursors[limit-1]
		if last.ID != "" && !last.SavedAt.IsZero() {
			encoded := last.Encode()
			page.NextCursor = &encoded
			page.HasMore = true
		}
	}
	return items, page, nil
}

// Members lists collaborators. Any role may see who else can see the
// collection they are in.
func (c *Collections) Members(ctx context.Context, collectionID, userID string) (string, []collections.Member, error) {
	if _, err := c.Role(ctx, collectionID, userID); err != nil {
		return "", nil, err
	}

	var ownerID string
	if err := c.pool.QueryRow(ctx,
		`SELECT owner_id::text FROM reelpin.collections WHERE id = $1`, collectionID).Scan(&ownerID); err != nil {
		return "", nil, collections.ErrNotFound
	}

	rows, err := c.pool.Query(ctx, `
		SELECT user_id::text, role, created_at
		FROM reelpin.collection_members
		WHERE collection_id = $1
		ORDER BY created_at`, collectionID)
	if err != nil {
		return "", nil, fmt.Errorf("reading members: %w", err)
	}
	defer rows.Close()

	members := []collections.Member{}
	for rows.Next() {
		var member collections.Member
		var createdAt *time.Time
		if err := rows.Scan(&member.UserID, &member.Role, &createdAt); err != nil {
			return "", nil, fmt.Errorf("reading members: %w", err)
		}
		member.CreatedAt = collections.ISOTime(createdAt)
		members = append(members, member)
	}
	return ownerID, members, rows.Err()
}

func (c *Collections) RemoveMember(ctx context.Context, collectionID, userID, memberUserID string) error {
	role, err := c.Role(ctx, collectionID, userID)
	if err != nil {
		return err
	}
	if role != collections.RoleOwner {
		return collections.ErrForbidden
	}
	if _, err := c.pool.Exec(ctx,
		`DELETE FROM reelpin.collection_members WHERE collection_id = $1 AND user_id = $2`,
		collectionID, memberUserID,
	); err != nil {
		return fmt.Errorf("removing the member: %w", err)
	}
	return nil
}

// Leave is a member removing themselves. An owner cannot leave their own
// collection: they delete it or keep it.
func (c *Collections) Leave(ctx context.Context, collectionID, userID string) error {
	role, err := c.Role(ctx, collectionID, userID)
	if err != nil {
		return err
	}
	if role == collections.RoleOwner {
		return collections.ErrForbidden
	}
	if _, err := c.pool.Exec(ctx,
		`DELETE FROM reelpin.collection_members WHERE collection_id = $1 AND user_id = $2`,
		collectionID, userID,
	); err != nil {
		return fmt.Errorf("leaving the collection: %w", err)
	}
	return nil
}

// EnableLink mints a view-link token with an expiry. Only its hash is stored,
// so the link exists once, in this response; re-minting revokes the old link.
func (c *Collections) EnableLink(ctx context.Context, collectionID, userID string) (string, string, time.Time, error) {
	role, err := c.Role(ctx, collectionID, userID)
	if err != nil {
		return "", "", time.Time{}, err
	}
	if role != collections.RoleOwner {
		return "", "", time.Time{}, collections.ErrForbidden
	}

	token, err := collections.NewToken()
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("minting a link token: %w", err)
	}
	expiresAt := c.now().UTC().Add(collections.LinkExpiry)
	if _, err := c.pool.Exec(ctx, `
		UPDATE reelpin.collections
		SET visibility = 'link', link_token_hash = $2, link_expires_at = $3, updated_at = now()
		WHERE id = $1`, collectionID, collections.HashToken(token), expiresAt,
	); err != nil {
		return "", "", time.Time{}, fmt.Errorf("enabling the link: %w", err)
	}
	return c.shareBase + "/c/" + token, token, expiresAt, nil
}

// DisableLink revokes the capability by forgetting its hash.
func (c *Collections) DisableLink(ctx context.Context, collectionID, userID string) error {
	role, err := c.Role(ctx, collectionID, userID)
	if err != nil {
		return err
	}
	if role != collections.RoleOwner {
		return collections.ErrForbidden
	}
	if _, err := c.pool.Exec(ctx, `
		UPDATE reelpin.collections
		SET visibility = 'private', link_token_hash = NULL, link_expires_at = NULL, updated_at = now()
		WHERE id = $1`, collectionID,
	); err != nil {
		return fmt.Errorf("disabling the link: %w", err)
	}
	return nil
}

// CreateInvite mints a single capability with an expiry. Only the hash is kept.
func (c *Collections) CreateInvite(ctx context.Context, collectionID, userID, role string) (string, string, string, time.Time, error) {
	actorRole, err := c.Role(ctx, collectionID, userID)
	if err != nil {
		return "", "", "", time.Time{}, err
	}
	if actorRole != collections.RoleOwner {
		return "", "", "", time.Time{}, collections.ErrForbidden
	}
	if role != collections.RoleEditor {
		role = collections.RoleViewer
	}

	token, err := collections.NewToken()
	if err != nil {
		return "", "", "", time.Time{}, fmt.Errorf("minting an invite: %w", err)
	}
	expiresAt := c.now().UTC().Add(collections.InviteExpiry)

	if _, err := c.pool.Exec(ctx, `
		INSERT INTO reelpin.collection_invites (collection_id, token_hash, role, created_by, expires_at)
		VALUES ($1, $2, $3, $4, $5)`,
		collectionID, collections.HashToken(token), role, userID, expiresAt,
	); err != nil {
		return "", "", "", time.Time{}, fmt.Errorf("creating the invite: %w", err)
	}
	return c.shareBase + "/c/invite/" + token, token, role, expiresAt, nil
}

// AcceptInvite turns a capability into membership. Accepting twice leaves one
// membership and spends one use.
func (c *Collections) AcceptInvite(ctx context.Context, token, userID string) (collections.Collection, error) {
	transaction, err := c.pool.Begin(ctx)
	if err != nil {
		return collections.Collection{}, fmt.Errorf("starting the accept: %w", err)
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
		SELECT i.id::text, i.collection_id::text, i.role, i.expires_at, i.max_uses, i.uses, i.revoked, c.owner_id::text
		FROM reelpin.collection_invites i
		JOIN reelpin.collections c ON c.id = i.collection_id
		WHERE i.token_hash = $1
		FOR UPDATE OF i`, collections.HashToken(token),
	).Scan(&inviteID, &collectionID, &role, &expiresAt, &maxUses, &uses, &revoked, &ownerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return collections.Collection{}, collections.ErrInviteInvalid
	}
	if err != nil {
		return collections.Collection{}, fmt.Errorf("reading the invite: %w", err)
	}

	now := c.now().UTC()
	switch {
	case revoked:
		return collections.Collection{}, collections.ErrInviteInvalid
	case expiresAt != nil && now.After(*expiresAt):
		return collections.Collection{}, collections.ErrInviteInvalid
	case maxUses != nil && uses >= *maxUses:
		return collections.Collection{}, collections.ErrInviteInvalid
	}

	// The owner accepting their own invite is a no-op, not a membership.
	if ownerID != userID {
		tag, err := transaction.Exec(ctx, `
			INSERT INTO reelpin.collection_members (collection_id, user_id, role, invited_by)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (collection_id, user_id) DO NOTHING`,
			collectionID, userID, role, ownerID)
		if err != nil {
			return collections.Collection{}, fmt.Errorf("adding the member: %w", err)
		}
		if tag.RowsAffected() > 0 {
			if _, err := transaction.Exec(ctx,
				`UPDATE reelpin.collection_invites SET uses = uses + 1 WHERE id = $1`, inviteID,
			); err != nil {
				return collections.Collection{}, fmt.Errorf("recording the invite use: %w", err)
			}
		}
	}

	if err := transaction.Commit(ctx); err != nil {
		return collections.Collection{}, fmt.Errorf("committing the accept: %w", err)
	}
	return c.Get(ctx, collectionID, userID)
}

func scanCollection(rows pgx.Rows) (collections.Collection, error) {
	var (
		collection collections.Collection
		cover      *string
		createdAt  *time.Time
		updatedAt  *time.Time
	)
	if err := rows.Scan(&collection.ID, &collection.OwnerID, &collection.Name, &collection.Description,
		&cover, &collection.Visibility, &createdAt, &updatedAt,
		&collection.Role, &collection.ItemCount, &collection.MemberCount); err != nil {
		return collections.Collection{}, fmt.Errorf("reading a collection: %w", err)
	}
	collection.CoverReelID = cover
	collection.CreatedAt = collections.ISOTime(createdAt)
	collection.UpdatedAt = collections.ISOTime(updatedAt)
	return collection, nil
}

func optionalText(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}
