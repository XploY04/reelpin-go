//go:build integration

package lifecycle

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

const legacySchema = `
CREATE TABLE public.reels (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id TEXT NOT NULL,
    url TEXT NOT NULL,
    title TEXT NOT NULL DEFAULT 'Untitled',
    created_at TIMESTAMPTZ DEFAULT now()
);
CREATE TABLE public.processing_jobs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id TEXT NOT NULL,
    environment TEXT NOT NULL DEFAULT 'test',
    url TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'queued',
    result_reel_id UUID,
    created_at TIMESTAMPTZ DEFAULT now()
);
CREATE TABLE public.profiles (id UUID PRIMARY KEY, display_name TEXT);
CREATE TABLE public.collections (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id TEXT NOT NULL,
    name TEXT NOT NULL,
    cover_reel_id UUID REFERENCES public.reels(id) ON DELETE SET NULL,
    visibility TEXT NOT NULL DEFAULT 'private',
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
    UNIQUE (collection_id, user_id)
);
CREATE TABLE public.collection_invites (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    collection_id UUID NOT NULL REFERENCES public.collections(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL,
    role TEXT NOT NULL DEFAULT 'viewer',
    created_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE public.manual_map_pins (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id TEXT NOT NULL, name TEXT NOT NULL,
    latitude DOUBLE PRECISION NOT NULL, longitude DOUBLE PRECISION NOT NULL,
    google_place_id TEXT, created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE public.hidden_reel_map_pins (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id TEXT NOT NULL,
    reel_id UUID NOT NULL REFERENCES public.reels(id) ON DELETE CASCADE,
    location_index INT NOT NULL,
    location_fingerprint TEXT
);
CREATE TABLE public.device_push_tokens (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id TEXT NOT NULL, fcm_token TEXT NOT NULL, platform TEXT DEFAULT '',
    last_seen_at TIMESTAMPTZ DEFAULT now(), created_at TIMESTAMPTZ DEFAULT now()
);
CREATE TABLE public.device_share_tokens (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id TEXT NOT NULL, token_hash TEXT NOT NULL, revoked BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ DEFAULT now()
);
CREATE TABLE public.notification_campaigns (
    campaign_id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    title TEXT NOT NULL, body TEXT NOT NULL, target TEXT NOT NULL DEFAULT 'announcement',
    announcement_id TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'draft',
    audience_filters JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE public.notifications (
    notification_id UUID PRIMARY KEY, event_key TEXT NOT NULL, user_id TEXT NOT NULL,
    type TEXT NOT NULL, target TEXT NOT NULL, target_id TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'queued',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE public.processing_cache (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_platform TEXT NOT NULL, source_content_id TEXT NOT NULL, normalized_url TEXT NOT NULL,
    extracted_data JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ DEFAULT now(), updated_at TIMESTAMPTZ DEFAULT now()
);
CREATE TABLE public.geocode_cache (
    query_key TEXT PRIMARY KEY, query_text TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'ok',
    latitude DOUBLE PRECISION, longitude DOUBLE PRECISION,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
`

const (
	userA = "11111111-1111-4111-8111-111111111111"
	userB = "22222222-2222-4222-8222-222222222222"
)

type stubAuth struct {
	err     error
	deleted []string
}

func (s *stubAuth) DeleteUser(_ context.Context, userID string) error {
	if s.err != nil {
		return s.err
	}
	s.deleted = append(s.deleted, userID)
	return nil
}

type stubCache struct{ invalidated []string }

func (s *stubCache) InvalidateUser(_ context.Context, userID string) error {
	s.invalidated = append(s.invalidated, userID)
	return nil
}

func testService(t *testing.T) (*Service, *pgxpool.Pool, *stubAuth, *stubCache) {
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

	name := "reelpin_lifecycle_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
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

	auth := &stubAuth{}
	cache := &stubCache{}
	return New(pool, auth, cache, slog.New(slog.NewJSONHandler(io.Discard, nil)), testEnvironment), pool, auth, cache
}

