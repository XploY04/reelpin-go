package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/collections"
	"github.com/XploY04/reelpin-go/internal/reels"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const bridgeProcessor = "python-collection-bridge-v1"

func (c *Collections) EnsureBridgeUser(ctx context.Context, userID string) error {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = bridgeUserLock(ctx, tx, userID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `LOCK TABLE reelpin.account_deletion_requests IN SHARE MODE`); err != nil {
		return err
	}
	if err = bridgeUserActive(ctx, tx, userID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO auth.users(id) SELECT $1::uuid WHERE NOT EXISTS(SELECT 1 FROM reelpin.account_deletion_requests WHERE user_id=$1::uuid) ON CONFLICT(id) DO NOTHING`, userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func bridgeUserLock(ctx context.Context, tx pgx.Tx, userID string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('collection-bridge-user:' || $1,0))`, userID)
	return err
}

type bridgeActorReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func bridgeUserActive(ctx context.Context, tx bridgeActorReader, userID string) error {
	var deleted bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM reelpin.account_deletion_requests WHERE user_id=$1) OR EXISTS(SELECT 1 FROM reelpin.collection_bridge_deletions WHERE user_id=$1 AND save_id='00000000-0000-0000-0000-000000000000')`, userID).Scan(&deleted)
	if err != nil {
		return err
	}
	if deleted {
		return collections.ErrBridgeDeleted
	}
	return nil
}

func bridgeActor(ctx context.Context, tx pgx.Tx, userID string) error {
	if err := bridgeUserLock(ctx, tx, userID); err != nil {
		return err
	}
	var id string
	if _, err := tx.Exec(ctx, `LOCK TABLE reelpin.account_deletion_requests IN SHARE MODE`); err != nil {
		return err
	}
	err := tx.QueryRow(ctx, `SELECT id::text FROM auth.users WHERE id=$1`, userID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return collections.ErrBridgeDeleted
	}
	if err != nil {
		return err
	}
	return bridgeUserActive(ctx, tx, userID)
}

func bridgeRole(ctx context.Context, tx pgx.Tx, collectionID, userID string) (string, error) {
	var owner string
	err := tx.QueryRow(ctx, `SELECT owner_id::text FROM reelpin.collections WHERE id=$1 FOR UPDATE`, collectionID).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", collections.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if owner == userID {
		return collections.RoleOwner, nil
	}
	var role string
	err = tx.QueryRow(ctx, `SELECT role FROM reelpin.collection_members WHERE collection_id=$1 AND user_id=$2 FOR SHARE`, collectionID, userID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", collections.ErrNotFound
	}
	return role, err
}

func (c *Collections) CreateImported(ctx context.Context, userID, name, description string, ids []string, snapshots []collections.Snapshot) (collections.Collection, int, error) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return collections.Collection{}, 0, err
	}
	defer tx.Rollback(ctx)
	if err = bridgeActor(ctx, tx, userID); err != nil {
		return collections.Collection{}, 0, err
	}
	var id string
	err = tx.QueryRow(ctx, `INSERT INTO reelpin.collections(owner_id,name,description) VALUES($1,$2,$3) RETURNING id::text`, userID, strings.TrimSpace(name), description).Scan(&id)
	if err != nil {
		return collections.Collection{}, 0, err
	}
	if err = importSnapshots(ctx, tx, userID, snapshots); err != nil {
		return collections.Collection{}, 0, err
	}
	added, err := fileSaves(ctx, tx, userID, id, ids)
	if err != nil {
		return collections.Collection{}, 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return collections.Collection{}, 0, err
	}
	collection, err := c.Get(ctx, id, userID)
	return collection, added, err
}

func (c *Collections) AddImported(ctx context.Context, id, userID string, ids []string, snapshots []collections.Snapshot) (collections.Collection, int, error) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return collections.Collection{}, 0, err
	}
	defer tx.Rollback(ctx)
	if err = bridgeActor(ctx, tx, userID); err != nil {
		return collections.Collection{}, 0, err
	}
	role, err := bridgeRole(ctx, tx, id, userID)
	if err != nil {
		return collections.Collection{}, 0, err
	}
	if !collections.CanEdit(role) {
		return collections.Collection{}, 0, collections.ErrForbidden
	}
	if err = importSnapshots(ctx, tx, userID, snapshots); err != nil {
		return collections.Collection{}, 0, err
	}
	added, err := fileSaves(ctx, tx, userID, id, ids)
	if err != nil {
		return collections.Collection{}, 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return collections.Collection{}, 0, err
	}
	collection, err := c.Get(ctx, id, userID)
	return collection, added, err
}

func (c *Collections) EditableTargets(ctx context.Context, userID string, ids []string) ([]string, error) {
	result := make([]string, 0, len(ids))
	for _, id := range ids {
		role, err := c.Role(ctx, id, userID)
		if errors.Is(err, collections.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if collections.CanEdit(role) {
			result = append(result, id)
		}
	}
	return result, nil
}

func (c *Collections) FileImported(ctx context.Context, userID string, targetIDs, ids []string, snapshots []collections.Snapshot) ([]string, error) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err = bridgeActor(ctx, tx, userID); err != nil {
		return nil, err
	}
	targetIDs = append([]string{}, targetIDs...)
	sort.Strings(targetIDs)
	targets := make([]string, 0, len(targetIDs))
	for _, id := range targetIDs {
		role, err := bridgeRole(ctx, tx, id, userID)
		if errors.Is(err, collections.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if collections.CanEdit(role) {
			targets = append(targets, id)
		}
	}
	if len(targets) == 0 {
		return targets, nil
	}
	if err = importSnapshots(ctx, tx, userID, snapshots); err != nil {
		return nil, err
	}
	for _, id := range targets {
		if _, err = fileSaves(ctx, tx, userID, id, ids); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return targets, nil
}

func importSnapshots(ctx context.Context, tx pgx.Tx, userID string, snapshots []collections.Snapshot) error {
	snapshots = append([]collections.Snapshot{}, snapshots...)
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].ID < snapshots[j].ID })
	for _, item := range snapshots {
		if item.UserID != userID {
			return collections.ErrBridgeInput
		}
		var deleted bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM reelpin.collection_bridge_deletions WHERE user_id=$1 AND save_id=$2)`, userID, item.ID).Scan(&deleted); err != nil {
			return err
		}
		if deleted {
			return reels.ErrNotFound
		}
		parsed, err := url.Parse(item.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil {
			return collections.ErrBridgeInput
		}
		if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, item.ID); err != nil {
			return err
		}
		var owner string
		err = tx.QueryRow(ctx, `SELECT user_id::text FROM reelpin.user_saves WHERE id=$1`, item.ID).Scan(&owner)
		if err == nil {
			if owner != userID {
				return reels.ErrNotFound
			}
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		contentID := uuid.NewSHA1(uuid.NameSpaceURL, []byte("python-collection-bridge:"+item.ID)).String()
		normalized := item.NormalizedURL
		if normalized == "" {
			normalized = item.URL
		}
		hash := sha256.Sum256([]byte(normalized))
		platform := item.SourcePlatform
		if platform == "" {
			platform = parsed.Hostname()
		}
		kind := item.SourceContentType
		switch kind {
		case "reel", "post", "video", "image", "carousel", "article", "page", "place", "other":
		default:
			kind = "other"
		}
		_, err = tx.Exec(ctx, `INSERT INTO reelpin.contents(id,source_platform,source_content_type,normalized_url,normalized_url_hash,source_content_id,access_scope_hash)
   VALUES($1,$2,$3,$4,$5,$6,$7)`, contentID, platform, kind, normalized, hex.EncodeToString(hash[:]), item.SourceContentID, "python-collection-bridge:"+userID+":"+item.ID)
		if err != nil {
			return err
		}
		array := func(v json.RawMessage) json.RawMessage {
			if len(v) == 0 || string(v) == "null" {
				return json.RawMessage(`[]`)
			}
			return v
		}
		raw, err := json.Marshal(map[string]any{"category": item.Category, "subcategory": item.Subcategory, "parse_status": item.ParseStatus, "extraction": map[string]any{
			"locations": array(item.Locations), "people_mentioned": array(item.PeopleMentioned), "actionable_items": array(item.ActionableItems), "events": array(item.Events)}})
		if err != nil {
			return err
		}
		media, err := json.Marshal(map[string]string{"thumbnail_url": item.ThumbnailURL})
		if err != nil {
			return err
		}
		var versionID string
		err = tx.QueryRow(ctx, `INSERT INTO reelpin.content_versions(content_id,processor_version,prompt_version,schema_version,model_version,title,summary,caption,transcript,tags,key_facts,raw_extraction,media)
   VALUES($1,$2,'legacy','legacy','legacy',$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id::text`, contentID, bridgeProcessor, item.Title, item.Summary, item.Caption, item.Transcript, nonNil(item.SecondaryCategories), nonNil(item.KeyFacts), string(raw), string(media)).Scan(&versionID)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE reelpin.contents SET current_version_id=$2 WHERE id=$1`, contentID, versionID); err != nil {
			return err
		}
		savedAt := time.Now().UTC()
		if item.CreatedAt != nil {
			savedAt = *item.CreatedAt
		}
		if _, err = tx.Exec(ctx, `INSERT INTO reelpin.user_saves(id,user_id,content_id,saved_at,source_url) VALUES($1,$2,$3,$4,$5)`, item.ID, userID, contentID, savedAt, item.URL); err != nil {
			return err
		}
	}
	return nil
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func (c *Collections) VisibleReel(ctx context.Context, userID, id string) (reels.ReelRecord, error) {
	rows, err := c.pool.Query(ctx, fmt.Sprintf(`SELECT %s,transcript FROM %s WHERE id=$1 AND (user_id=$2 OR EXISTS(
 SELECT 1 FROM reelpin.collection_items i JOIN reelpin.collections c ON c.id=i.collection_id
 WHERE i.save_id=reels.id AND (c.owner_id=$2 OR EXISTS(SELECT 1 FROM reelpin.collection_members m WHERE m.collection_id=c.id AND m.user_id=$2))))`, reelListColumns, savedReels), id, userID)
	if err != nil {
		return reels.ReelRecord{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err = rows.Err(); err != nil {
			return reels.ReelRecord{}, err
		}
		return reels.ReelRecord{}, reels.ErrNotFound
	}
	record, err := scanReel(rows, true)
	if err != nil {
		return reels.ReelRecord{}, err
	}
	return record, rows.Err()
}

func (c *Collections) DeleteImported(ctx context.Context, userID, id string) error {
	if parsed, err := uuid.Parse(id); err != nil || parsed == uuid.Nil {
		return collections.ErrBridgeInput
	}
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = bridgeUserLock(ctx, tx, userID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO reelpin.collection_bridge_deletions(user_id,save_id) VALUES($1,$2) ON CONFLICT(user_id,save_id) DO NOTHING`, userID, id); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `SELECT reelpin.delete_python_collection_saves($1::uuid,$2::uuid)`, userID, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (c *Collections) DeleteBridgeAccount(ctx context.Context, userID string) error {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = bridgeUserLock(ctx, tx, userID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO reelpin.collection_bridge_deletions(user_id) VALUES($1) ON CONFLICT(user_id,save_id) DO NOTHING`, userID); err != nil {
		return err
	}
	for _, statement := range []string{
		`DELETE FROM reelpin.collections WHERE owner_id=$1`,
		`DELETE FROM reelpin.collection_members WHERE user_id=$1`,
		`DELETE FROM reelpin.collection_invites WHERE created_by=$1`,
		`SELECT reelpin.delete_python_collection_saves($1::uuid,NULL::uuid)`,
	} {
		if _, err = tx.Exec(ctx, statement, userID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
