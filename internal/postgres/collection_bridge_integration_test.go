//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/XploY04/reelpin-go/internal/collections"
	"github.com/XploY04/reelpin-go/internal/reels"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func bridgeSnapshot(userID, id string) collections.Snapshot {
	return collections.Snapshot{ID: id, UserID: userID, URL: "https://example.com/" + id, Title: "Python article", Summary: "Shared summary", Transcript: "Private source transcript", KeyFacts: []string{"A fact"}, Locations: json.RawMessage(`[]`), PeopleMentioned: json.RawMessage(`["A person"]`), ActionableItems: json.RawMessage(`["Read it"]`), Events: json.RawMessage(`[]`)}
}

func asApplicationRole(t *testing.T, c *Collections) {
	t.Helper()
	cfg := c.pool.Config()
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, `SET ROLE reelpin_app`)
		return err
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	c.pool = pool
}

func TestCollectionBridgeImportsAndSharesThroughApplicationRole(t *testing.T) {
	c := collectionPool(t)
	ctx := context.Background()
	asApplicationRole(t, c)
	id := uuid.NewString()
	snapshot := bridgeSnapshot(ownerID, id)
	collection, added, err := c.CreateImported(ctx, ownerID, "Across backends", "", []string{id}, []collections.Snapshot{snapshot})
	if err != nil || added != 1 {
		t.Fatalf("create/import added=%d err=%v", added, err)
	}
	_, token, _, _, err := c.CreateInvite(ctx, collection.ID, ownerID, collections.RoleEditor)
	if err != nil {
		t.Fatal(err)
	}
	freshUser := uuid.NewString()
	if err = c.EnsureBridgeUser(ctx, freshUser); err != nil {
		t.Fatal(err)
	}
	if _, err = c.AcceptInvite(ctx, token, freshUser); err != nil {
		t.Fatal(err)
	}
	detail, page, err := c.Detail(ctx, collection.ID, freshUser, firstPage)
	if err != nil || detail.Role != collections.RoleEditor || len(page.Items) != 1 {
		t.Fatalf("joined detail=%+v items=%d err=%v", detail, len(page.Items), err)
	}
	record, err := c.VisibleReel(ctx, freshUser, id)
	if err != nil || record.Transcript != "Private source transcript" {
		t.Fatalf("visible reel=%+v err=%v", record, err)
	}
	_, again, err := c.AddImported(ctx, collection.ID, ownerID, []string{id}, []collections.Snapshot{snapshot})
	if err != nil || again != 0 {
		t.Fatalf("idempotent add=%d err=%v", again, err)
	}
	if err = c.RemoveMember(ctx, collection.ID, ownerID, freshUser); err != nil {
		t.Fatal(err)
	}
	if _, err = c.VisibleReel(ctx, freshUser, id); !errors.Is(err, reels.ErrNotFound) {
		t.Fatalf("removed member can read: %v", err)
	}
	if err = c.DeleteImported(ctx, ownerID, id); err != nil {
		t.Fatal(err)
	}
	if _, _, err = c.AddImported(ctx, collection.ID, ownerID, []string{id}, []collections.Snapshot{snapshot}); !errors.Is(err, reels.ErrNotFound) {
		t.Fatalf("deleted source snapshot was reimported: %v", err)
	}
	if _, err = c.VisibleReel(ctx, ownerID, id); !errors.Is(err, reels.ErrNotFound) {
		t.Fatalf("deleted import can read: %v", err)
	}
	var remaining int
	if err = c.pool.QueryRow(ctx, `SELECT count(*) FROM reelpin.contents WHERE access_scope_hash=$1`, "python-collection-bridge:"+ownerID+":"+id).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("private content remained count=%d err=%v", remaining, err)
	}
}

