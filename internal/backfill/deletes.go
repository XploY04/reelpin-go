package backfill

import (
	"context"
	"fmt"
)

// deletedInLegacy is the audit action a carried row is moved to once its
// legacy row is gone and its canonical row has been deleted with it. It is
// neither carried nor skipped, so the verifier stops expecting the row.
const deletedInLegacy = "deleted_in_legacy"

// propagateDeletes is the final delta's last step. A copy can only add and
// update, so without it a row deleted in Python after the snapshot would live
// on in Go. Only rows this backfill carried are candidates, found through the
// audit, and never a row owned by a user Go has served: their Go rows are
// theirs. Everything runs in one transaction, and a dry run rolls it back.
func (b *Backfiller) propagateDeletes(ctx context.Context, options Options, report *Report) error {
	transaction, err := b.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("starting the delete transaction: %w", err)
	}
	defer transaction.Rollback(ctx)

	report.Deleted = map[string]int{}
	for _, source := range verifySources() {
		if source.owner == "" || !b.source.shape(source.name).exists {
			continue
		}
		statement := b.deleteCarried(source)
		if source.canonicalKey == "" {
			statement = b.deleteHiddenPins(source)
		}
		var deleted int
		if err := transaction.QueryRow(ctx, statement, Version).Scan(&deleted); err != nil {
			return fmt.Errorf("propagating deletes from legacy %s: %w", source.name, err)
		}
		report.Deleted[source.name] = deleted
	}

	if !options.Execute {
		return nil
	}
	return transaction.Commit(ctx)
}

// gone is the carried audit rows of one legacy table whose legacy row no
// longer exists, aliased a.
func (b *Backfiller) gone(source legacySource) string {
	return `
		SELECT a.source_id FROM reelpin.backfill_audit a
		WHERE a.backfill_version = $1 AND a.source_table = '` + source.name + `'
		  AND a.action = '` + source.carried + `'
		  AND NOT EXISTS (SELECT 1 FROM ` + b.source.table(source.name) + ` l
		      WHERE ` + keyExpression(source.name, source.key, b.source.shape(source.name)) + ` = a.source_id)`
}

func (b *Backfiller) markGone(source legacySource) string {
	return `
		UPDATE reelpin.backfill_audit a SET action = '` + deletedInLegacy + `', note = ''
		FROM gone
		WHERE a.backfill_version = $1 AND a.source_table = '` + source.name + `'
		  AND a.source_id = gone.source_id
		RETURNING 1`
}

// deleteCarried deletes the canonical row each gone audit row names. A
// canonical row that is already gone, through a cascade from its save, still
// has its audit row moved.
func (b *Backfiller) deleteCarried(source legacySource) string {
	return `
		WITH gone AS (` + b.gone(source) + `
		  AND NOT EXISTS (SELECT 1 FROM ` + source.canonical + ` c
		      JOIN reelpin.user_migrations m ON m.user_id = c.user_id
		      WHERE c.` + source.canonicalKey + ` = a.source_id)
		),
		removed AS (
			DELETE FROM ` + source.canonical + ` c USING gone
			WHERE c.` + source.canonicalKey + ` = gone.source_id
			RETURNING 1
		),
		marked AS (` + b.markGone(source) + `)
		SELECT count(*)::int FROM removed`
}

// deleteHiddenPins handles the one carried table with no single canonical id.
// The preference names a location, which only the legacy row could resolve,
// and that row is gone. So the deletion runs the other way: a hidden
// preference of a user Go has not served that no remaining legacy hidden pin
// resolves to came from a pin since unhidden in Python.
func (b *Backfiller) deleteHiddenPins(source legacySource) string {
	return `
		WITH gone AS (` + b.gone(source) + `),
		removed AS (
			DELETE FROM reelpin.user_pin_preferences p
			WHERE p.hidden
			  AND NOT EXISTS (SELECT 1 FROM reelpin.user_migrations m WHERE m.user_id = p.user_id)
			  AND NOT EXISTS (SELECT 1 FROM ` + b.source.table(source.name) + ` l` + hiddenPinJoin + `
			      WHERE cl.id = p.location_id AND s.user_id = p.user_id)
			RETURNING 1
		),
		marked AS (` + b.markGone(source) + `)
		SELECT count(*)::int FROM removed`
}