func seedSharedContent(t *testing.T, pool *pgxpool.Pool) (string, string) {
	t.Helper()
	ctx := context.Background()

	var versionID string
	if err := pool.QueryRow(ctx, `
		WITH content AS (
			INSERT INTO reelpin.contents
				(source_platform, source_content_type, source_content_id, normalized_url, normalized_url_hash)
			VALUES ('instagram','reel','SHARED','https://www.instagram.com/reel/SHARED/','hash')
			RETURNING id
		)
		INSERT INTO reelpin.content_versions (content_id, processor_version, extraction_schema_version)
		SELECT id, 'v1', 'v1' FROM content
		RETURNING id::text`).Scan(&versionID); err != nil {
		t.Fatalf("seeding content: %v", err)
	}

	var reelA, reelB string
	pool.QueryRow(ctx, `INSERT INTO public.reels (user_id, url, title, content_version_id)
		VALUES ($1,'https://example.com/a','A',$2) RETURNING id::text`, userA, versionID).Scan(&reelA)
	pool.QueryRow(ctx, `INSERT INTO public.reels (user_id, url, title, content_version_id)
		VALUES ($1,'https://example.com/b','B',$2) RETURNING id::text`, userB, versionID).Scan(&reelB)
	return reelA, reelB
}

func TestDeletingASaveLeavesSharedContentAlone(t *testing.T) {
	service, pool, _, cache := testService(t)
	ctx := context.Background()

	reelA, reelB := seedSharedContent(t, pool)

	var collectionID string
	pool.QueryRow(ctx, `INSERT INTO public.collections (owner_id, name) VALUES ($1,'Goa') RETURNING id::text`,
		userA).Scan(&collectionID)
	pool.Exec(ctx, `INSERT INTO public.collection_items (collection_id, reel_id, added_by) VALUES ($1,$2,$3)`,
		collectionID, reelA, userA)
	pool.Exec(ctx, `UPDATE public.collections SET cover_reel_id = $2 WHERE id = $1`, collectionID, reelA)
	pool.Exec(ctx, `INSERT INTO public.processing_jobs (user_id, url, status, result_reel_id)
		VALUES ($1,'https://example.com/a','completed',$2)`, userA, reelA)

	if err := service.DeleteReel(ctx, userA, reelA); err != nil {
		t.Fatalf("DeleteReel: %v", err)
	}

	var reels, items, versions, contents int
	pool.QueryRow(ctx, `SELECT count(*) FROM public.reels`).Scan(&reels)
	pool.QueryRow(ctx, `SELECT count(*) FROM public.collection_items`).Scan(&items)
	pool.QueryRow(ctx, `SELECT count(*) FROM reelpin.content_versions`).Scan(&versions)
	pool.QueryRow(ctx, `SELECT count(*) FROM reelpin.contents`).Scan(&contents)

	if reels != 1 {
		t.Errorf("reels = %d, want the other user's save to survive", reels)
	}
	if items != 0 {
		t.Errorf("collection items = %d, want the reference cleared", items)
	}
	// The other user is still reading this content.
	if versions != 1 || contents != 1 {
		t.Fatalf("shared content was deleted with one user's save: versions=%d contents=%d", versions, contents)
	}

	var cover *string
	pool.QueryRow(ctx, `SELECT cover_reel_id::text FROM public.collections`).Scan(&cover)
	if cover != nil {
		t.Error("the collection still points at a deleted cover")
	}
	var resultReel *string
	pool.QueryRow(ctx, `SELECT result_reel_id::text FROM public.processing_jobs`).Scan(&resultReel)
	if resultReel != nil {
		t.Error("a job still points at a deleted reel")
	}

	var events int
	pool.QueryRow(ctx, `SELECT count(*) FROM reelpin.outbox_events WHERE event_type = 'reel.deleted'`).Scan(&events)
	if events != 1 {
		t.Errorf("cleanup events = %d, want one", events)
	}
	if len(cache.invalidated) == 0 {
		t.Error("the user's cached responses were not invalidated")
	}

	// Another user's reel is not deletable, and looks like it does not exist.
	if err := service.DeleteReel(ctx, userA, reelB); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user delete = %v, want ErrNotFound", err)
	}
	if err := service.DeleteReel(ctx, userA, "not-a-uuid"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("malformed id = %v, want ErrNotFound", err)
	}
}

