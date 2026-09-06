//go:build integration

package postgres

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/collections"
	"github.com/XploY04/reelpin-go/internal/migrations"
	"github.com/XploY04/reelpin-go/internal/reels"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	ownerID      = "11111111-1111-4111-8111-111111111111"
	strangerID   = "22222222-2222-4222-8222-222222222222"
	collaborator = "33333333-3333-4333-8333-333333333333"
)

// collectionPool builds one throwaway database per test with a
// production-shaped auth.users, then migrates it. Cleanup drops WITH (FORCE),
// so a connection left open by a failure cannot hang the package.
func collectionPool(t *testing.T) *Collections {
	t.Helper()
	pool := rawPool(t)

	ctx := context.Background()
	for _, id := range []string{ownerID, strangerID, collaborator} {
		if _, err := pool.Exec(ctx, `INSERT INTO auth.users (id) VALUES ($1)`, id); err != nil {
			t.Fatalf("seeding a user: %v", err)
		}
	}
	return NewCollections(pool, "https://reelpin.in", func() time.Time { return time.Now() })
}

func rawPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	adminURL := os.Getenv("TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer admin.Close()

	name := "reelpin_coll_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
	if len(name) > 60 {
		name = name[:60]
	}
	for _, statement := range []string{
		`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`,
		`CREATE DATABASE ` + name,
	} {
		if _, err := admin.Exec(ctx, statement); err != nil {
			t.Fatalf("preparing %s: %v", name, err)
		}
	}

	parsed, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + name

	pool, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		t.Fatalf("connecting to %s: %v", name, err)
	}
	t.Cleanup(func() {
		pool.Close()
		cleanup, err := pgxpool.New(context.Background(), adminURL)
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
	})

	if _, err := pool.Exec(ctx, `
		CREATE SCHEMA auth;
		CREATE TABLE auth.users (
			id         UUID PRIMARY KEY,
			email      TEXT,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		t.Fatalf("creating the production-shaped auth schema: %v", err)
	}
	if _, err := migrations.Up(ctx, parsed.String()); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	return pool
}

// seedSave creates one content, one version and one user's save of it, and
// returns the save id, which is the public reel id.
func seedSave(t *testing.T, c *Collections, userID, sourceID string) string {
	t.Helper()
	ctx := context.Background()

	var contentID, versionID string
	if err := c.pool.QueryRow(ctx, `
		INSERT INTO reelpin.contents
			(source_platform, source_content_type, source_content_id,
			 normalized_url, normalized_url_hash, access_scope_hash)
		VALUES ('instagram', 'reel', $1, 'https://www.instagram.com/reel/'||$1||'/', $1, 'public')
		RETURNING id::text`, sourceID).Scan(&contentID); err != nil {
		t.Fatalf("seeding content: %v", err)
	}
	if err := c.pool.QueryRow(ctx, `
		INSERT INTO reelpin.content_versions
			(content_id, processor_version, prompt_version, schema_version, model_version, title, summary, media)
		VALUES ($1, 'v1', 'p1', 's1', 'm1', 'A title', 'A summary', '{"thumbnail_url":"https://cdn/x.jpg"}'::jsonb)
		RETURNING id::text`, contentID).Scan(&versionID); err != nil {
		t.Fatalf("seeding a version: %v", err)
	}
	if _, err := c.pool.Exec(ctx,
		`UPDATE reelpin.contents SET current_version_id = $2 WHERE id = $1`, contentID, versionID); err != nil {
		t.Fatalf("pointing at the version: %v", err)
	}

	var saveID string
	if err := c.pool.QueryRow(ctx, `
		INSERT INTO reelpin.user_saves (user_id, content_id) VALUES ($1, $2)
		RETURNING id::text`, userID, contentID).Scan(&saveID); err != nil {
		t.Fatalf("seeding a save: %v", err)
	}
	return saveID
}

func TestOnlyPeopleWithAccessSeeACollection(t *testing.T) {
	c := collectionPool(t)
	ctx := context.Background()

	collection, _, err := c.Create(ctx, ownerID, "Trips", "", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The owner sees it; a stranger gets not-found, never forbidden: a 403
	// would confirm the collection exists.
	if _, err := c.Get(ctx, collection.ID, ownerID); err != nil {
		t.Fatalf("the owner cannot read their own collection: %v", err)
	}
	for _, call := range []struct {
		name string
		run  func() error
	}{
		{"get", func() error { _, err := c.Get(ctx, collection.ID, strangerID); return err }},
		{"detail", func() error { _, err := c.Detail(ctx, collection.ID, strangerID, nil, 25); return err }},
		{"members", func() error { _, _, err := c.Members(ctx, collection.ID, strangerID); return err }},
		{"update", func() error {
			name := "Stolen"
			_, err := c.Update(ctx, collection.ID, strangerID, &name, nil, nil)
			return err
		}},
		{"delete", func() error { return c.Delete(ctx, collection.ID, strangerID) }},
		{"add items", func() error { _, err := c.AddItems(ctx, collection.ID, strangerID, nil); return err }},
		{"remove item", func() error { return c.RemoveItem(ctx, collection.ID, strangerID, collection.ID) }},
		{"enable link", func() error { _, _, _, err := c.EnableLink(ctx, collection.ID, strangerID); return err }},
		{"disable link", func() error { return c.DisableLink(ctx, collection.ID, strangerID) }},
		{"invite", func() error { _, _, _, _, err := c.CreateInvite(ctx, collection.ID, strangerID, "viewer"); return err }},
		{"remove member", func() error { return c.RemoveMember(ctx, collection.ID, strangerID, ownerID) }},
		{"leave", func() error { return c.Leave(ctx, collection.ID, strangerID) }},
	} {
		if err := call.run(); !errors.Is(err, collections.ErrNotFound) {
			t.Errorf("%s for a stranger = %v, want ErrNotFound", call.name, err)
		}
	}

	// And the stranger's own list never contains it.
	list, err := c.List(ctx, strangerID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("a stranger lists %d collections, want none", len(list))
	}
}

func TestFilingIsIdempotent(t *testing.T) {
	c := collectionPool(t)
	ctx := context.Background()

	collection, _, err := c.Create(ctx, ownerID, "Trips", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	saveID := seedSave(t, c, ownerID, "SAVE1")

	first, err := c.AddItems(ctx, collection.ID, ownerID, []string{saveID})
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.AddItems(ctx, collection.ID, ownerID, []string{saveID, saveID})
	if err != nil {
		t.Fatal(err)
	}
	if first != 1 || second != 0 {
		t.Fatalf("added %d then %d, want 1 then 0: a re-add is not an error and not a duplicate", first, second)
	}

	var items int
	if err := c.pool.QueryRow(ctx,
		`SELECT count(*) FROM reelpin.collection_items WHERE collection_id = $1`, collection.ID).Scan(&items); err != nil {
		t.Fatal(err)
	}
	if items != 1 {
		t.Fatalf("rows = %d, want one", items)
	}
}

// TestFileSaveIsIdempotentUnderDuplicateDelivery is the contract Task 8 calls
// inside its enqueue transaction: the same save filed twice by two deliveries
// of one message leaves one row.
func TestFileSaveIsIdempotentUnderDuplicateDelivery(t *testing.T) {
	c := collectionPool(t)
	ctx := context.Background()

	collection, _, err := c.Create(ctx, ownerID, "Trips", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	saveID := seedSave(t, c, ownerID, "SAVE2")

	for delivery := 0; delivery < 2; delivery++ {
		tx, err := c.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		added, fileErr := c.FileSave(ctx, tx, ownerID, collection.ID, []string{saveID})
		if fileErr != nil {
			// Roll back before failing: the per-test database cannot be
			// dropped while this connection holds it.
			tx.Rollback(ctx)
			t.Fatalf("FileSave delivery %d: %v", delivery+1, fileErr)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if delivery == 1 && added != 0 {
			t.Errorf("the second delivery filed %d rows, want none", added)
		}
	}

	var items int
	if err := c.pool.QueryRow(ctx,
		`SELECT count(*) FROM reelpin.collection_items WHERE collection_id = $1`, collection.ID).Scan(&items); err != nil {
		t.Fatal(err)
	}
	if items != 1 {
		t.Fatalf("rows = %d, want one after two deliveries", items)
	}
}

func TestFileSaveSkipsWhatItCannotFile(t *testing.T) {
	c := collectionPool(t)
	ctx := context.Background()

	collection, _, err := c.Create(ctx, ownerID, "Trips", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	mine := seedSave(t, c, ownerID, "MINE")

	tx, err := c.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A stale picker sends a malformed id and one that does not exist. Neither
	// may cost the user the rest of the batch.
	added, fileErr := c.FileSave(ctx, tx, ownerID, collection.ID,
		[]string{"not-a-uuid", "44444444-4444-4444-8444-000000000000", mine})
	if fileErr != nil {
		tx.Rollback(ctx)
		t.Fatalf("FileSave: %v", fileErr)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if added != 1 {
		t.Fatalf("added = %d, want only the real save", added)
	}
}

func TestMembershipIsIdempotentAndRoleBound(t *testing.T) {
	c := collectionPool(t)
	ctx := context.Background()

	collection, _, err := c.Create(ctx, ownerID, "Trips", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, token, _, _, err := c.CreateInvite(ctx, collection.ID, ownerID, collections.RoleViewer)
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}

	for i := 0; i < 2; i++ {
		if _, err := c.AcceptInvite(ctx, token, collaborator); err != nil {
			t.Fatalf("accept %d: %v", i+1, err)
		}
	}

	var members, uses int
	if err := c.pool.QueryRow(ctx,
		`SELECT count(*) FROM reelpin.collection_members WHERE collection_id = $1`, collection.ID).Scan(&members); err != nil {
		t.Fatal(err)
	}
	if err := c.pool.QueryRow(ctx,
		`SELECT uses FROM reelpin.collection_invites WHERE collection_id = $1`, collection.ID).Scan(&uses); err != nil {
		t.Fatal(err)
	}
	if members != 1 || uses != 1 {
		t.Fatalf("members = %d, uses = %d: accepting twice must leave one membership and spend one use", members, uses)
	}

	// A viewer reads but cannot change, and cannot be promoted by asking.
	saveID := seedSave(t, c, ownerID, "VIEW1")
	if _, err := c.AddItems(ctx, collection.ID, collaborator, []string{saveID}); !errors.Is(err, collections.ErrForbidden) {
		t.Errorf("a viewer adding items = %v, want ErrForbidden", err)
	}
	if _, err := c.Detail(ctx, collection.ID, collaborator, nil, 25); err != nil {
		t.Errorf("a viewer cannot read: %v", err)
	}
}

func TestAnEditorChangesItemsButNotTheCollection(t *testing.T) {
	c := collectionPool(t)
	ctx := context.Background()

	collection, _, err := c.Create(ctx, ownerID, "Trips", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, token, _, _, err := c.CreateInvite(ctx, collection.ID, ownerID, collections.RoleEditor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AcceptInvite(ctx, token, collaborator); err != nil {
		t.Fatal(err)
	}

	saveID := seedSave(t, c, collaborator, "EDIT1")
	if _, err := c.AddItems(ctx, collection.ID, collaborator, []string{saveID}); err != nil {
		t.Errorf("an editor cannot add items: %v", err)
	}

	name := "Renamed"
	if _, err := c.Update(ctx, collection.ID, collaborator, &name, nil, nil); !errors.Is(err, collections.ErrForbidden) {
		t.Errorf("an editor renaming = %v, want ErrForbidden", err)
	}
	if err := c.Delete(ctx, collection.ID, collaborator); !errors.Is(err, collections.ErrForbidden) {
		t.Errorf("an editor deleting = %v, want ErrForbidden", err)
	}
	// An editor may remove themselves; the owner may not.
	if err := c.Leave(ctx, collection.ID, collaborator); err != nil {
		t.Errorf("an editor cannot leave: %v", err)
	}
	if err := c.Leave(ctx, collection.ID, ownerID); !errors.Is(err, collections.ErrForbidden) {
		t.Errorf("the owner leaving = %v, want ErrForbidden", err)
	}
}

// TestAShareTokenRevealsNothingWhenItShouldNot covers the three ways a token
// stops working. All three answer identically.
func TestAShareTokenRevealsNothingWhenItShouldNot(t *testing.T) {
	c := collectionPool(t)
	ctx := context.Background()

	collection, _, err := c.Create(ctx, ownerID, "Trips", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	saveID := seedSave(t, c, ownerID, "SHARE1")
	if _, err := c.AddItems(ctx, collection.ID, ownerID, []string{saveID}); err != nil {
		t.Fatal(err)
	}

	_, token, _, err := c.EnableLink(ctx, collection.ID, ownerID)
	if err != nil {
		t.Fatalf("EnableLink: %v", err)
	}

	shared, err := c.SharedByToken(ctx, token, nil, 25)
	if err != nil {
		t.Fatalf("a live token: %v", err)
	}
	if len(shared.Items) != 1 {
		t.Fatalf("items = %d, want the filed save", len(shared.Items))
	}
	// The token grants a view of the saves, not of who can see them.
	if shared.Collection.OwnerID != "" || shared.Collection.MemberCount != 0 {
		t.Errorf("the anonymous view leaks membership: %+v", shared.Collection)
	}

	// Malformed, revoked and expired all answer the same.
	if _, err := c.SharedByToken(ctx, "not-a-token", nil, 25); !errors.Is(err, collections.ErrNotFound) {
		t.Errorf("a malformed token = %v, want ErrNotFound", err)
	}

	if err := c.DisableLink(ctx, collection.ID, ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SharedByToken(ctx, token, nil, 25); !errors.Is(err, collections.ErrNotFound) {
		t.Errorf("a revoked token = %v, want ErrNotFound", err)
	}

	_, fresh, _, err := c.EnableLink(ctx, collection.ID, ownerID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.pool.Exec(ctx, `
		UPDATE reelpin.collections SET link_expires_at = now() - interval '1 hour' WHERE id = $1`,
		collection.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SharedByToken(ctx, fresh, nil, 25); !errors.Is(err, collections.ErrNotFound) {
		t.Errorf("an expired token = %v, want ErrNotFound", err)
	}
}

// TestOnlyTheHashIsStored is the reason a database leak cannot be replayed.
func TestOnlyTheHashIsStored(t *testing.T) {
	c := collectionPool(t)
	ctx := context.Background()

	collection, _, err := c.Create(ctx, ownerID, "Trips", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, linkToken, _, err := c.EnableLink(ctx, collection.ID, ownerID)
	if err != nil {
		t.Fatal(err)
	}
	_, inviteToken, _, _, err := c.CreateInvite(ctx, collection.ID, ownerID, collections.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}

	var storedLink, storedInvite string
	if err := c.pool.QueryRow(ctx,
		`SELECT link_token_hash FROM reelpin.collections WHERE id = $1`, collection.ID).Scan(&storedLink); err != nil {
		t.Fatal(err)
	}
	if err := c.pool.QueryRow(ctx,
		`SELECT token_hash FROM reelpin.collection_invites WHERE collection_id = $1`, collection.ID).Scan(&storedInvite); err != nil {
		t.Fatal(err)
	}
	if storedLink == linkToken || storedInvite == inviteToken {
		t.Fatal("a raw capability token is stored; a database leak would be replayable")
	}
	if storedLink != collections.HashToken(linkToken) || storedInvite != collections.HashToken(inviteToken) {
		t.Fatal("the stored value is not this token's hash")
	}
}

func TestAnExpiredInviteCannotBeAccepted(t *testing.T) {
	c := collectionPool(t)
	ctx := context.Background()

	collection, _, err := c.Create(ctx, ownerID, "Trips", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, token, _, _, err := c.CreateInvite(ctx, collection.ID, ownerID, collections.RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.pool.Exec(ctx, `
		UPDATE reelpin.collection_invites SET expires_at = now() - interval '1 hour'
		WHERE collection_id = $1`, collection.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := c.AcceptInvite(ctx, token, collaborator); !errors.Is(err, collections.ErrInviteInvalid) {
		t.Fatalf("an expired invite = %v, want ErrInviteInvalid", err)
	}
	if _, err := c.AcceptInvite(ctx, "never-issued", collaborator); !errors.Is(err, collections.ErrInviteInvalid) {
		t.Fatalf("an unknown invite = %v, want ErrInviteInvalid", err)
	}
}

func TestItemPagesResumeWithoutRepeating(t *testing.T) {
	c := collectionPool(t)
	ctx := context.Background()

	collection, _, err := c.Create(ctx, ownerID, "Trips", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		saveID := seedSave(t, c, ownerID, "PAGE"+string(rune('A'+i)))
		if _, err := c.AddItems(ctx, collection.ID, ownerID, []string{saveID}); err != nil {
			t.Fatal(err)
		}
	}

	first, err := c.Detail(ctx, collection.ID, ownerID, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 2 || !first.Page.HasMore || first.Page.NextCursor == nil {
		t.Fatalf("first page = %+v", first.Page)
	}

	cursor, err := reels.DecodeCursor(*first.Page.NextCursor)
	if err != nil {
		t.Fatalf("the cursor does not decode: %v", err)
	}
	second, err := c.Detail(ctx, collection.ID, ownerID, &cursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 1 || second.Page.HasMore {
		t.Fatalf("second page = %d items, has_more %v", len(second.Items), second.Page.HasMore)
	}
	for _, shown := range first.Items {
		for _, next := range second.Items {
			if shown.ReelID == next.ReelID {
				t.Fatalf("reel %s appears on both pages", shown.ReelID)
			}
		}
	}
}

func TestDeletingACollectionKeepsTheSaves(t *testing.T) {
	c := collectionPool(t)
	ctx := context.Background()

	collection, _, err := c.Create(ctx, ownerID, "Trips", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	saveID := seedSave(t, c, ownerID, "KEEP1")
	if _, err := c.AddItems(ctx, collection.ID, ownerID, []string{saveID}); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, collection.ID, ownerID); err != nil {
		t.Fatal(err)
	}

	var saves int
	if err := c.pool.QueryRow(ctx,
		`SELECT count(*) FROM reelpin.user_saves WHERE id = $1`, saveID).Scan(&saves); err != nil {
		t.Fatal(err)
	}
	if saves != 1 {
		t.Fatal("deleting a collection deleted the save it held")
	}
}
