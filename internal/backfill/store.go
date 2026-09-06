package backfill

import (
	"context"
	"errors"
	"fmt"

	"github.com/XploY04/reelpin-go/internal/sourceidentity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// cursor is where the last run of this backfill version stopped.
func (b *Backfiller) cursor(ctx context.Context, table string) (string, error) {
	var last *string
	err := b.pool.QueryRow(ctx, `
		SELECT last_source_id::text FROM reelpin.backfill_progress
		WHERE backfill_version = $1 AND source_table = $2`,
		Version, table,
	).Scan(&last)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading the backfill cursor: %w", err)
	}
	return text(last), nil
}

// saveCursor advances the resume point. A dry run keeps no cursor: it has to be
// repeatable and produce identical totals every time.
func (b *Backfiller) saveCursor(ctx context.Context, options Options, table, cursor string, scanned int) error {
	if !options.Execute {
		return nil
	}
	if _, err := b.pool.Exec(ctx, `
		INSERT INTO reelpin.backfill_progress (backfill_version, source_table, last_source_id, scanned)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (backfill_version, source_table)
		DO UPDATE SET last_source_id = EXCLUDED.last_source_id,
		              scanned = EXCLUDED.scanned,
		              updated_at = now()`,
		Version, table, nullableUUID(cursor), scanned); err != nil {
		return fmt.Errorf("saving the backfill cursor: %w", err)
	}
	return nil
}

func (b *Backfiller) finishCursor(ctx context.Context, options Options, table string) error {
	if !options.Execute {
		return nil
	}
	if _, err := b.pool.Exec(ctx, `
		UPDATE reelpin.backfill_progress SET finished_at = now(), updated_at = now()
		WHERE backfill_version = $1 AND source_table = $2`,
		Version, table); err != nil {
		return fmt.Errorf("closing the backfill cursor: %w", err)
	}
	return nil
}

// exists reports whether a legacy id has already been carried over. The
// canonical row keeps the legacy id, so its presence is the whole record of
// what this backfill has done.
func (b *Backfiller) exists(ctx context.Context, table, id string) (bool, error) {
	var found bool
	if err := b.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM `+table+` WHERE id = $1)`, id).Scan(&found); err != nil {
		return false, fmt.Errorf("checking %s for %s: %w", table, id, err)
	}
	return found, nil
}

// audit records one decision outside a transaction, for the skip paths.
func (b *Backfiller) audit(
	ctx context.Context,
	options Options,
	batch int64,
	table, sourceID, action string,
	contentID, versionID, runID *string,
	note string,
) error {
	if !options.Execute {
		return nil
	}
	if _, err := b.pool.Exec(ctx, auditStatement,
		Version, batch, table, sourceID, action, contentID, versionID, runID, note); err != nil {
		return fmt.Errorf("writing the backfill audit: %w", err)
	}
	return nil
}

func auditTx(
	ctx context.Context,
	tx pgx.Tx,
	batch int64,
	table, sourceID, action string,
	contentID, versionID, runID *string,
	note string,
) error {
	if _, err := tx.Exec(ctx, auditStatement,
		Version, batch, table, sourceID, action, contentID, versionID, runID, note); err != nil {
		return fmt.Errorf("writing the backfill audit: %w", err)
	}
	return nil
}

const auditStatement = `
	INSERT INTO reelpin.backfill_audit
		(backfill_version, batch, source_table, source_id, action, content_id,
		 content_version_id, run_id, note)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	ON CONFLICT (backfill_version, source_table, source_id)
	DO UPDATE SET batch = EXCLUDED.batch,
	              action = EXCLUDED.action,
	              content_id = EXCLUDED.content_id,
	              content_version_id = EXCLUDED.content_version_id,
	              run_id = EXCLUDED.run_id,
	              note = EXCLUDED.note`

// identityKey is the dry run's stand-in for the uniqueness the two partial
// indexes on contents enforce.
func identityKey(identity sourceidentity.SourceIdentity, scopeHash string) string {
	if identity.ContentID != "" {
		return identity.Platform + "|" + identity.ContentType + "|" + identity.ContentID + "|" + scopeHash
	}
	return "url|" + sourceidentity.URLHash(identity.NormalizedURL) + "|" + scopeHash
}

func isBlocklisted(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == blocklistViolation
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