func TestDeletingAnAccountRemovesEverythingItOwns(t *testing.T) {
	service, pool, auth, _ := testService(t)
	ctx := context.Background()

	reelA, _ := seedSharedContent(t, pool)
	pool.Exec(ctx, `INSERT INTO public.profiles (id, display_name) VALUES ($1::uuid, 'A')`, userA)
	pool.Exec(ctx, `INSERT INTO public.device_push_tokens (user_id, fcm_token) VALUES ($1,'token-a')`, userA)
	pool.Exec(ctx, `INSERT INTO public.device_share_tokens (user_id, token_hash) VALUES ($1,'hash-a')`, userA)
	pool.Exec(ctx, `INSERT INTO public.manual_map_pins (user_id, name, latitude, longitude) VALUES ($1,'Pin',1,2)`, userA)
	pool.Exec(ctx, `INSERT INTO public.hidden_reel_map_pins (user_id, reel_id, location_index) VALUES ($1,$2,0)`, userA, reelA)
	pool.Exec(ctx, `INSERT INTO public.notifications (notification_id, event_key, user_id, type, target, target_id)
		VALUES (gen_random_uuid(),'k',$1,'reel_ready','reel_detail','x')`, userA)

	var ownedCollection, othersCollection string
	pool.QueryRow(ctx, `INSERT INTO public.collections (owner_id, name) VALUES ($1,'Mine') RETURNING id::text`, userA).Scan(&ownedCollection)
	pool.QueryRow(ctx, `INSERT INTO public.collections (owner_id, name) VALUES ($1,'Theirs') RETURNING id::text`, userB).Scan(&othersCollection)
	pool.Exec(ctx, `INSERT INTO public.collection_members (collection_id, user_id, role) VALUES ($1,$2,'editor')`,
		othersCollection, userA)

	report, err := service.DeleteAccount(ctx, userA)
	if err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	if report.Reels != 1 || report.Collections != 1 || report.DeviceTokens != 1 || report.ShareTokens != 1 {
		t.Fatalf("report = %+v", report)
	}
	if !report.AuthUserDeleted || len(auth.deleted) != 1 {
		t.Error("the identity was not deleted last")
	}

	for _, check := range []struct {
		table string
		query string
	}{
		{"reels", `SELECT count(*) FROM public.reels WHERE user_id = $1`},
		{"profiles", `SELECT count(*) FROM public.profiles WHERE id::text = $1`},
		{"device tokens", `SELECT count(*) FROM public.device_push_tokens WHERE user_id = $1`},
		{"share tokens", `SELECT count(*) FROM public.device_share_tokens WHERE user_id = $1`},
		{"manual pins", `SELECT count(*) FROM public.manual_map_pins WHERE user_id = $1`},
		{"notifications", `SELECT count(*) FROM public.notifications WHERE user_id = $1`},
		{"memberships", `SELECT count(*) FROM public.collection_members WHERE user_id = $1`},
		{"owned collections", `SELECT count(*) FROM public.collections WHERE owner_id = $1`},
	} {
		var count int
		pool.QueryRow(ctx, check.query, userA).Scan(&count)
		if count != 0 {
			t.Errorf("%s left %d rows", check.table, count)
		}
	}

	// Someone else's collection survives, minus the membership.
	var others int
	pool.QueryRow(ctx, `SELECT count(*) FROM public.collections WHERE owner_id = $1`, userB).Scan(&others)
	if others != 1 {
		t.Error("another user's collection was deleted")
	}
	// The other user's save survives, and so does the shared content.
	var remainingReels, contents int
	pool.QueryRow(ctx, `SELECT count(*) FROM public.reels`).Scan(&remainingReels)
	pool.QueryRow(ctx, `SELECT count(*) FROM reelpin.contents`).Scan(&contents)
	if remainingReels != 1 || contents != 1 {
		t.Errorf("reels = %d contents = %d, want the other user untouched", remainingReels, contents)
	}

	var events int
	pool.QueryRow(ctx, `SELECT count(*) FROM reelpin.outbox_events WHERE event_type = 'account.deleted'`).Scan(&events)
	if events != 1 {
		t.Errorf("cleanup events = %d, want one", events)
	}
}

