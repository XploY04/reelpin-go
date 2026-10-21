//go:build integration

package backfill

import (
	"context"
	"testing"

	"github.com/XploY04/reelpin-go/internal/lifecycle"
)

const (
	newcomer  = "55555555-5555-4555-8555-555555555555"
	latecomer = "66666666-6666-4666-8666-666666666666"

	newcomerReel = "bbbbbbbb-0000-4000-8000-000000000009"
)

// A user who exists only in Supabase reaches auth.users before their reel is
// carried, even on a dry run. Every run upserts every user, including one Go
// has served, but never one Go deleted. The final delta removes the users
// Supabase no longer has unless Go has served them.
func TestUsersArriveFirstAndFollowTheFinalDelta(t *testing.T) {
	pool := testPool(t)
	exec(t, pool, scratchLegacySchema)
	ctx := context.Background()

	exec(t, pool, `
		INSERT INTO legacy.users (id, email, created_at)
		VALUES ($1, 'a@example.com', '2025-01-01T00:00:00Z'),
		       ($2, 'b@example.com', '2025-02-01T00:00:00Z'),
		       ($3, 'new@example.com', '2025-06-01T00:00:00Z')
		ON CONFLICT (id) DO UPDATE SET email = EXCLUDED.email, created_at = EXCLUDED.created_at`,
		userA, userB, newcomer)
	exec(t, pool, `
		INSERT INTO legacy.reels (id, user_id, url, title, created_at)
		VALUES ($1, $2, 'https://www.instagram.com/reel/NEWBIE9/', 'Only in Supabase', '2026-01-01T00:00:00Z')`,
		newcomerReel, newcomer)

	// The dry run commits the users, so the rows that point at them are
	// counted the way a real run would count them.
	dry, err := New(pool, quiet()).Run(ctx, Options{LegacySchema: scratchSchema})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if users := dry.Tables[sourceUsers]; users.Scanned != 3 || users.Copied != 3 || !dry.UsersWritten {
		t.Errorf("dry run users = %+v, written %v; want 3 scanned, 3 copied, written", users, dry.UsersWritten)
	}
	if !exists(t, pool, `
		SELECT 1 FROM auth.users
		WHERE id = $1 AND email = 'new@example.com' AND created_at = '2025-06-01T00:00:00Z'`, newcomer) {
		t.Error("the dry run did not carry the user only Supabase had, with their email and created_at")
	}
	if total := count(t, pool, "reelpin.user_saves"); total != 0 {
		t.Errorf("the dry run wrote %d saves", total)
	}
	again, err := New(pool, quiet()).Run(ctx, Options{LegacySchema: scratchSchema})
	if err != nil {
		t.Fatalf("second dry run: %v", err)
	}
	if again.Tables[sourceUsers].Copied != 0 {
		t.Errorf("an unchanged users table copied %+v, want nothing", again.Tables[sourceUsers])
	}

	snapshot, err := New(pool, quiet()).Run(ctx, Options{Execute: true, DumpedAt: dumpedNow(), LegacySchema: scratchSchema})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if !exists(t, pool, `SELECT 1 FROM reelpin.user_saves WHERE id = $1 AND user_id = $2`, newcomerReel, newcomer) {
		t.Error("the newcomer's reel was not carried")
	}

	verified, err := NewVerifier(pool).Verify(ctx, VerifyOptions{LegacySchema: scratchSchema})
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	if users := tableOf(t, verified, sourceUsers); users.InScope != 3 || users.Present != 3 || users.Unexplained != 0 {
		t.Errorf("verified users = %+v, want all 3 present", users)
	}
	if !verified.OK() {
		t.Errorf("the snapshot does not verify: %+v", verified)
	}

	// User a moves to Go and changes email in Supabase, a new user signs up
	// there, and user b deletes their account in Go. The deletion cascades
	// user b's auth.users row away; only the deletion request remains.
	exec(t, pool, `INSERT INTO reelpin.user_migrations (user_id) VALUES ($1)`, userA)
	exec(t, pool, `UPDATE legacy.users SET email = 'a2@example.com' WHERE id = $1`, userA)
	exec(t, pool, `INSERT INTO legacy.users (id, email) VALUES ($1, 'late@example.com')`, latecomer)
	if _, err := lifecycle.New(pool, nil, nil, quiet()).DeleteAccount(ctx, userB); err != nil {
		t.Fatalf("deleting user b's account: %v", err)
	}

	behind, err := NewVerifier(pool).Verify(ctx, VerifyOptions{LegacySchema: scratchSchema})
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	users := tableOf(t, behind, sourceUsers)
	if behind.OK() || users.InScope != 3 || users.Unexplained != 1 ||
		len(users.UnexplainedIDs) != 1 || users.UnexplainedIDs[0] != latecomer {
		t.Errorf("verified users = %+v, want user b out of scope, the latecomer unexplained and the run failed", users)
	}

	delta, err := New(pool, quiet()).Run(ctx, Options{
		Execute: true, DumpedAt: dumpedNow(), LegacySchema: scratchSchema, Since: snapshot.HighWaterMark,
	})
	if err != nil {
		t.Fatalf("delta: %v", err)
	}
	if users := delta.Tables[sourceUsers]; users.Scanned != 3 || users.Copied != 2 {
		t.Errorf("delta users = %+v, want user a's new email and the latecomer", users)
	}
	if !exists(t, pool, `SELECT 1 FROM auth.users WHERE id = $1 AND email = 'a2@example.com'`, userA) {
		t.Error("the delta did not update the email of a user Go has served")
	}
	if !exists(t, pool, `SELECT 1 FROM auth.users WHERE id = $1`, latecomer) {
		t.Error("the delta did not carry the latecomer")
	}
	if exists(t, pool, `SELECT 1 FROM auth.users WHERE id = $1`, userB) {
		t.Error("the delta brought back a user who deleted their account in Go")
	}

	// The final delta, with Python stopped. The newcomer deleted their account
	// in Supabase, which took their reel with it; user b's identity is gone
	// from Supabase as well, and so is user a's, whom Go has served.
	exec(t, pool, `DELETE FROM legacy.reels WHERE id = $1`, newcomerReel)
	exec(t, pool, `DELETE FROM legacy.users WHERE id IN ($1, $2, $3)`, userA, userB, newcomer)

	dryFinal, err := New(pool, quiet()).Run(ctx, Options{
		DumpedAt: dumpedNow(), LegacySchema: scratchSchema, Since: delta.HighWaterMark, PropagateDeletes: true,
	})
	if err != nil {
		t.Fatalf("dry final delta: %v", err)
	}
	if dryFinal.Deleted[sourceUsers] != 1 || !exists(t, pool, `SELECT 1 FROM auth.users WHERE id = $1`, newcomer) {
		t.Errorf("a dry final delta deleted = %v and must change nothing", dryFinal.Deleted)
	}

	final, err := New(pool, quiet()).Run(ctx, Options{
		Execute: true, DumpedAt: dumpedNow(), LegacySchema: scratchSchema, Since: delta.HighWaterMark,
		PropagateDeletes: true,
	})
	if err != nil {
		t.Fatalf("final delta: %v", err)
	}
	if final.Deleted[sourceUsers] != 1 || final.Deleted[sourceReels] != 1 {
		t.Errorf("deleted = %v, want the newcomer and the newcomer's save", final.Deleted)
	}
	if exists(t, pool, `SELECT 1 FROM auth.users WHERE id = $1`, newcomer) {
		t.Error("the newcomer, gone from Supabase, survived the final delta")
	}
	if !exists(t, pool, `SELECT 1 FROM auth.users WHERE id = $1`, userA) {
		t.Error("the final delta deleted a user Go has served")
	}
	if !exists(t, pool, `SELECT 1 FROM reelpin.user_migrations WHERE user_id = $1`, userA) {
		t.Error("the final delta took the migration record of a user Go has served")
	}

	verified, err = NewVerifier(pool).Verify(ctx, VerifyOptions{LegacySchema: scratchSchema})
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	if !verified.OK() {
		t.Errorf("the final delta does not verify: %+v", verified)
	}
}

