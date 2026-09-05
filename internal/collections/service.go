package collections

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/outbox"
	"github.com/XploY04/reelpin-go/internal/reels"
	"github.com/XploY04/reelpin-go/internal/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const collectionColumns = `c.id::text, c.owner_id, c.name, c.description, c.cover_reel_id::text,
	c.visibility, c.created_at, c.updated_at`

type Service struct {
	pool      *pgxpool.Pool
	shareBase string
	now       func() time.Time
	// environment scopes the outbox events this service writes.
	environment string
}

func New(pool *pgxpool.Pool, shareBaseURL string, now func() time.Time, environment string) *Service {
	if environment == "" {
		environment = outbox.DefaultEnvironment
	}
	if now == nil {
		now = time.Now
	}
	return &Service{
		pool:        pool,
		shareBase:   strings.TrimSuffix(shareBaseURL, "/"),
		now:         now,
		environment: environment,
	}
}

// Role answers what a user may do. A user with no relationship gets an empty
// role, which every caller turns into ErrNotFound rather than a 403: a stranger
// must not learn that a collection exists.
func (s *Service) Role(ctx context.Context, collectionID, userID string) (string, error) {
	var ownerID string
	var memberRole *string
	err := s.pool.QueryRow(ctx, `
		SELECT c.owner_id, m.role
		FROM public.collections c
		LEFT JOIN public.collection_members m ON m.collection_id = c.id AND m.user_id = $2
		WHERE c.id = $1`, collectionID, userID,
	).Scan(&ownerID, &memberRole)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("reading collection access: %w", err)
	}

	switch {
	case ownerID == userID:
		return RoleOwner, nil
	case memberRole != nil && *memberRole != "":
		return *memberRole, nil
	default:
		return "", ErrNotFound
	}
}

// List returns everything a user owns or belongs to, newest first, with counts
// loaded in one pass rather than one query per collection.
func (s *Service) List(ctx context.Context, userID string) ([]Collection, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+collectionColumns+`,
		       CASE WHEN c.owner_id = $1 THEN 'owner' ELSE COALESCE(m.role, 'viewer') END AS role,
		       (SELECT count(*) FROM public.collection_items i WHERE i.collection_id = c.id) AS item_count,
		       (SELECT count(*) FROM public.collection_members mm WHERE mm.collection_id = c.id) AS member_count
		FROM public.collections c
		LEFT JOIN public.collection_members m ON m.collection_id = c.id AND m.user_id = $1
		WHERE c.owner_id = $1 OR m.user_id = $1
		ORDER BY c.updated_at DESC
		LIMIT 200`, userID)
	if err != nil {
		return nil, fmt.Errorf("listing collections: %w", err)
	}
	defer rows.Close()

	collections := []Collection{}
	for rows.Next() {
		collection, err := scanCollection(rows)
		if err != nil {
			return nil, err
		}
		collections = append(collections, collection)
	}
	return collections, rows.Err()
}

func (s *Service) Create(ctx context.Context, userID, name, description string, reelIDs []string) (Collection, int, error) {
	cleanName := strings.TrimSpace(name)
	if cleanName == "" {
		return Collection{}, 0, fmt.Errorf("a collection name is required")
	}

	transaction, err := s.pool.Begin(ctx)
	if err != nil {
		return Collection{}, 0, fmt.Errorf("starting the create: %w", err)
	}
	defer transaction.Rollback(ctx)

	var collectionID string
	if err := transaction.QueryRow(ctx, `
		INSERT INTO public.collections (owner_id, name, description)
		VALUES ($1, $2, $3) RETURNING id::text`,
		userID, cleanName, strings.TrimSpace(description),
	).Scan(&collectionID); err != nil {
		return Collection{}, 0, fmt.Errorf("creating the collection: %w", err)
	}

	added, err := addItems(ctx, transaction, collectionID, userID, reelIDs)
	if err != nil {
		return Collection{}, 0, err
	}
	if err := transaction.Commit(ctx); err != nil {
		return Collection{}, 0, fmt.Errorf("committing the create: %w", err)
	}

	collection, err := s.Get(ctx, collectionID, userID)
	return collection, added, err
}

func (s *Service) Get(ctx context.Context, collectionID, userID string) (Collection, error) {
	role, err := s.Role(ctx, collectionID, userID)
	if err != nil {
		return Collection{}, err
	}

	rows, err := s.pool.Query(ctx, `
		SELECT `+collectionColumns+`, $2::text AS role,
		       (SELECT count(*) FROM public.collection_items i WHERE i.collection_id = c.id),
		       (SELECT count(*) FROM public.collection_members m WHERE m.collection_id = c.id)
		FROM public.collections c WHERE c.id = $1`, collectionID, role)
	if err != nil {
		return Collection{}, fmt.Errorf("reading the collection: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return Collection{}, ErrNotFound
	}
	return scanCollection(rows)
}

func (s *Service) Update(ctx context.Context, collectionID, userID string, name, description, coverReelID *string) (Collection, error) {
	role, err := s.Role(ctx, collectionID, userID)
	if err != nil {
		return Collection{}, err
	}
	if role != RoleOwner {
		return Collection{}, ErrForbidden
	}

	if _, err := s.pool.Exec(ctx, `
		UPDATE public.collections
		SET name = COALESCE(NULLIF(TRIM($2), ''), name),
		    description = COALESCE($3, description),
		    cover_reel_id = COALESCE($4::uuid, cover_reel_id),
		    updated_at = now()
		WHERE id = $1`,
		collectionID, optional(name), optional(description), optional(coverReelID),
	); err != nil {
		return Collection{}, fmt.Errorf("updating the collection: %w", err)
	}
	return s.Get(ctx, collectionID, userID)
}

func (s *Service) Delete(ctx context.Context, collectionID, userID string) error {
	role, err := s.Role(ctx, collectionID, userID)
	if err != nil {
		return err
	}
	if role != RoleOwner {
		return ErrForbidden
	}
	// Items, members and invites cascade; the reels themselves are untouched.
	if _, err := s.pool.Exec(ctx, `DELETE FROM public.collections WHERE id = $1`, collectionID); err != nil {
		return fmt.Errorf("deleting the collection: %w", err)
	}
	return nil
}

// AddItems files reels into a collection. It is idempotent, so adding the same
// reel twice is not an error and does not duplicate a row.
func (s *Service) AddItems(ctx context.Context, collectionID, userID string, reelIDs []string) (int, error) {
	role, err := s.Role(ctx, collectionID, userID)
	if err != nil {
		return 0, err
	}
	if !CanEdit(role) {
		return 0, ErrForbidden
	}

	transaction, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("starting the add: %w", err)
	}
	defer transaction.Rollback(ctx)

	added, err := addItems(ctx, transaction, collectionID, userID, reelIDs)
	if err != nil {
		return 0, err
	}
	if added > 0 {
		if err := emitCollectionEvent(ctx, transaction, collectionID, userID, added, s.environment); err != nil {
			return 0, err
		}
	}
	if err := transaction.Commit(ctx); err != nil {
		return 0, fmt.Errorf("committing the add: %w", err)
	}
	return added, nil
}

func (s *Service) RemoveItem(ctx context.Context, collectionID, userID, reelID string) error {
	role, err := s.Role(ctx, collectionID, userID)
	if err != nil {
		return err
	}
	if !CanEdit(role) {
		return ErrForbidden
	}
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM public.collection_items WHERE collection_id = $1 AND reel_id = $2`,
		collectionID, reelID,
	); err != nil {
		return fmt.Errorf("removing the item: %w", err)
	}
	return nil
}