func TestAccountDeletionIsSafeToRepeat(t *testing.T) {
	service, pool, auth, _ := testService(t)
	ctx := context.Background()
	seedSharedContent(t, pool)

	// The identity service is down: everything else is already gone, and the
	// caller sees a failure.
	auth.err = errors.New("auth is unavailable")
	if _, err := service.DeleteAccount(ctx, userA); err == nil {
		t.Fatal("a failed identity delete was reported as success")
	}

	var reels int
	pool.QueryRow(ctx, `SELECT count(*) FROM public.reels WHERE user_id = $1`, userA).Scan(&reels)
	if reels != 0 {
		t.Error("the relational data was rolled back, so the retry has more to do than it should")
	}

	// Running it again finishes the job.
	auth.err = nil
	report, err := service.DeleteAccount(ctx, userA)
	if err != nil {
		t.Fatalf("second DeleteAccount: %v", err)
	}
	if !report.AuthUserDeleted {
		t.Error("the retry did not delete the identity")
	}
}

func TestSweepIsDryRunByDefault(t *testing.T) {
	_, pool, _, _ := testService(t)
	ctx := context.Background()

	// Old rows across three tables.
	pool.Exec(ctx, `INSERT INTO public.processing_jobs (user_id, url, status, created_at)
		VALUES ($1,'https://example.com/old','completed', now() - interval '200 days')`, userA)
	pool.Exec(ctx, `INSERT INTO public.geocode_cache (query_key, query_text, updated_at)
		VALUES ('old','old', now() - interval '400 days')`)
	pool.Exec(ctx, `INSERT INTO public.device_push_tokens (user_id, fcm_token, last_seen_at)
		VALUES ($1,'stale', now() - interval '300 days')`, userA)

	dry, err := Sweep(ctx, pool, false, 100)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if dry.TerminalJobs != 1 || dry.GeocodeCache != 1 || dry.DeviceTokens != 1 {
		t.Fatalf("dry run = %+v, want it to find all three", dry)
	}

	var jobs int
	pool.QueryRow(ctx, `SELECT count(*) FROM public.processing_jobs`).Scan(&jobs)
	if jobs != 1 {
		t.Fatal("a dry run deleted rows")
	}

	executed, err := Sweep(ctx, pool, true, 100)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if executed.TerminalJobs != 1 || executed.GeocodeCache != 1 || executed.DeviceTokens != 1 {
		t.Fatalf("execute = %+v", executed)
	}

	pool.QueryRow(ctx, `SELECT count(*) FROM public.processing_jobs`).Scan(&jobs)
	if jobs != 0 {
		t.Errorf("jobs = %d, want the expired one gone", jobs)
	}

	// A second pass finds nothing left.
	again, err := Sweep(ctx, pool, true, 100)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if again.TerminalJobs != 0 || again.GeocodeCache != 0 {
		t.Errorf("a second sweep removed more: %+v", again)
	}
}

func TestSweepKeepsContentSomebodyStillSaved(t *testing.T) {
	_, pool, _, _ := testService(t)
	ctx := context.Background()
	seedSharedContent(t, pool)

	// Age the content past the window; it is still referenced by two saves.
	pool.Exec(ctx, `UPDATE reelpin.contents SET created_at = now() - interval '400 days'`)

	report, err := Sweep(ctx, pool, true, 100)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if report.UnreferencedContent != 0 {
		t.Fatalf("removed %d referenced contents", report.UnreferencedContent)
	}

	// Once nobody saves it, it goes.
	pool.Exec(ctx, `DELETE FROM public.reels`)
	report, err = Sweep(ctx, pool, true, 100)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if report.UnreferencedContent != 1 {
		t.Fatalf("unreferenced content = %d, want it collected", report.UnreferencedContent)
	}
}
