package backfill

import (
	"context"
	"fmt"
)

// sourceUsers is the legacy table of users: the id, email and created_at of
// Supabase's auth.users, exported alone so no password hash or token ever
// leaves Supabase. See docs/rehearsal.md.
const sourceUsers = "users"

// notDeleted leaves out a legacy user l who asked Go to delete their account.
// The request row has no foreign key, so it outlives the auth.users row and
// the migration record the deletion cascaded away.
const notDeleted = `NOT EXISTS (SELECT 1 FROM reelpin.account_deletion_requests d WHERE d.user_id = l.id)`

// copyUsers materialises every legacy user in auth.users, which every
// user-owned canonical table points at, so it runs before anything else. The
// table is a few hundred rows, so every run, a delta or not, upserts all of it.
// A user Go has served is not skipped: their row is already there from their
// first request, and the email is the only thing an upsert changes. A user Go
// deleted is skipped, or the next run would bring them back. Copied counts
// the users inserted or whose email changed.
func (b *Backfiller) copyUsers(ctx context.Context, report *Report) error {
	counts := TableReport{SkippedByAction: map[string]int{}}
	defer func() { report.Tables[sourceUsers] = counts }()

	transaction, err := b.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("starting the users transaction: %w", err)
	}
	defer transaction.Rollback(ctx)

	from := b.source.table(sourceUsers)
	if err := transaction.QueryRow(ctx, `SELECT count(*) FROM `+from+` l WHERE `+notDeleted).
		Scan(&counts.Scanned); err != nil {
		return fmt.Errorf("counting legacy users: %w", err)
	}
	tag, err := transaction.Exec(ctx, `
		INSERT INTO auth.users AS u (id, email, created_at)
		SELECT l.id, l.email, coalesce(l.created_at, now()) FROM `+from+` l
		WHERE `+notDeleted+`
		ON CONFLICT (id) DO UPDATE SET email = EXCLUDED.email
		WHERE u.email IS DISTINCT FROM EXCLUDED.email`)
	if err != nil {
		return fmt.Errorf("copying legacy users: %w", err)
	}
	counts.Copied = int(tag.RowsAffected())

	// Committed on a dry run too: it only mirrors identities Supabase already
	// has, and every other table's dry-run count needs their parent rows.
	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf("committing legacy users: %w", err)
	}
	report.UsersWritten = true
	return nil
}

// deleteUsers is the final delta's step for users gone from Supabase: their
// auth.users row goes, and every canonical row they own with it by cascade. A
// user Go has served stays, as their rows do in every other table.
func (b *Backfiller) deleteUsers() string {
	return `
		WITH removed AS (
			DELETE FROM auth.users u
			WHERE NOT EXISTS (SELECT 1 FROM ` + b.source.table(sourceUsers) + ` l WHERE l.id = u.id)
			  AND NOT EXISTS (SELECT 1 FROM ` + servedUsers + ` m WHERE m.user_id = u.id)
			RETURNING 1
		)
		SELECT count(*)::int FROM removed`
}