// addItems inserts only reels that exist, ignoring duplicates. A reel id that
// does not exist is skipped rather than failing the whole request: a stale
// picker on a device must not cost the user the rest of the batch.
func addItems(ctx context.Context, tx pgx.Tx, collectionID, userID string, reelIDs []string) (int, error) {
	cleaned := make([]string, 0, len(reelIDs))
	seen := map[string]bool{}
	for _, raw := range reelIDs {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] {
			continue
		}
		if _, err := uuid.Parse(id); err != nil {
			continue
		}
		seen[id] = true
		cleaned = append(cleaned, id)
		if len(cleaned) >= MaxReelIDsPerRequest {
			break
		}
	}
	if len(cleaned) == 0 {
		return 0, nil
	}

	tag, err := tx.Exec(ctx, `
		INSERT INTO public.collection_items (collection_id, reel_id, added_by)
		SELECT $1, r.id, $2 FROM public.reels r WHERE r.id = ANY($3::uuid[])
		ON CONFLICT (collection_id, reel_id) DO NOTHING`,
		collectionID, userID, cleaned)
	if err != nil {
		return 0, fmt.Errorf("adding collection items: %w", err)
	}

	if tag.RowsAffected() > 0 {
		if _, err := tx.Exec(ctx,
			`UPDATE public.collections SET updated_at = now() WHERE id = $1`, collectionID); err != nil {
			return 0, fmt.Errorf("touching the collection: %w", err)
		}
	}
	return int(tag.RowsAffected()), nil
}

// emitCollectionEvent records one notification per actual change, in the same
// transaction as the change itself.
func emitCollectionEvent(ctx context.Context, tx pgx.Tx, collectionID, actorID string, added int, environment string) error {
	payload := map[string]any{
		"run_id":        collectionID,
		"platform":      "collection",
		"collection_id": collectionID,
		"actor_user_id": actorID,
		"added":         added,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encoding the collection event: %w", err)
	}

	var eventID string
	if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&eventID); err != nil {
		return fmt.Errorf("generating an event id: %w", err)
	}
	_ = encoded

	return outbox.Insert(ctx, tx, outbox.Event{
		Environment: environment,
		EventID:     eventID,
		EventType:   "collection.items_added",
		RoutingKey:  "reelpin.notifications",
		Payload:     payload,
	})
}

func scanCollection(rows pgx.Rows) (Collection, error) {
	var (
		collection Collection
		cover      *string
		createdAt  *time.Time
		updatedAt  *time.Time
	)
	if err := rows.Scan(&collection.ID, &collection.OwnerID, &collection.Name, &collection.Description,
		&cover, &collection.Visibility, &createdAt, &updatedAt,
		&collection.Role, &collection.ItemCount, &collection.MemberCount); err != nil {
		return Collection{}, fmt.Errorf("reading a collection: %w", err)
	}
	collection.CoverReelID = cover
	collection.CreatedAt = isoTime(createdAt)
	collection.UpdatedAt = isoTime(updatedAt)
	return collection, nil
}

func isoTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.UTC().Format(time.RFC3339Nano)
	return &formatted
}

func optional(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

var _ = reels.DisplayReel{}
