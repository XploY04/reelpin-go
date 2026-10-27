//go:build integration

package backfill

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/collections"
	"github.com/XploY04/reelpin-go/internal/postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const legacyCollections = `
CREATE TABLE public.collections (
 id UUID PRIMARY KEY, owner_id TEXT NOT NULL, name TEXT NOT NULL,
 description TEXT NOT NULL DEFAULT '', cover_reel_id UUID,
 visibility TEXT NOT NULL DEFAULT 'private', link_token_hash TEXT,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE public.collection_members (
 id UUID PRIMARY KEY, collection_id UUID NOT NULL REFERENCES public.collections ON DELETE CASCADE,
 user_id TEXT NOT NULL, role TEXT NOT NULL, invited_by TEXT, created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 UNIQUE(collection_id,user_id)
);
CREATE TABLE public.collection_items (
 id UUID PRIMARY KEY, collection_id UUID NOT NULL REFERENCES public.collections ON DELETE CASCADE,
 reel_id UUID NOT NULL REFERENCES public.reels ON DELETE CASCADE, added_by TEXT NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(), UNIQUE(collection_id,reel_id)
);
CREATE TABLE public.collection_invites (
 id UUID PRIMARY KEY, collection_id UUID NOT NULL REFERENCES public.collections ON DELETE CASCADE,
 token_hash TEXT NOT NULL, role TEXT NOT NULL, created_by TEXT NOT NULL, expires_at TIMESTAMPTZ,
 max_uses INTEGER, uses INTEGER NOT NULL DEFAULT 0, revoked BOOLEAN NOT NULL DEFAULT false,
 created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);`

const (
	legacyCollection = "12121212-0000-4000-8000-000000000001"
	legacyMember     = "12121212-0000-4000-8000-000000000002"
	legacyItem       = "12121212-0000-4000-8000-000000000003"
	legacyInvite     = "12121212-0000-4000-8000-000000000004"
)

func collectionFixture(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testPool(t)
	seed(t, pool)
	exec(t, pool, legacyCollections)
	exec(t, pool, `INSERT INTO public.collections(id,owner_id,name,description,cover_reel_id,visibility,link_token_hash,created_at,updated_at)
 VALUES($1,$2,'A collection','Description',$3,'link',$4,'2026-01-01','2026-01-02')`, legacyCollection, userA, sharedReelB, collections.HashToken("existing-link"))
	exec(t, pool, `INSERT INTO public.collection_members(id,collection_id,user_id,role,invited_by,created_at)
 VALUES($1,$2,$3,'editor',$4,'2026-01-03')`, legacyMember, legacyCollection, userB, userA)
	exec(t, pool, `INSERT INTO public.collection_items(id,collection_id,reel_id,added_by,created_at)
 VALUES($1,$2,$3,$4,'2026-01-04')`, legacyItem, legacyCollection, sharedReelB, userB)
	exec(t, pool, `INSERT INTO public.collection_invites(id,collection_id,token_hash,role,created_by,expires_at,max_uses,uses,revoked,created_at)
 VALUES($1,$2,$3,'editor',$4,'2026-01-05',2,2,true,'2026-01-03')`, legacyInvite, legacyCollection, collections.HashToken("expired-invite"), userA)
	return pool
}

func copyCollectionsFixture(t *testing.T, pool *pgxpool.Pool, deletes bool) Report {
	t.Helper()
	options := Options{Execute: true, DumpedAt: dumpedNow(), LegacySchema: publicSchema, PropagateDeletes: deletes}
	if deletes {
		since := time.Now().Add(-time.Hour)
		options.Since = &since
	}
	report, err := New(pool, quiet()).Run(context.Background(), options)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	return report
}

func TestCollectionBackfillPreservesCapabilitiesAndEditorSaves(t *testing.T) {
	pool := collectionFixture(t)
	copyCollectionsFixture(t, pool, false)
	if !exists(t, pool, `SELECT 1 FROM reelpin.collections WHERE id=$1 AND owner_id=$2 AND cover_save_id=$3
 AND visibility='link' AND link_token_hash=$4 AND link_expires_at IS NULL AND created_at='2026-01-01' AND updated_at='2026-01-02'`, legacyCollection, userA, sharedReelB, collections.HashToken("existing-link")) {
		t.Fatal("collection identity, capability, cover or timestamps were not preserved")
	}
	if !exists(t, pool, `SELECT 1 FROM reelpin.collection_items WHERE collection_id=$1 AND save_id=$2 AND added_by=$3 AND added_at='2026-01-04'`, legacyCollection, sharedReelB, userB) {
		t.Error("editor's save was not filed under its own owner")
	}
	if !exists(t, pool, `SELECT 1 FROM reelpin.collection_members WHERE collection_id=$1 AND user_id=$2 AND role='editor' AND invited_by=$3 AND created_at='2026-01-03'`, legacyCollection, userB, userA) {
		t.Error("membership was not preserved")
	}
	if !exists(t, pool, `SELECT 1 FROM reelpin.collection_invites WHERE id=$1 AND token_hash=$2 AND expires_at='2026-01-05' AND uses=2 AND max_uses=2 AND revoked`, legacyInvite, collections.HashToken("expired-invite")) {
		t.Error("expired/revoked/exhausted invite was changed")
	}
	reader := postgres.NewCollections(pool, "https://reelpin.in", nil)
	collection, items, err := reader.SharedByToken(context.Background(), "existing-link", collections.ItemQuery{Limit: 25})
	if err != nil || collection.ID != legacyCollection || len(items.Items) != 1 || items.Items[0].Reel.ID != sharedReelB {
		t.Fatalf("existing share capability did not resolve the editor save: %v", err)
	}
	if role, err := reader.Role(context.Background(), legacyCollection, userB); err != nil || role != collections.RoleEditor {
		t.Fatalf("migrated editor lost access: %v", err)
	}
	if _, err := reader.Role(context.Background(), legacyCollection, strangerUser); !errors.Is(err, collections.ErrNotFound) {
		t.Fatal("unrelated account gained access")
	}
	if _, err := reader.AcceptInvite(context.Background(), "expired-invite", userB); !errors.Is(err, collections.ErrInviteInvalid) {
		t.Fatal("expired/revoked/exhausted invite became usable")
	}
	copyCollectionsFixture(t, pool, false)
	report := verified(t, pool)
	if !report.OK() {
		t.Fatalf("idempotent copy failed verification: %+v", report)
	}
	for _, name := range []string{"collections", "collection_members", "collection_items", "collection_invites"} {
		if tableOf(t, report, name).Present != 1 {
			t.Errorf("%s was not verified", name)
		}
	}
	exec(t, pool, `UPDATE reelpin.collections SET visibility='private',link_token_hash=NULL WHERE id=$1`, legacyCollection)
	if verified(t, pool).OK() {
		t.Error("verifier missed changed sharing capability")
	}
}

func TestCollectionBackfillDeltaCarriesMutableRowsAndDeletes(t *testing.T) {
	pool := collectionFixture(t)
	copyCollectionsFixture(t, pool, false)
	if !exists(t, pool, `SELECT 1 FROM reelpin.collections WHERE id=$1`, legacyCollection) {
		t.Fatal("snapshot omitted collection")
	}
	exec(t, pool, `UPDATE public.collection_members SET role='viewer' WHERE id=$1`, legacyMember)
	exec(t, pool, `UPDATE public.collection_invites SET uses=3,revoked=true WHERE id=$1`, legacyInvite)
	exec(t, pool, `UPDATE public.collections SET visibility='private',link_token_hash=NULL WHERE id=$1`, legacyCollection)
	copyCollectionsFixture(t, pool, true)
	if !exists(t, pool, `SELECT 1 FROM reelpin.collection_members WHERE collection_id=$1 AND role='viewer'`, legacyCollection) {
		t.Error("delta missed backdated member change")
	}
	if !exists(t, pool, `SELECT 1 FROM reelpin.collection_invites WHERE id=$1 AND uses=3`, legacyInvite) {
		t.Error("delta missed backdated invite change")
	}
	exec(t, pool, `DELETE FROM public.collection_members WHERE id=$1`, legacyMember)
	exec(t, pool, `DELETE FROM public.collection_items WHERE id=$1`, legacyItem)
	exec(t, pool, `DELETE FROM public.collection_invites WHERE id=$1`, legacyInvite)
	copyCollectionsFixture(t, pool, true)
	for _, table := range []string{"collection_members", "collection_items", "collection_invites"} {
		if exists(t, pool, `SELECT 1 FROM reelpin.`+table+` WHERE collection_id=$1`, legacyCollection) {
			t.Errorf("final delta kept deleted %s", table)
		}
	}
	exec(t, pool, `DELETE FROM public.collections WHERE id=$1`, legacyCollection)
	copyCollectionsFixture(t, pool, true)
	if exists(t, pool, `SELECT 1 FROM reelpin.collections WHERE id=$1`, legacyCollection) {
		t.Error("final delta kept deleted collection")
	}
	if !verified(t, pool).OK() {
		t.Error("deleted aggregate failed verification")
	}
}

func TestCollectionBackfillProtectsServedParticipantsAndCascades(t *testing.T) {
	for _, participant := range []string{"member", "added_by", "invited_by", "created_by", "owner"} {
		t.Run(participant, func(t *testing.T) {
			pool := collectionFixture(t)

			exec(t, pool, `INSERT INTO auth.users(id) VALUES($1);`, strangerUser)
			exec(t, pool, `INSERT INTO public.users(id) VALUES($1)`, strangerUser)
			switch participant {
			case "owner":
				exec(t, pool, `UPDATE public.collections SET owner_id=$1`, strangerUser)
			case "member":
				exec(t, pool, `UPDATE public.collection_members SET user_id=$1`, strangerUser)
			case "added_by":
				exec(t, pool, `UPDATE public.collection_items SET added_by=$1`, strangerUser)
			case "invited_by":
				exec(t, pool, `UPDATE public.collection_members SET invited_by=$1`, strangerUser)
			case "created_by":
				exec(t, pool, `UPDATE public.collection_invites SET created_by=$1`, strangerUser)
			}
			copyCollectionsFixture(t, pool, false)
			if !exists(t, pool, `SELECT 1 FROM reelpin.collections WHERE id=$1`, legacyCollection) {
				t.Fatal("snapshot omitted collection")
			}
			exec(t, pool, `INSERT INTO reelpin.user_migrations(user_id) VALUES($1)`, strangerUser)
			exec(t, pool, `UPDATE public.collections SET name='Python changed',visibility='private',link_token_hash=NULL WHERE id=$1`, legacyCollection)
			exec(t, pool, `UPDATE public.collection_invites SET revoked=false,uses=0 WHERE id=$1`, legacyInvite)
			copyCollectionsFixture(t, pool, true)
			if !exists(t, pool, `SELECT 1 FROM reelpin.collections WHERE id=$1 AND name='A collection' AND visibility='link' AND link_token_hash=$2`, legacyCollection, collections.HashToken("existing-link")) {
				t.Fatal("delta overwrote a served participant's collection")
			}
			if !exists(t, pool, `SELECT 1 FROM reelpin.collection_invites WHERE id=$1 AND revoked AND uses=2`, legacyInvite) {
				t.Error("delta resurrected invite capability")
			}
			exec(t, pool, `DELETE FROM public.collections WHERE id=$1`, legacyCollection)
			exec(t, pool, `DELETE FROM public.reels WHERE id=$1`, sharedReelB)
			exec(t, pool, `DELETE FROM public.users WHERE id=$1`, userA)
			copyCollectionsFixture(t, pool, true)
			if !exists(t, pool, `SELECT 1 FROM reelpin.collections WHERE id=$1`, legacyCollection) {
				t.Error("final delta cascaded a protected collection away")
			}
			if !exists(t, pool, `SELECT 1 FROM reelpin.collection_items WHERE collection_id=$1`, legacyCollection) {
				t.Error("final delta cascaded protected items away")
			}
			if !exists(t, pool, `SELECT 1 FROM auth.users WHERE id=$1`, userA) {
				t.Error("final delta cascaded a protected user's identity away")
			}
			if !verified(t, pool).OK() {
				t.Error("protected aggregate failed verification")
			}
			if participant == "member" {
				exec(t, pool, `UPDATE reelpin.user_saves SET saved_at=saved_at-interval '1 day' WHERE id=$1`, sharedReelA)
				report := verified(t, pool)
				if report.OK() || !mismatched(report, sharedReelA, "user_saves.saved_at") {
					t.Error("protected dependency accounting hid unrelated reel drift")
				}
			}
		})
	}
}

func TestCollectionBackfillDryRunAndRejectedReferences(t *testing.T) {
	pool := collectionFixture(t)
	options := Options{LegacySchema: publicSchema}
	report, err := New(pool, quiet()).Run(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if report.Tables["collections"].Copied != 1 || exists(t, pool, `SELECT 1 FROM reelpin.collections`) || exists(t, pool, `SELECT 1 FROM reelpin.backfill_audit WHERE source_table='collections'`) {
		t.Fatal("dry run did not predict the copy without writing it")
	}
	exec(t, pool, `DELETE FROM reelpin.collections; DELETE FROM reelpin.backfill_audit WHERE source_table IN ('collections','collection_members','collection_items','collection_invites')`)
	exec(t, pool, `UPDATE public.collection_items SET added_by='not-a-user' WHERE id=$1`, legacyItem)
	report = copyCollectionsFixture(t, pool, false)
	if report.Tables["collections"].Skipped != 1 || exists(t, pool, `SELECT 1 FROM reelpin.collections`) {
		t.Fatal("invalid participant produced a partial collection")
	}
	exec(t, pool, `UPDATE public.collection_items SET added_by=$1 WHERE id=$2`, userB, legacyItem)
	copyCollectionsFixture(t, pool, false)
	if !exists(t, pool, `SELECT 1 FROM reelpin.collections WHERE id=$1`, legacyCollection) {
		t.Fatal("corrected reference was not retried")
	}
	exec(t, pool, `DELETE FROM reelpin.collections; DELETE FROM reelpin.backfill_audit WHERE source_table IN ('collections','collection_members','collection_items','collection_invites')`)
	exec(t, pool, `UPDATE public.collection_items SET reel_id=$1 WHERE id=$2`, brokenReel, legacyItem)
	report = copyCollectionsFixture(t, pool, false)
	if report.Tables["collections"].Skipped != 1 || exists(t, pool, `SELECT 1 FROM reelpin.collections`) {
		t.Error("unmapped save produced a partial collection")
	}
}

func TestCollectionBackfillResolvesMergedAliasesAndTrackedDeletes(t *testing.T) {
	pool := collectionFixture(t)
	const alias = "ffffffff-0000-4000-8000-000000000005"
	const aliasItem = "12121212-0000-4000-8000-000000000006"
	exec(t, pool, `INSERT INTO public.reels(id,user_id,url,created_at) SELECT $1,user_id,url,created_at+interval '1 second' FROM public.reels WHERE id=$2`, alias, sharedReelB)
	exec(t, pool, `INSERT INTO public.collection_items(id,collection_id,reel_id,added_by,created_at) VALUES($1,$2,$3,$4,'2026-01-05')`, aliasItem, legacyCollection, alias, userB)
	copyCollectionsFixture(t, pool, false)
	if !exists(t, pool, `SELECT 1 FROM reelpin.collection_items WHERE collection_id=$1 AND save_id=$2 AND added_at='2026-01-05'`, legacyCollection, sharedReelB) {
		t.Fatal("merged reel did not resolve to the same user's surviving save")
	}
	if !verified(t, pool).OK() {
		t.Fatal("merged aliases failed verification")
	}
	exec(t, pool, `DELETE FROM public.collection_items WHERE id=$1`, legacyItem)
	copyCollectionsFixture(t, pool, true)
	if !exists(t, pool, `SELECT 1 FROM reelpin.collection_items WHERE collection_id=$1 AND save_id=$2`, legacyCollection, sharedReelB) {
		t.Fatal("deleting one alias deleted the remaining item")
	}
	exec(t, pool, `DELETE FROM public.collection_items WHERE id=$1`, aliasItem)
	copyCollectionsFixture(t, pool, true)
	if exists(t, pool, `SELECT 1 FROM reelpin.collection_items WHERE collection_id=$1`, legacyCollection) {
		t.Error("deleting all aliases kept the item")
	}
}

func TestCollectionBackfillHistoricalParticipantPreventsResurrection(t *testing.T) {
	pool := collectionFixture(t)
	copyCollectionsFixture(t, pool, false)
	exec(t, pool, `INSERT INTO reelpin.user_migrations(user_id) VALUES($1)`, userB)
	exec(t, pool, `DELETE FROM public.collection_members; DELETE FROM public.collection_items`)
	exec(t, pool, `DELETE FROM reelpin.collections WHERE id=$1`, legacyCollection)
	copyCollectionsFixture(t, pool, true)
	if exists(t, pool, `SELECT 1 FROM reelpin.collections WHERE id=$1`, legacyCollection) {
		t.Fatal("snapshot resurrected an aggregate after Go removed all current evidence of its served member")
	}
	if tableOf(t, verified(t, pool), "collections").Migrated != 1 {
		t.Error("historically owned aggregate was not excluded from verification")
	}
}

func TestCollectionBackfillVerifierChecksCapabilityAndCompositeFields(t *testing.T) {
	pool := collectionFixture(t)
	copyCollectionsFixture(t, pool, false)
	exec(t, pool, `UPDATE reelpin.collection_members SET role='viewer' WHERE collection_id=$1`, legacyCollection)
	exec(t, pool, `UPDATE reelpin.collection_items SET added_by=$1 WHERE collection_id=$2`, userA, legacyCollection)
	exec(t, pool, `UPDATE reelpin.collection_invites SET expires_at=now()+interval '1 day',revoked=false,uses=0 WHERE id=$1`, legacyInvite)
	report := verified(t, pool)
	if report.OK() || !mismatched(report, legacyMember, "collection_members.role") || !mismatched(report, legacyItem, "collection_items.added_by") || !mismatched(report, legacyInvite, "collection_invites.expires_at") {
		t.Fatal("verifier missed changed roles, item ownership or invite lifetime")
	}
	exec(t, pool, `DELETE FROM reelpin.collection_members WHERE collection_id=$1`, legacyCollection)
	if tableOf(t, verified(t, pool), "collection_members").CarriedButMissing != 1 {
		t.Error("verifier missed deleted composite-key membership")
	}
}

func TestCollectionBackfillProtectsTransientGoInvitee(t *testing.T) {
	pool := collectionFixture(t)
	exec(t, pool, `DELETE FROM public.collection_members`)
	exec(t, pool, `UPDATE public.collection_items SET added_by=$1,reel_id=$2 WHERE id=$3`, userA, sharedReelA, legacyItem)
	exec(t, pool, `UPDATE public.collections SET cover_reel_id=NULL WHERE id=$1`, legacyCollection)
	copyCollectionsFixture(t, pool, false)
	exec(t, pool, `INSERT INTO reelpin.user_migrations(user_id) VALUES($1)`, userB)
	exec(t, pool, `INSERT INTO reelpin.collection_members(collection_id,user_id,role) VALUES($1,$2,'editor')`, legacyCollection, userB)
	exec(t, pool, `UPDATE reelpin.collection_invites SET uses=uses+1 WHERE id=$1`, legacyInvite)
	exec(t, pool, `DELETE FROM reelpin.collection_items WHERE collection_id=$1`, legacyCollection)
	exec(t, pool, `DELETE FROM reelpin.collection_members WHERE collection_id=$1`, legacyCollection)
	if verified(t, pool).OK() {
		t.Fatal("verifier silently excluded unaudited canonical drift")
	}
	copyCollectionsFixture(t, pool, true)
	if exists(t, pool, `SELECT 1 FROM reelpin.collection_items WHERE collection_id=$1`, legacyCollection) {
		t.Error("delta resurrected transient Go member's removal")
	}
	if !exists(t, pool, `SELECT 1 FROM reelpin.collection_invites WHERE id=$1 AND uses=3`, legacyInvite) {
		t.Error("delta reset transient Go member's invite use")
	}
	if !verified(t, pool).OK() {
		t.Error("explicitly protected aggregate did not verify")
	}
}

func TestCollectionBackfillRefreshesFingerprintAfterOwnDeletes(t *testing.T) {
	pool := collectionFixture(t)
	copyCollectionsFixture(t, pool, false)
	exec(t, pool, `DELETE FROM public.collection_items WHERE id=$1`, legacyItem)
	copyCollectionsFixture(t, pool, true)
	exec(t, pool, `UPDATE public.collections SET name='Python changed' WHERE id=$1`, legacyCollection)
	copyCollectionsFixture(t, pool, true)
	if !exists(t, pool, `SELECT 1 FROM reelpin.collections WHERE id=$1 AND name='Python changed'`, legacyCollection) {
		t.Error("maintenance mistook its own delete for Go ownership")
	}
}

func TestCollectionBackfillFreshDryRunMatchesReferenceEligibility(t *testing.T) {
	for _, scenario := range []string{"valid", "invalid_url", "skipped_reel", "unknown_owner", "blocklisted"} {
		t.Run(scenario, func(t *testing.T) {
			pool := collectionFixture(t)
			switch scenario {
			case "invalid_url":
				exec(t, pool, `UPDATE public.reels SET url='ftp://example.com/unsupported',normalized_url=NULL WHERE id=$1`, sharedReelB)
			case "skipped_reel":
				exec(t, pool, `UPDATE public.collection_items SET reel_id=$1`, brokenReel)
			case "unknown_owner":
				exec(t, pool, `UPDATE public.collections SET owner_id=$1`, strangerUser)
			case "blocklisted":
				exec(t, pool, `INSERT INTO reelpin.source_blocklist(source_platform,source_content_type,source_content_id,reason,blocked_by) VALUES('instagram','reel','SHARED1','test','test')`)
			}
			dry, err := New(pool, quiet()).Run(context.Background(), Options{LegacySchema: publicSchema})
			if err != nil {
				t.Fatal(err)
			}
			for _, table := range []string{"collections", "collection_members", "collection_items", "collection_invites", "user_saves"} {
				if exists(t, pool, `SELECT 1 FROM reelpin.`+table) {
					t.Fatalf("dry run wrote %s", table)
				}
			}
			real := copyCollectionsFixture(t, pool, false)
			for _, name := range []string{"collections", "collection_members", "collection_items", "collection_invites"} {
				if dry.Tables[name].Copied != real.Tables[name].Copied || dry.Tables[name].Skipped != real.Tables[name].Skipped {
					t.Errorf("%s dry copied/skipped=%d/%d, execute=%d/%d", name, dry.Tables[name].Copied, dry.Tables[name].Skipped, real.Tables[name].Copied, real.Tables[name].Skipped)
				}
			}
			if scenario == "valid" && real.Tables["collections"].Copied != 1 {
				t.Error("eligible references were not carried")
			}
			if scenario != "valid" && real.Tables["collections"].Skipped != 1 {
				t.Error("ineligible source reference was not audited")
			}
		})
	}
}

func TestCollectionBackfillRejectsUnownedCanonicalID(t *testing.T) {
	pool := collectionFixture(t)
	exec(t, pool, `INSERT INTO reelpin.collections(id,owner_id,name) VALUES($1,$2,'Existing Go data')`, legacyCollection, userB)
	report := copyCollectionsFixture(t, pool, false)
	if report.Tables["collections"].SkippedByAction["skipped_canonical_change"] != 1 || !exists(t, pool, `SELECT 1 FROM reelpin.collections WHERE id=$1 AND owner_id=$2 AND name='Existing Go data'`, legacyCollection, userB) {
		t.Error("backfill overwrote an unaudited canonical ID")
	}
}

func TestCollectionBackfillSerializesConcurrentChildMutations(t *testing.T) {
	pool := collectionFixture(t)
	exec(t, pool, `DELETE FROM public.collection_members`)
	copyCollectionsFixture(t, pool, false)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	watcher, err := pgx.Connect(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close(context.Background())
	blocker, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Release()
	if _, err := blocker.Exec(ctx, `SELECT pg_advisory_lock(198411)`); err != nil {
		t.Fatal(err)
	}
	defer blocker.Exec(context.Background(), `SELECT pg_advisory_unlock(198411)`)
	exec(t, pool, `CREATE FUNCTION public.pause_collection_update() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN PERFORM pg_advisory_xact_lock(198411);RETURN NEW;END $$;
 CREATE TRIGGER pause_collection_update BEFORE UPDATE ON reelpin.collections FOR EACH ROW EXECUTE FUNCTION public.pause_collection_update()`)
	copied := make(chan error, 1)
	go func() {
		_, err := New(pool, quiet()).Run(ctx, Options{Execute: true, DumpedAt: dumpedNow(), LegacySchema: publicSchema})
		copied <- err
	}()
	waitForLocks := func(want int, event string) {
		t.Helper()
		for {
			var count int
			if err := watcher.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND ($1='' OR wait_event=$1)`, event).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count >= want {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal("concurrent collection operation did not reach its expected lock")
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	waitForLocks(1, "advisory")
	marker, err := pgx.Connect(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer marker.Close(context.Background())
	marked := make(chan error, 1)
	go func() {
		_, err := marker.Exec(ctx, `INSERT INTO reelpin.user_migrations(user_id) VALUES($1)`, userB)
		marked <- err
	}()
	for {
		select {
		case err := <-marked:
			t.Fatalf("first marker committed inside collection copy: %v", err)
		default:
		}
		var waiting bool
		if err := watcher.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock')`, marker.PgConn().PID()).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	removed := make(chan error, 1)
	joined := make(chan error, 1)
	go func() {
		_, err := pool.Exec(ctx, `DELETE FROM reelpin.collection_items WHERE collection_id=$1`, legacyCollection)
		removed <- err
	}()
	go func() {
		_, err := pool.Exec(ctx, `INSERT INTO reelpin.collection_members(collection_id,user_id,role) VALUES($1,$2,'editor')`, legacyCollection, userB)
		joined <- err
	}()
	waitForLocks(4, "")
	select {
	case err := <-marked:
		t.Fatalf("first marker committed inside collection copy: %v", err)
	default:
	}
	if _, err := blocker.Exec(ctx, `SELECT pg_advisory_unlock(198411)`); err != nil {
		t.Fatal(err)
	}
	for _, done := range []chan error{copied, removed, joined, marked} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if exists(t, pool, `SELECT 1 FROM reelpin.collection_items WHERE collection_id=$1`, legacyCollection) || !exists(t, pool, `SELECT 1 FROM reelpin.collection_members WHERE collection_id=$1 AND user_id=$2`, legacyCollection, userB) {
		t.Error("concurrent Go child mutations were overwritten")
	}
	copyCollectionsFixture(t, pool, false)
	if exists(t, pool, `SELECT 1 FROM reelpin.collection_items WHERE collection_id=$1`, legacyCollection) || !exists(t, pool, `SELECT 1 FROM reelpin.collection_members WHERE collection_id=$1 AND user_id=$2`, legacyCollection, userB) {
		t.Error("later delta overwrote concurrent Go edits")
	}
	if !exists(t, pool, `SELECT 1 FROM reelpin.backfill_audit WHERE source_table='collections' AND source_id=$1 AND note::jsonb->>'protected'='true'`, legacyCollection) {
		t.Error("participant served after the serialized copy was not protected")
	}
}

func TestCollectionBackfillRetriesCorrectedIncomingReferences(t *testing.T) {
	pool := collectionFixture(t)
	exec(t, pool, `INSERT INTO auth.users(id) VALUES($1)`, strangerUser)
	exec(t, pool, `INSERT INTO public.users(id) VALUES($1)`, strangerUser)
	const extraMember = "12121212-0000-4000-8000-000000000007"
	exec(t, pool, `INSERT INTO public.collection_members(id,collection_id,user_id,role) VALUES($1,$2,$3,'viewer')`, extraMember, legacyCollection, strangerUser)
	copyCollectionsFixture(t, pool, false)
	exec(t, pool, `UPDATE public.collection_items SET added_by='not-a-user' WHERE id=$1`, legacyItem)
	if report := copyCollectionsFixture(t, pool, false); report.Tables["collections"].Skipped != 1 {
		t.Fatal("invalid incoming reference was not rejected")
	}
	exec(t, pool, `UPDATE public.collection_items SET added_by=$1 WHERE id=$2`, userB, legacyItem)
	exec(t, pool, `UPDATE public.collections SET name='Corrected source' WHERE id=$1`, legacyCollection)
	copyCollectionsFixture(t, pool, false)
	if !exists(t, pool, `SELECT 1 FROM reelpin.collections WHERE id=$1 AND name='Corrected source'`, legacyCollection) {
		t.Fatal("historical invalid reference poisoned a corrected source")
	}
	exec(t, pool, `DELETE FROM public.collection_members WHERE id=$1`, extraMember)
	exec(t, pool, `DELETE FROM public.users WHERE id=$1`, strangerUser)
	copyCollectionsFixture(t, pool, true)
	if exists(t, pool, `SELECT 1 FROM auth.users WHERE id=$1`, strangerUser) {
		t.Fatal("unserved departed member was not removed")
	}
	exec(t, pool, `UPDATE public.collections SET name='After member departure' WHERE id=$1`, legacyCollection)
	copyCollectionsFixture(t, pool, true)
	if !exists(t, pool, `SELECT 1 FROM reelpin.collections WHERE id=$1 AND name='After member departure'`, legacyCollection) {
		t.Error("historical deleted participant blocked valid source updates")
	}
	if exists(t, pool, `SELECT 1 FROM auth.users WHERE id=$1`, strangerUser) || !verified(t, pool).OK() {
		t.Error("departed participant was resurrected or verification failed")
	}
}
