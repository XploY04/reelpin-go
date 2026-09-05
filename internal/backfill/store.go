package backfill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/XploY04/reelpin-go/internal/sourceidentity"
	"github.com/jackc/pgx/v5"
)

// cursor is where the last run of this backfill version stopped.
func (b *Backfiller) cursor(ctx context.Context, table string) (string, error) {
	var last *string
	err := b.pool.QueryRow(ctx,
		`SELECT last_source_id::text FROM reelpin.backfill_progress
		 WHERE backfill_version = $1 AND source_table = $2`,
		Version, table,
	).Scan(&last)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading the backfill cursor: %w", err)
	}
	if last == nil {
		return "", nil
	}
	return *last, nil
}

// saveCursor advances the resume point. A dry run keeps no cursor: it must be
// repeatable and produce identical totals.
func (b *Backfiller) saveCursor(ctx context.Context, options Options, table, cursor string, scanned int) error {
	if !options.Execute {
		return nil
	}
	_, err := b.pool.Exec(ctx, `
		INSERT INTO reelpin.backfill_progress (backfill_version, source_table, last_source_id, scanned)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (backfill_version, source_table)
		DO UPDATE SET last_source_id = EXCLUDED.last_source_id,
		              scanned = EXCLUDED.scanned,
		              updated_at = now()`,
		Version, table, nullableUUID(cursor), scanned,
	)
	if err != nil {
		return fmt.Errorf("saving the backfill cursor: %w", err)
	}
	return nil
}

func (b *Backfiller) finishCursor(ctx context.Context, options Options, table string) error {
	if !options.Execute {
		return nil
	}
	_, err := b.pool.Exec(ctx, `
		UPDATE reelpin.backfill_progress SET finished_at = now(), updated_at = now()
		WHERE backfill_version = $1 AND source_table = $2`,
		Version, table,
	)
	if err != nil {
		return fmt.Errorf("closing the backfill cursor: %w", err)
	}
	return nil
}

// audit records one decision outside a transaction, for the skip paths.
func (b *Backfiller) audit(
	ctx context.Context,
	options Options,
	batch int64,
	table, sourceID, action string,
	contentID, contentVersionID, runID *string,
	note string,
) error {
	if !options.Execute {
		return nil
	}
	_, err := b.pool.Exec(ctx, auditStatement,
		Version, batch, table, sourceID, action, contentID, contentVersionID, runID, note)
	if err != nil {
		return fmt.Errorf("writing the backfill audit: %w", err)
	}
	return nil
}

func auditTx(
	ctx context.Context,
	tx pgx.Tx,
	batch int64,
	table, sourceID, action string,
	contentID, contentVersionID, runID *string,
	note string,
) error {
	_, err := tx.Exec(ctx, auditStatement,
		Version, batch, table, sourceID, action, contentID, contentVersionID, runID, note)
	if err != nil {
		return fmt.Errorf("writing the backfill audit: %w", err)
	}
	return nil
}

// The audit is keyed by row, so a resumed run updates its own decision rather
// than writing a second one.
const auditStatement = `
	INSERT INTO reelpin.backfill_audit
		(backfill_version, batch, source_table, source_id, action, content_id,
		 content_version_id, processing_run_id, note)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	ON CONFLICT (backfill_version, source_table, source_id)
	DO UPDATE SET batch = EXCLUDED.batch,
	              action = EXCLUDED.action,
	              content_id = EXCLUDED.content_id,
	              content_version_id = EXCLUDED.content_version_id,
	              processing_run_id = EXCLUDED.processing_run_id,
	              note = EXCLUDED.note`

// publicContentID is null for a link with no id of its own, which is what makes
// the generic uniqueness index apply instead of the public one.
func publicContentID(identity sourceidentity.SourceIdentity) *string {
	if identity.ContentType == "link" || identity.ContentID == "" {
		return nil
	}
	id := identity.ContentID
	return &id
}

func urlHash(identity sourceidentity.SourceIdentity) string {
	sum := sha256.Sum256([]byte(identity.NormalizedURL))
	return hex.EncodeToString(sum[:])
}

func nullableUUID(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func text(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func number(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}