func TestCollectionBridgeImportChecksPermissionAndRollsBackWholeBatch(t *testing.T) {
	c := collectionPool(t)
	ctx := context.Background()
	foreignSave := seedSave(t, c, strangerID, "foreign")
	collection, _, err := c.Create(ctx, ownerID, "Private", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	asApplicationRole(t, c)
	imported := uuid.NewString()
	snap := bridgeSnapshot(strangerID, imported)
	if _, _, err = c.AddImported(ctx, collection.ID, strangerID, []string{imported}, []collections.Snapshot{snap}); !errors.Is(err, collections.ErrNotFound) {
		t.Fatalf("stranger import error=%v", err)
	}
	var n int
	if err = c.pool.QueryRow(ctx, `SELECT count(*) FROM reelpin.user_saves WHERE id=$1`, imported).Scan(&n); err != nil || n != 0 {
		t.Fatalf("unauthorized import exists=%d err=%v", n, err)
	}
	valid := uuid.NewString()
	_, _, err = c.AddImported(ctx, collection.ID, ownerID, []string{valid, foreignSave}, []collections.Snapshot{bridgeSnapshot(ownerID, valid), bridgeSnapshot(ownerID, foreignSave)})
	if !errors.Is(err, reels.ErrNotFound) {
		t.Fatalf("foreign save collision error=%v", err)
	}
	if err = c.pool.QueryRow(ctx, `SELECT count(*) FROM reelpin.user_saves WHERE id=$1`, valid).Scan(&n); err != nil || n != 0 {
		t.Fatalf("partial import remained=%d err=%v", n, err)
	}
	if err = c.DeleteImported(ctx, ownerID, foreignSave); err != nil {
		t.Fatal(err)
	}
	if err = c.pool.QueryRow(ctx, `SELECT count(*) FROM reelpin.user_saves WHERE id=$1`, foreignSave).Scan(&n); err != nil || n != 1 {
		t.Fatalf("other-owner save deleted count=%d err=%v", n, err)
	}
}

func TestCollectionBridgeDeletionUsesPythonProvenance(t *testing.T) {
	c := collectionPool(t)
	ctx := context.Background()
	native := seedFullSave(t, c, ownerID, "native")
	legacy := seedFullSave(t, c, ownerID, "legacy")
	var content string
	if err := c.pool.QueryRow(ctx, `SELECT content_id::text FROM reelpin.user_saves WHERE id=$1`, legacy).Scan(&content); err != nil {
		t.Fatal(err)
	}
	if _, err := c.pool.Exec(ctx, `INSERT INTO reelpin.backfill_audit(backfill_version,batch,source_table,source_id,action,content_id) VALUES('test',1,'reels',$1,'copied',$2)`, legacy, content); err != nil {
		t.Fatal(err)
	}
	asApplicationRole(t, c)
	if err := c.DeleteImported(ctx, ownerID, native); err != nil {
		t.Fatal(err)
	}
	if _, err := c.VisibleReel(ctx, ownerID, native); err != nil {
		t.Fatalf("native save was removed: %v", err)
	}
	if err := c.DeleteImported(ctx, ownerID, legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := c.VisibleReel(ctx, ownerID, legacy); !errors.Is(err, reels.ErrNotFound) {
		t.Fatalf("legacy save survived: %v", err)
	}
	if err := c.DeleteBridgeAccount(ctx, ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.VisibleReel(ctx, ownerID, native); err != nil {
		t.Fatalf("collection cleanup removed unrelated native save: %v", err)
	}
}

func TestCollectionBridgeConcurrentImportIsIdempotent(t *testing.T) {
	c := collectionPool(t)
	ctx := context.Background()
	collection, _, err := c.Create(ctx, ownerID, "Concurrent", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	asApplicationRole(t, c)
	id := uuid.NewString()
	snapshot := bridgeSnapshot(ownerID, id)
	errors := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() {
			_, _, err := c.AddImported(ctx, collection.ID, ownerID, []string{id}, []collections.Snapshot{snapshot})
			errors <- err
		})
	}
	workers.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	_, page, err := c.Detail(ctx, collection.ID, ownerID, firstPage)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("items=%d err=%v", len(page.Items), err)
	}
}

func TestCollectionBridgeFilingRespectsRolesAndPreservesExistingVersions(t *testing.T) {
	c := collectionPool(t)
	ctx := context.Background()
	native := seedFullSave(t, c, ownerID, "unchanged-native")
	collection, _, err := c.Create(ctx, ownerID, "Filing", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, viewerToken, _, _, err := c.CreateInvite(ctx, collection.ID, ownerID, collections.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.AcceptInvite(ctx, viewerToken, collaborator); err != nil {
		t.Fatal(err)
	}
	_, editorToken, _, _, err := c.CreateInvite(ctx, collection.ID, ownerID, collections.RoleEditor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.AcceptInvite(ctx, editorToken, strangerID); err != nil {
		t.Fatal(err)
	}
	asApplicationRole(t, c)
	viewerID := uuid.NewString()
	filed, err := c.FileImported(ctx, collaborator, []string{collection.ID}, []string{viewerID}, []collections.Snapshot{bridgeSnapshot(collaborator, viewerID)})
	if err != nil || len(filed) != 0 {
		t.Fatalf("viewer filed=%v err=%v", filed, err)
	}
	if _, err = c.VisibleReel(ctx, collaborator, viewerID); !errors.Is(err, reels.ErrNotFound) {
		t.Fatalf("viewer import survived: %v", err)
	}
	editorID := uuid.NewString()
	filed, err = c.FileImported(ctx, strangerID, []string{collection.ID}, []string{editorID}, []collections.Snapshot{bridgeSnapshot(strangerID, editorID)})
	if err != nil || len(filed) != 1 {
		t.Fatalf("editor filed=%v err=%v", filed, err)
	}
	if _, _, err = c.AddImported(ctx, collection.ID, ownerID, []string{native}, []collections.Snapshot{bridgeSnapshot(ownerID, native)}); err != nil {
		t.Fatal(err)
	}
	original, err := c.VisibleReel(ctx, ownerID, native)
	if err != nil || original.Title != "Best cafes in Goa" || original.Transcript != "spoken words" {
		t.Fatalf("native version overwritten: %+v err=%v", original, err)
	}
	_, page, err := c.Detail(ctx, collection.ID, ownerID, collections.ItemQuery{Offset: 1, Limit: 1, Transcripts: true})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("offset items=%d err=%v", len(page.Items), err)
	}
	if err = c.DeleteBridgeAccount(ctx, strangerID); err != nil {
		t.Fatal(err)
	}
	if _, err = c.VisibleReel(ctx, ownerID, editorID); !errors.Is(err, reels.ErrNotFound) {
		t.Fatalf("account-deleted imported reel survived: %v", err)
	}
	if _, err = c.Role(ctx, collection.ID, strangerID); !errors.Is(err, collections.ErrBridgeDeleted) {
		t.Fatalf("account-deleted member survived: %v", err)
	}
	if _, err = c.Get(ctx, collection.ID, ownerID); err != nil {
		t.Fatalf("another owner's collection removed: %v", err)
	}
}

func TestCollectionBridgeDeletedActorsCannotBeRecreated(t *testing.T) {
	c := collectionPool(t)
	ctx := context.Background()
	ownerCollection, _, err := c.Create(ctx, ownerID, "Survives", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, token, _, _, err := c.CreateInvite(ctx, ownerCollection.ID, ownerID, collections.RoleEditor)
	if err != nil {
		t.Fatal(err)
	}
	asApplicationRole(t, c)
	if err = c.DeleteBridgeAccount(ctx, strangerID); err != nil {
		t.Fatal(err)
	}
	if err = c.EnsureBridgeUser(ctx, strangerID); !errors.Is(err, collections.ErrBridgeDeleted) {
		t.Fatalf("bridge tombstone did not fence mirror: %v", err)
	}
	if _, _, err = c.Create(ctx, strangerID, "Resurrected", "", nil); !errors.Is(err, collections.ErrBridgeDeleted) {
		t.Fatalf("native create ignored tombstone: %v", err)
	}
	if _, err = c.AcceptInvite(ctx, token, strangerID); !errors.Is(err, collections.ErrBridgeDeleted) {
		t.Fatalf("native accept ignored tombstone: %v", err)
	}
	id := uuid.NewString()
	if _, err = c.FileImported(ctx, strangerID, []string{ownerCollection.ID}, []string{id}, []collections.Snapshot{bridgeSnapshot(strangerID, id)}); !errors.Is(err, collections.ErrBridgeDeleted) {
		t.Fatalf("stale worker ignored tombstone: %v", err)
	}
	if _, err = c.pool.Exec(ctx, `INSERT INTO reelpin.account_deletion_requests(user_id,database_cleanup_state) VALUES($1,'done')`, collaborator); err != nil {
		t.Fatal(err)
	}
	if err = c.EnsureBridgeUser(ctx, collaborator); !errors.Is(err, collections.ErrBridgeDeleted) {
		t.Fatalf("native deletion tombstone ignored: %v", err)
	}
}

func TestCollectionBridgeInviteReportsOnlyNewCommittedMembership(t *testing.T) {
	c := collectionPool(t)
	ctx := context.Background()
	asApplicationRole(t, c)
	collection, _, err := c.Create(ctx, ownerID, "Join notifications", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, token, _, _, err := c.CreateInvite(ctx, collection.ID, ownerID, collections.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	if _, joined, err := c.AcceptBridgeInvite(ctx, token, ownerID); err != nil || joined {
		t.Fatalf("owner joined=%v err=%v", joined, err)
	}
	type result struct {
		joined bool
		err    error
	}
	results := make(chan result, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() {
			_, joined, err := c.AcceptBridgeInvite(ctx, token, collaborator)
			results <- result{joined: joined, err: err}
		})
	}
	workers.Wait()
	close(results)
	joins := 0
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.joined {
			joins++
		}
	}
	if joins != 1 {
		t.Fatalf("concurrent accepts reported %d new joins", joins)
	}
	if _, joined, err := c.AcceptBridgeInvite(ctx, token, collaborator); err != nil || joined {
		t.Fatalf("duplicate joined=%v err=%v", joined, err)
	}
	var uses int
	if err = c.pool.QueryRow(ctx, `SELECT uses FROM reelpin.collection_invites WHERE token_hash=$1`, collections.HashToken(token)).Scan(&uses); err != nil || uses != 1 {
		t.Fatalf("uses=%d err=%v", uses, err)
	}
	if _, err = c.pool.Exec(ctx, `UPDATE reelpin.collection_invites SET revoked=true WHERE token_hash=$1`, collections.HashToken(token)); err != nil {
		t.Fatal(err)
	}
	if _, joined, err := c.AcceptBridgeInvite(ctx, token, strangerID); !errors.Is(err, collections.ErrInviteInvalid) || joined {
		t.Fatalf("revoked joined=%v err=%v", joined, err)
	}
}
