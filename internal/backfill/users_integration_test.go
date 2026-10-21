//go:build integration

package backfill

import (
	"context"
	"testing"
)

const (
	newcomer  = "55555555-5555-4555-8555-555555555555"
	latecomer = "66666666-6666-4666-8666-666666666666"

	newcomerReel = "bbbbbbbb-0000-4000-8000-000000000009"
)

// The id, email and created_at exported from Supabase's auth.users, and
// nothing else, as docs/rehearsal.md creates it.
const legacyUsers = `
CREATE TABLE legacy.users (id UUID PRIMARY KEY, email TEXT, created_at TIMESTAMPTZ)`

// A user who exists only in Supabase reaches auth.users before their reel is
// carried, every run upserts every user including one Go has served, and the
// final delta removes the users Supabase no longer has unless Go has served
// them.
func TestUsersArriveFirstAndFollowTheFinalDelta(t *testing.T) {
	pool := testPool(t)
	exec(t, pool, scratchLegacySchema)
	exec(t, pool, legacyUsers)
	ctx := context.Background()

	exec(t, pool, `
		INSERT INTO legacy.users (id, email, created_at)
		VALUES ($1, 'a@example.com', '2025-01-01T00:00:00Z'),
		       ($2, 'b@example.com', '2025-02-01T00:00:00Z'),
		       ($3, 'new@example.com', '2025-06-01T00:00:00Z')`, userA, userB, newcomer)
	exec(t, pool, `
		INSERT INTO legacy.reels (id, user_id, url, title, created_at)
		VALUES ($1, $2, 'https://www.instagram.com/reel/NEWBIE9/', 'Only in Supabase', '2026-01-01T00:00:00Z')`,
		newcomerReel, newcomer)

	dry, err := New(pool, quiet()).Run(ctx, Options{LegacySchema: scratchSchema})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if users := dry.Tables[sourceUsers]; users.Absent || users.Scanned != 3 || users.Copied != 3 {
		t.Errorf("dry run users = %+v, want 3 scanned and 3 copied", users)
	}
	if exists(t, pool, `SELECT 1 FROM auth.users WHERE id = $1`, newcomer) {
		t.Fatal("a dry run wrote a user")
	}

	snapshot, err := New(pool, quiet()).Run(ctx, Options{Execute: true, DumpedAt: dumpedNow(), LegacySchema: scratchSchema})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if !exists(t, pool, `
		SELECT 1 FROM auth.users
		WHERE id = $1 AND email = 'new@example.com' AND created_at = '2025-06-01T00:00:00Z'`, newcomer) {
		t.Error("the user only Supabase had was not carried with their email and created_at")
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

	// User a moves to Go and changes email in Supabase, and a new user signs
	// up there. The verifier sees the newcomer before any delta carries them.
	exec(t, pool, `INSERT INTO reelpin.user_migrations (user_id) VALUES ($1)`, userA)
	exec(t, pool, `UPDATE legacy.users SET email = 'a2@example.com' WHERE id = $1`, userA)
	exec(t, pool, `INSERT INTO legacy.users (id, email) VALUES ($1, 'late@example.com')`, latecomer)

	behind, err := NewVerifier(pool).Verify(ctx, VerifyOptions{LegacySchema: scratchSchema})
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	users := tableOf(t, behind, sourceUsers)
	if behind.OK() || users.Unexplained != 1 || len(users.UnexplainedIDs) != 1 || users.UnexplainedIDs[0] != latecomer {
		t.Errorf("verified users = %+v, want the latecomer unexplained and the run failed", users)
	}

	delta, err := New(pool, quiet()).Run(ctx, Options{
		Execute: true, DumpedAt: dumpedNow(), LegacySchema: scratchSchema, Since: snapshot.HighWaterMark,
	})
	if err != nil {
		t.Fatalf("delta: %v", err)
	}
	if delta.Tables[sourceUsers].Copied != 4 {
		t.Errorf("delta users = %+v, want every user upserted", delta.Tables[sourceUsers])
	}
	if !exists(t, pool, `SELECT 1 FROM auth.users WHERE id = $1 AND email = 'a2@example.com'`, userA) {
		t.Error("the delta did not update the email of a user Go has served")
	}
	if !exists(t, pool, `SELECT 1 FROM auth.users WHERE id = $1`, latecomer) {
		t.Error("the delta did not carry the latecomer")
	}

	// The final delta, with Python stopped. The newcomer deleted their account
	// in Supabase, which took their reel with it; user b is gone too, and so
	// is user a, whom Go has served.
	exec(t, pool, `DELETE FROM legacy.reels WHERE id = $1`, newcomerReel)
	exec(t, pool, `DELETE FROM legacy.users WHERE id IN ($1, $2, $3)`, userA, userB, newcomer)

	dryFinal, err := New(pool, quiet()).Run(ctx, Options{
		DumpedAt: dumpedNow(), LegacySchema: scratchSchema, Since: delta.HighWaterMark, PropagateDeletes: true,
	})
	if err != nil {
		t.Fatalf("dry final delta: %v", err)
	}
	if dryFinal.Deleted[sourceUsers] != 2 || !exists(t, pool, `SELECT 1 FROM auth.users WHERE id = $1`, userB) {
		t.Errorf("a dry final delta deleted = %v and must change nothing", dryFinal.Deleted)
	}

	final, err := New(pool, quiet()).Run(ctx, Options{
		Execute: true, DumpedAt: dumpedNow(), LegacySchema: scratchSchema, Since: delta.HighWaterMark,
		PropagateDeletes: true,
	})
	if err != nil {
		t.Fatalf("final delta: %v", err)
	}
	if final.Deleted[sourceUsers] != 2 || final.Deleted[sourceReels] != 1 {
		t.Errorf("deleted = %v, want user b, the newcomer and the newcomer's save", final.Deleted)
	}
	for _, gone := range []string{userB, newcomer} {
		if exists(t, pool, `SELECT 1 FROM auth.users WHERE id = $1`, gone) {
			t.Errorf("user %s, gone from Supabase, survived the final delta", gone)
		}
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
