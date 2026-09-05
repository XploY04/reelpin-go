//go:build integration

package collections

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

const legacySchema = `
CREATE TABLE public.reels (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id TEXT NOT NULL,
    url TEXT NOT NULL,
    normalized_url TEXT,
    source_platform TEXT,
    source_content_type TEXT,
    source_content_id TEXT,
    processing_version TEXT,
    ingestion_method TEXT,
    transcript_source TEXT,
    thumbnail_url TEXT,
    title TEXT NOT NULL DEFAULT 'Untitled',
    summary TEXT DEFAULT '',
    transcript TEXT DEFAULT '',
    category TEXT DEFAULT 'Other',
    subcategory TEXT DEFAULT 'Other',
    secondary_categories JSONB DEFAULT '[]'::jsonb,
    key_facts JSONB DEFAULT '[]'::jsonb,
    locations JSONB DEFAULT '[]'::jsonb,
    people_mentioned JSONB DEFAULT '[]'::jsonb,
    actionable_items JSONB DEFAULT '[]'::jsonb,
    events JSONB NOT NULL DEFAULT '[]'::jsonb,
    parse_status TEXT NOT NULL DEFAULT 'parsed',
    created_at TIMESTAMPTZ DEFAULT now()
);
CREATE TABLE public.profiles (
    id UUID PRIMARY KEY,
    display_name TEXT
);
CREATE TABLE public.collections (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id TEXT NOT NULL,
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    cover_reel_id UUID REFERENCES public.reels(id) ON DELETE SET NULL,
    visibility TEXT NOT NULL DEFAULT 'private',
    link_token_hash TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE public.collection_items (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    collection_id UUID NOT NULL REFERENCES public.collections(id) ON DELETE CASCADE,
    reel_id UUID NOT NULL REFERENCES public.reels(id) ON DELETE CASCADE,
    added_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (collection_id, reel_id)
);
CREATE TABLE public.collection_members (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    collection_id UUID NOT NULL REFERENCES public.collections(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL,
    role TEXT NOT NULL DEFAULT 'viewer',
    invited_by TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (collection_id, user_id)
);
CREATE TABLE public.collection_invites (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    collection_id UUID NOT NULL REFERENCES public.collections(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL,
    role TEXT NOT NULL DEFAULT 'viewer',
    created_by TEXT NOT NULL,
    expires_at TIMESTAMPTZ,
    max_uses INT,
    uses INT NOT NULL DEFAULT 0,
    revoked BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
`

const (
	owner    = "11111111-1111-4111-8111-111111111111"
	editor   = "22222222-2222-4222-8222-222222222222"
	viewer   = "33333333-3333-4333-8333-333333333333"
	outsider = "44444444-4444-4444-8444-444444444444"
)

func testService(t *testing.T) (*Service, *pgxpool.Pool) {
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

	name := "reelpin_collections_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
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

	parsed, _ := url.Parse(adminURL)
	parsed.Path = "/" + name
	pool, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		t.Fatalf("connecting: %v", err)
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

	if _, err := pool.Exec(ctx, legacySchema); err != nil {
		t.Fatalf("legacy schema: %v", err)
	}
	if _, err := migrations.Up(ctx, parsed.String()); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	return New(pool, "https://reelpin.in", func() time.Time { return time.Now() }), pool
}

func seedReel(t *testing.T, pool *pgxpool.Pool, userID, title string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO public.reels (user_id, url, title) VALUES ($1, $2, $3) RETURNING id::text`,
		userID, "https://example.com/"+title, title).Scan(&id); err != nil {
		t.Fatalf("seeding a reel: %v", err)
	}
	return id
}

func TestRolesDecideWhatIsPossible(t *testing.T) {
	service, pool := testService(t)
	ctx := context.Background()

	collection, _, err := service.Create(ctx, owner, "Goa", "trip", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO public.collection_members (collection_id, user_id, role) VALUES ($1,$2,'editor'),($1,$3,'viewer')`,
		collection.ID, editor, viewer); err != nil {
		t.Fatalf("seeding members: %v", err)
	}

	reelID := seedReel(t, pool, owner, "cafes")

	// An editor may add.
	if added, err := service.AddItems(ctx, collection.ID, editor, []string{reelID}); err != nil || added != 1 {
		t.Fatalf("editor add = %d (%v)", added, err)
	}
	// A viewer may not.
	if _, err := service.AddItems(ctx, collection.ID, viewer, []string{reelID}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("viewer add = %v, want ErrForbidden", err)
	}
	// An outsider cannot even learn it exists.
	if _, err := service.Get(ctx, collection.ID, outsider); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider get = %v, want ErrNotFound", err)
	}
	if _, err := service.AddItems(ctx, collection.ID, outsider, []string{reelID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider add = %v, want ErrNotFound, not a permission hint", err)
	}
	// Only the owner may delete or rename.
	if _, err := service.Update(ctx, collection.ID, editor, stringPtr("Renamed"), nil, nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("editor update = %v, want ErrForbidden", err)
	}
	if err := service.Delete(ctx, collection.ID, editor); !errors.Is(err, ErrForbidden) {
		t.Fatalf("editor delete = %v, want ErrForbidden", err)
	}
}