// Off Supabase the users table is auth.users' only source. A run without it
// would skip every user-owned row as an unknown reference and still finish.
func TestAMissingUsersTableStopsTheRunAndTheVerify(t *testing.T) {
	pool := testPool(t)
	exec(t, pool, scratchLegacySchema)
	exec(t, pool, `DROP TABLE legacy.users`)
	ctx := context.Background()

	if _, err := New(pool, quiet()).Run(ctx, Options{LegacySchema: scratchSchema}); err == nil {
		t.Error("a dry run without legacy users succeeded")
	}
	if _, err := New(pool, quiet()).Run(ctx, Options{
		Execute: true, DumpedAt: dumpedNow(), LegacySchema: scratchSchema,
	}); err == nil {
		t.Error("a real run without legacy users succeeded")
	}
	if _, err := NewVerifier(pool).Verify(ctx, VerifyOptions{LegacySchema: scratchSchema}); err == nil {
		t.Error("a verify without legacy users succeeded")
	}
}

// A user who deleted their account in Go is left alone by every run, as one
// Go has served is. The deletion cascaded their migration record away, so
// only the deletion request says so, and without it the backfill would try to
// carry their legacy rows onto an auth.users row that no longer exists.
func TestAUserDeletedInGoIsLeftAlone(t *testing.T) {
	pool := testPool(t)
	exec(t, pool, scratchLegacySchema)
	ctx := context.Background()

	exec(t, pool, `
		INSERT INTO legacy.reels (id, user_id, url, title, created_at)
		VALUES ($1, $2, 'https://www.instagram.com/reel/KEPT9/', 'Still on the old app', '2026-01-01T00:00:00Z'),
		       ($3, $4, 'https://www.instagram.com/reel/GONE9/', 'Saved before the delete', '2026-01-01T00:00:00Z')`,
		olderLegacyReel, userA, placeReel, userB)
	exec(t, pool, `
		INSERT INTO legacy.user_entitlements (user_id, pro_expires_at, store)
		VALUES ($1, '2027-01-01T00:00:00Z', 'manual')`, userB)
	if _, err := lifecycle.New(pool, nil, nil, quiet()).DeleteAccount(ctx, userB); err != nil {
		t.Fatalf("deleting user b's account: %v", err)
	}

	assertNothingOfUserB := func(run string) {
		t.Helper()
		if exists(t, pool, `SELECT 1 FROM auth.users WHERE id = $1`, userB) {
			t.Errorf("the %s brought user b back", run)
		}
		for _, table := range []string{"reelpin.user_saves", "reelpin.user_entitlements"} {
			if exists(t, pool, `SELECT 1 FROM `+table+` WHERE user_id = $1`, userB) {
				t.Errorf("the %s wrote %s for user b", run, table)
			}
		}
		if exists(t, pool, `SELECT 1 FROM reelpin.backfill_audit WHERE source_id IN ($1, $2)`, placeReel, lateReel) {
			t.Errorf("the %s audited a reel of user b", run)
		}
	}

	snapshot, err := New(pool, quiet()).Run(ctx, Options{Execute: true, DumpedAt: dumpedNow(), LegacySchema: scratchSchema})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if !exists(t, pool, `SELECT 1 FROM reelpin.user_saves WHERE id = $1 AND user_id = $2`, olderLegacyReel, userA) {
		t.Error("the snapshot did not carry user a's reel")
	}
	assertNothingOfUserB("snapshot")

	// The stale install keeps writing to Python after the delete.
	exec(t, pool, `
		INSERT INTO legacy.reels (id, user_id, url, title, created_at)
		VALUES ($1, $2, 'https://www.instagram.com/reel/LATE9/', 'Saved after the delete', now())`, lateReel, userB)

	if _, err := New(pool, quiet()).Run(ctx, Options{
		Execute: true, DumpedAt: dumpedNow(), LegacySchema: scratchSchema, Since: snapshot.HighWaterMark,
	}); err != nil {
		t.Fatalf("delta: %v", err)
	}
	assertNothingOfUserB("delta")

	if _, err := New(pool, quiet()).Run(ctx, Options{Execute: true, DumpedAt: dumpedNow(), LegacySchema: scratchSchema}); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	assertNothingOfUserB("rerun")

	verified, err := NewVerifier(pool).Verify(ctx, VerifyOptions{LegacySchema: scratchSchema})
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	if !verified.OK() {
		t.Errorf("a user deleted in Go fails the verify: %+v", verified)
	}
}