func TestAddingIsIdempotentAndCounted(t *testing.T) {
	service, pool := testService(t)
	ctx := context.Background()

	collection, _, _ := service.Create(ctx, owner, "Goa", "", nil)
	first := seedReel(t, pool, owner, "one")
	second := seedReel(t, pool, owner, "two")

	added, err := service.AddItems(ctx, collection.ID, owner, []string{first, second, first, "not-a-uuid",
		"99999999-9999-4999-8999-999999999999"})
	if err != nil {
		t.Fatalf("AddItems: %v", err)
	}
	if added != 2 {
		t.Fatalf("added = %d, want the two real reels", added)
	}

	// Adding the same reels again changes nothing.
	if again, err := service.AddItems(ctx, collection.ID, owner, []string{first, second}); err != nil || again != 0 {
		t.Fatalf("second add = %d (%v), want nothing", again, err)
	}

	loaded, err := service.Get(ctx, collection.ID, owner)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if loaded.ItemCount != 2 {
		t.Errorf("item_count = %d, want 2", loaded.ItemCount)
	}

	// One notification event per actual change, not per request.
	var events int
	pool.QueryRow(ctx, `SELECT count(*) FROM reelpin.outbox_events WHERE event_type = 'collection.items_added'`).Scan(&events)
	if events != 1 {
		t.Errorf("events = %d, want one for the one change", events)
	}
}

func TestShareLinkIsACapability(t *testing.T) {
	service, pool := testService(t)
	ctx := context.Background()

	collection, _, _ := service.Create(ctx, owner, "Goa", "", nil)
	reelID := seedReel(t, pool, owner, "cafes")
	service.AddItems(ctx, collection.ID, owner, []string{reelID})

	_, token, err := service.EnableLink(ctx, collection.ID, owner)
	if err != nil {
		t.Fatalf("EnableLink: %v", err)
	}

	// Only the hash is stored.
	var stored string
	pool.QueryRow(ctx, `SELECT link_token_hash FROM public.collections WHERE id = $1`, collection.ID).Scan(&stored)
	if stored == token {
		t.Fatal("the raw link token was stored")
	}

	shared, err := service.SharedByToken(ctx, token, 0, 25, time.Now())
	if err != nil {
		t.Fatalf("SharedByToken: %v", err)
	}
	if len(shared.Reels) != 1 {
		t.Errorf("shared reels = %d", len(shared.Reels))
	}
	if shared.Collection.OwnerID != "" || shared.Collection.MemberCount != 0 {
		t.Error("the anonymous view exposes the owner or the member count")
	}

	if _, err := service.SharedByToken(ctx, "guessed", 0, 25, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Errorf("a wrong token gave %v, want ErrNotFound", err)
	}

	// Disabling revokes it immediately.
	if err := service.DisableLink(ctx, collection.ID, owner); err != nil {
		t.Fatalf("DisableLink: %v", err)
	}
	if _, err := service.SharedByToken(ctx, token, 0, 25, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Errorf("a revoked link still works: %v", err)
	}
}

func TestInvitesGrantMembershipOnce(t *testing.T) {
	service, pool := testService(t)
	ctx := context.Background()

	collection, _, _ := service.Create(ctx, owner, "Goa", "", nil)
	_, token, role, expiresAt, err := service.CreateInvite(ctx, collection.ID, owner, "editor")
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	if role != RoleEditor || !expiresAt.After(time.Now()) {
		t.Fatalf("invite role=%s expires=%s", role, expiresAt)
	}

	var stored string
	pool.QueryRow(ctx, `SELECT token_hash FROM public.collection_invites`).Scan(&stored)
	if stored == token {
		t.Fatal("the raw invite token was stored")
	}

	if _, err := service.AcceptInvite(ctx, token, editor); err != nil {
		t.Fatalf("AcceptInvite: %v", err)
	}
	if role, err := service.Role(ctx, collection.ID, editor); err != nil || role != RoleEditor {
		t.Fatalf("role after accepting = %q (%v)", role, err)
	}

	// Accepting twice leaves one membership and spends one use.
	if _, err := service.AcceptInvite(ctx, token, editor); err != nil {
		t.Fatalf("second accept: %v", err)
	}
	var members, uses int
	pool.QueryRow(ctx, `SELECT count(*) FROM public.collection_members`).Scan(&members)
	pool.QueryRow(ctx, `SELECT uses FROM public.collection_invites`).Scan(&uses)
	if members != 1 || uses != 1 {
		t.Fatalf("members=%d uses=%d, want one of each", members, uses)
	}

	// An expired invite is refused.
	if _, err := pool.Exec(ctx, `UPDATE public.collection_invites SET expires_at = now() - interval '1 day'`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AcceptInvite(ctx, token, viewer); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("expired invite = %v, want ErrInviteInvalid", err)
	}

	// A revoked one too.
	if _, err := pool.Exec(ctx, `UPDATE public.collection_invites SET expires_at = now() + interval '1 day', revoked = true`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AcceptInvite(ctx, token, viewer); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("revoked invite = %v, want ErrInviteInvalid", err)
	}
	if _, err := service.AcceptInvite(ctx, "never-existed", viewer); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("unknown invite = %v, want ErrInviteInvalid", err)
	}
}

func TestLeavingAndRemoving(t *testing.T) {
	service, pool := testService(t)
	ctx := context.Background()

	collection, _, _ := service.Create(ctx, owner, "Goa", "", nil)
	pool.Exec(ctx, `INSERT INTO public.collection_members (collection_id, user_id, role) VALUES ($1,$2,'editor'),($1,$3,'viewer')`,
		collection.ID, editor, viewer)

	// An owner cannot leave their own collection.
	if err := service.Leave(ctx, collection.ID, owner); !errors.Is(err, ErrForbidden) {
		t.Fatalf("owner leave = %v, want ErrForbidden", err)
	}
	if err := service.Leave(ctx, collection.ID, viewer); err != nil {
		t.Fatalf("viewer leave: %v", err)
	}
	if _, err := service.Get(ctx, collection.ID, viewer); !errors.Is(err, ErrNotFound) {
		t.Error("a member who left can still see the collection")
	}

	// Only the owner may remove someone else.
	if err := service.RemoveMember(ctx, collection.ID, editor, editor); !errors.Is(err, ErrForbidden) {
		t.Fatalf("editor removing = %v, want ErrForbidden", err)
	}
	if err := service.RemoveMember(ctx, collection.ID, owner, editor); err != nil {
		t.Fatalf("owner removing: %v", err)
	}
	_, members, err := service.Members(ctx, collection.ID, owner)
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(members) != 0 {
		t.Errorf("members = %+v, want none left", members)
	}
}

func TestFilingSkipsTargetsTheUserCannotEdit(t *testing.T) {
	service, pool := testService(t)
	ctx := context.Background()

	own, _, _ := service.Create(ctx, owner, "Mine", "", nil)
	theirs, _, _ := service.Create(ctx, editor, "Theirs", "", nil)
	shared, _, _ := service.Create(ctx, editor, "Shared", "", nil)
	pool.Exec(ctx, `INSERT INTO public.collection_members (collection_id, user_id, role) VALUES ($1,$2,'editor')`,
		shared.ID, owner)
	readOnly, _, _ := service.Create(ctx, editor, "Read only", "", nil)
	pool.Exec(ctx, `INSERT INTO public.collection_members (collection_id, user_id, role) VALUES ($1,$2,'viewer')`,
		readOnly.ID, owner)

	reelID := seedReel(t, pool, owner, "cafes")

	filed, err := service.FileReel(ctx, owner, reelID, []string{
		own.ID, shared.ID, readOnly.ID, theirs.ID, "99999999-9999-4999-8999-999999999999",
	})
	if err != nil {
		t.Fatalf("FileReel: %v", err)
	}
	if len(filed) != 2 {
		t.Fatalf("filed into %v, want only the owned and editable ones", filed)
	}

	// Filing twice files nothing new: it must never duplicate an item.
	again, err := service.FileReel(ctx, owner, reelID, []string{own.ID, shared.ID})
	if err != nil {
		t.Fatalf("second FileReel: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("filed again into %v", again)
	}
}

func TestDetailPaginatesAndReportsEditability(t *testing.T) {
	service, pool := testService(t)
	ctx := context.Background()

	collection, _, _ := service.Create(ctx, owner, "Goa", "", nil)
	ids := []string{}
	for i := 0; i < 5; i++ {
		ids = append(ids, seedReel(t, pool, owner, string(rune('a'+i))))
	}
	if _, err := service.AddItems(ctx, collection.ID, owner, ids); err != nil {
		t.Fatalf("AddItems: %v", err)
	}
	pool.Exec(ctx, `INSERT INTO public.collection_members (collection_id, user_id, role) VALUES ($1,$2,'viewer')`,
		collection.ID, viewer)
	pool.Exec(ctx, `INSERT INTO public.profiles (id, display_name) VALUES ($1::uuid, 'The Owner')`, owner)

	page, err := service.Detail(ctx, collection.ID, owner, 0, 2, time.Now())
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}
	if len(page.Reels) != 2 || !page.Pagination.HasMore {
		t.Fatalf("page = %d reels, has_more=%v", len(page.Reels), page.Pagination.HasMore)
	}
	if page.Pagination.NextOffset == nil || *page.Pagination.NextOffset != 2 {
		t.Errorf("next_offset = %v", page.Pagination.NextOffset)
	}
	if !page.CanEdit {
		t.Error("the owner cannot edit their own collection")
	}
	if page.OwnerName == nil || *page.OwnerName != "The Owner" {
		t.Errorf("owner name = %v, want it resolved once", page.OwnerName)
	}

	viewerPage, err := service.Detail(ctx, collection.ID, viewer, 0, 25, time.Now())
	if err != nil {
		t.Fatalf("viewer Detail: %v", err)
	}
	if viewerPage.CanEdit {
		t.Error("a viewer was told they can edit")
	}
}

func stringPtr(value string) *string { return &value }
