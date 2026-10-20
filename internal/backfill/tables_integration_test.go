//go:build integration

package backfill

import (
	"context"
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/sourceidentity"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	scratchSchema = "legacy"

	strangerUser = "33333333-3333-4333-8333-333333333333"

	olderLegacyReel = "bbbbbbbb-0000-4000-8000-000000000001"
	placeReel       = "bbbbbbbb-0000-4000-8000-000000000002"
	newerLegacyReel = "bbbbbbbb-0000-4000-8000-000000000003"

	liveJob = "dddddddd-0000-4000-8000-000000000001"
	devJob  = "dddddddd-0000-4000-8000-000000000002"

	manualPin = "eeeeeeee-0000-4000-8000-000000000001"
	hiddenPin = "eeeeeeee-0000-4000-8000-000000000002"

	placeReminder  = "ffffffff-0000-4000-8000-000000000001"
	orphanReminder = "ffffffff-0000-4000-8000-000000000002"
	devReminder    = "ffffffff-0000-4000-8000-000000000003"
	lateReminder   = "ffffffff-0000-4000-8000-000000000004"
	backdated      = "ffffffff-0000-4000-8000-000000000005"

	goodTrack = "99999999-0000-4000-8000-000000000001"
	zeroTrack = "99999999-0000-4000-8000-000000000002"

	goodFeedback   = "88888888-0000-4000-8000-000000000001"
	badTopic       = "88888888-0000-4000-8000-000000000002"
	sharedConflict = "https://www.instagram.com/reel/SHARED9/"
	laterConflict  = "https://www.instagram.com/reel/LATER9/"
)

// scratchLegacySchema is the Python shape of every table the backfill reads,
// trimmed to the columns it reads, in a schema of its own the way a Neon
// database holds them. reminders carries an environment column the real one
// does not, so the filter is proven on a wave 1 table as well as on jobs.
const scratchLegacySchema = `
CREATE SCHEMA legacy;
CREATE TABLE legacy.reels (
    id UUID PRIMARY KEY,
    user_id UUID NOT NULL,
    url TEXT NOT NULL,
    normalized_url TEXT,
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
    embedding public.vector(768),
    created_at TIMESTAMPTZ DEFAULT now()
);
CREATE TABLE legacy.processing_jobs (
    id UUID PRIMARY KEY,
    user_id UUID NOT NULL,
    url TEXT NOT NULL,
    normalized_url TEXT,
    status TEXT NOT NULL DEFAULT 'queued',
    failure_code TEXT,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 3,
    environment TEXT NOT NULL DEFAULT 'production',
    created_at TIMESTAMPTZ DEFAULT now(),
    updated_at TIMESTAMPTZ DEFAULT now(),
    completed_at TIMESTAMPTZ
);
CREATE TABLE legacy.processing_cache (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_platform TEXT NOT NULL,
    source_content_id TEXT NOT NULL,
    normalized_url TEXT NOT NULL,
    transcript TEXT DEFAULT '',
    caption TEXT DEFAULT '',
    thumbnail_url TEXT,
    extracted_data JSONB NOT NULL DEFAULT '{}'::jsonb,
    updated_at TIMESTAMPTZ DEFAULT now()
);
CREATE TABLE legacy.user_entitlements (
    user_id TEXT PRIMARY KEY,
    pro_expires_at TIMESTAMPTZ,
    product_id TEXT,
    store TEXT,
    is_sandbox BOOLEAN NOT NULL DEFAULT FALSE,
    synced_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    note TEXT
);
CREATE TABLE legacy.profiles (
    id UUID PRIMARY KEY,
    email TEXT,
    full_name TEXT,
    avatar_url TEXT,
    updated_at TIMESTAMPTZ DEFAULT now()
);
CREATE TABLE legacy.manual_map_pins (
    id UUID PRIMARY KEY,
    user_id TEXT NOT NULL,
    google_place_id TEXT NOT NULL,
    name TEXT NOT NULL DEFAULT '',
    address TEXT DEFAULT '',
    latitude DOUBLE PRECISION NOT NULL,
    longitude DOUBLE PRECISION NOT NULL,
    google_maps_url TEXT,
    category TEXT DEFAULT 'Other',
    subcategory TEXT DEFAULT 'Other',
    secondary_categories JSONB DEFAULT '[]'::jsonb,
    place_types JSONB DEFAULT '[]'::jsonb,
    metadata JSONB DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW()
);
CREATE TABLE legacy.hidden_reel_map_pins (
    id UUID PRIMARY KEY,
    user_id TEXT NOT NULL,
    reel_id UUID NOT NULL,
    location_index INTEGER NOT NULL,
    location_fingerprint TEXT NOT NULL DEFAULT '',
    hidden_at TIMESTAMPTZ DEFAULT NOW(),
    created_at TIMESTAMPTZ DEFAULT NOW()
);
CREATE TABLE legacy.reminders (
    id UUID PRIMARY KEY,
    user_id TEXT NOT NULL,
    reel_id UUID,
    title TEXT NOT NULL,
    body TEXT NOT NULL DEFAULT '',
    fire_at TIMESTAMPTZ NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    environment TEXT NOT NULL DEFAULT 'production',
    created_at TIMESTAMPTZ DEFAULT NOW(),
    sent_at TIMESTAMPTZ
);
CREATE TABLE legacy.price_tracks (
    id UUID PRIMARY KEY,
    user_id TEXT NOT NULL,
    reel_id UUID,
    url TEXT NOT NULL,
    title TEXT NOT NULL DEFAULT '',
    target_price NUMERIC NOT NULL,
    currency TEXT NOT NULL DEFAULT '',
    last_price NUMERIC,
    active BOOLEAN NOT NULL DEFAULT TRUE,
    last_checked_at TIMESTAMPTZ,
    notified_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ DEFAULT NOW()
);
CREATE TABLE legacy.feedback (
    id UUID PRIMARY KEY,
    topic TEXT NOT NULL,
    email TEXT,
    message TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE legacy.link_clicks (
    id BIGSERIAL PRIMARY KEY,
    target TEXT NOT NULL,
    referer TEXT,
    user_agent TEXT,
    country TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
`

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("executing %q: %v", sql, err)
	}
}

func exists(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) bool {
	t.Helper()
	var found bool
	if err := pool.QueryRow(context.Background(), `SELECT EXISTS (`+sql+`)`, args...).Scan(&found); err != nil {
		t.Fatalf("checking %q: %v", sql, err)
	}
	return found
}

func auditAction(t *testing.T, pool *pgxpool.Pool, table, id string) string {
	t.Helper()
	var action string
	if err := pool.QueryRow(context.Background(), `
		SELECT action FROM reelpin.backfill_audit
		WHERE backfill_version = $1 AND source_table = $2 AND source_id = $3`,
		Version, table, id).Scan(&action); err != nil {
		t.Fatalf("reading the audit for %s %s: %v", table, id, err)
	}
	return action
}

// nativeSave is a save the new app made itself during the dual run, built the
// way the submission path builds one.
func nativeSave(t *testing.T, pool *pgxpool.Pool, userID, rawURL string, savedAt time.Time) string {
	t.Helper()
	ctx := context.Background()
	identity, scopeHash, err := identify(ctx, &sourceidentity.Resolver{}, rawURL, nil, userID)
	if err != nil {
		t.Fatalf("resolving %s: %v", rawURL, err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	contentID, _, _, err := findOrCreateContent(ctx, tx, identity, scopeHash)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("creating the content: %v", err)
	}
	var saveID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO reelpin.user_saves (user_id, content_id, saved_at)
		VALUES ($1, $2, $3) RETURNING id::text`, userID, contentID, savedAt).Scan(&saveID); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("creating the native save: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return saveID
}

func seedScratch(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	exec(t, pool, scratchLegacySchema)
	exec(t, pool, `INSERT INTO auth.users (id) VALUES ($1)`, strangerUser)

	exec(t, pool, `
		INSERT INTO legacy.reels (id, user_id, url, title, created_at)
		VALUES ($1, $2, $3, 'Saved on the old app first', '2026-01-01T00:00:00Z')`,
		olderLegacyReel, userA, sharedConflict)
	exec(t, pool, `
		INSERT INTO legacy.reels (id, user_id, url, title, locations, embedding, created_at)
		VALUES ($1, $2, 'https://www.instagram.com/reel/PLACE9/', 'A place',
		        '[{"name":"Cafe","city":"Pune","latitude":18.52,"longitude":73.85}]'::jsonb,
		        array_fill(0.1, ARRAY[768])::public.vector, '2026-02-01T00:00:00Z')`,
		placeReel, userA)

	exec(t, pool, `
		INSERT INTO legacy.processing_jobs (id, user_id, url, status, completed_at)
		VALUES ($1, $2, 'https://www.instagram.com/reel/PLACE9/', 'completed', now())`, liveJob, userA)
	exec(t, pool, `
		INSERT INTO legacy.processing_jobs (id, user_id, url, status, environment, completed_at)
		VALUES ($1, $2, 'https://www.instagram.com/reel/PLACE9/', 'completed', 'development', now())`,
		devJob, userA)

	exec(t, pool, `
		INSERT INTO legacy.user_entitlements (user_id, pro_expires_at, store, note)
		VALUES ($1, '2027-01-01T00:00:00Z', 'manual', 'paid by upi'),
		       ('not-a-user', '2027-01-01T00:00:00Z', 'manual', NULL),
		       ('44444444-4444-4444-8444-444444444444', NULL, 'manual', NULL)`, userA)
	exec(t, pool, `
		INSERT INTO legacy.profiles (id, email, full_name, updated_at)
		VALUES ($1, 'a@example.com', 'Old Name', '2026-03-01T00:00:00Z')`, userA)
	exec(t, pool, `
		INSERT INTO legacy.manual_map_pins
			(id, user_id, google_place_id, name, latitude, longitude, secondary_categories, place_types)
		VALUES ($1, $2, 'place-1', 'Beach shack', 15.55, 73.75, '["food"]'::jsonb, '["restaurant"]'::jsonb)`,
		manualPin, userA)
	exec(t, pool, `
		INSERT INTO legacy.hidden_reel_map_pins (id, user_id, reel_id, location_index, hidden_at)
		VALUES ($1, $2, $3, 0, '2026-02-02T00:00:00Z')`, hiddenPin, userA, placeReel)
	exec(t, pool, `
		INSERT INTO legacy.reminders (id, user_id, reel_id, title, fire_at, environment, created_at)
		VALUES ($1, $2, $3, 'Visit the cafe', '2027-01-01T09:00:00Z', 'production', '2026-02-03T00:00:00Z'),
		       ($4, $2, $5, 'Watch it again', '2027-01-01T09:00:00Z', 'production', '2026-02-03T00:00:00Z'),
		       ($6, $2, NULL, 'A dev reminder', '2027-01-01T09:00:00Z', 'development', '2026-02-03T00:00:00Z')`,
		placeReminder, userA, placeReel, orphanReminder, olderLegacyReel, devReminder)
	exec(t, pool, `
		INSERT INTO legacy.price_tracks (id, user_id, reel_id, url, title, target_price, currency)
		VALUES ($1, $2, $3, 'https://shop.example.com/p/1', 'Shoes', 2999, 'INR'),
		       ($4, $2, NULL, 'https://shop.example.com/p/2', 'Free thing', 0, 'INR')`,
		goodTrack, userA, placeReel, zeroTrack)
	exec(t, pool, `
		INSERT INTO legacy.feedback (id, topic, email, message)
		VALUES ($1, 'Support', 'a@example.com', 'It crashed'),
		       ($2, 'Bogus', NULL, 'Not a real topic')`, goodFeedback, badTopic)
	exec(t, pool, `
		INSERT INTO legacy.link_clicks (target, country) VALUES ('play', 'IN'), ('ios', 'IN'), ('web', 'US')`)
}

func TestWave1TablesCarryOverAndVerify(t *testing.T) {
	pool := testPool(t)
	seedScratch(t, pool)
	ctx := context.Background()

	// The new app already holds a newer save of the same reel for the same
	// user, so the legacy row has to lose to it.
	native := nativeSave(t, pool, userA, sharedConflict, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))

	dry, err := New(pool, quiet()).Run(ctx, Options{SourceSchema: scratchSchema})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if dry.Tables["link_clicks"].Copied != 2 || dry.Tables["link_clicks"].Skipped != 1 {
		t.Errorf("dry run link_clicks = %+v, want 2 copied and 1 skipped", dry.Tables["link_clicks"])
	}
	if dry.HighWaterMark != nil {
		t.Error("a dry run printed a high-water mark")
	}
	for _, table := range []string{
		"reelpin.user_entitlements", "reelpin.profiles", "reelpin.user_manual_pins",
		"reelpin.reminders", "reelpin.price_tracks", "reelpin.feedback", "reelpin.link_clicks",
		"reelpin.backfill_audit",
	} {
		if total := count(t, pool, table); total != 0 {
			t.Errorf("%s has %d rows after a dry run, want 0", table, total)
		}
	}

	report, err := New(pool, quiet()).Run(ctx, Options{Execute: true, SourceSchema: scratchSchema})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if report.HighWaterMark == nil {
		t.Fatal("a finished run printed no high-water mark")
	}

	if report.Conflicts != 1 || report.ConflictsReplaced != 0 {
		t.Errorf("conflicts=%d replaced=%d, want 1 kept as the newer native save",
			report.Conflicts, report.ConflictsReplaced)
	}
	if exists(t, pool, `SELECT 1 FROM reelpin.user_saves WHERE id = $1`, olderLegacyReel) {
		t.Error("the older legacy save went in beside the newer native one")
	}
	if !exists(t, pool, `SELECT 1 FROM reelpin.user_saves WHERE id = $1`, native) {
		t.Error("the newer native save was replaced")
	}
	if action := auditAction(t, pool, sourceReels, olderLegacyReel); action != "skipped_conflict" {
		t.Errorf("the losing legacy reel is audited %q, want skipped_conflict", action)
	}

	// The development job never reaches production.
	if report.JobsScanned != 1 {
		t.Errorf("jobs_scanned = %d, want 1: the development job is excluded", report.JobsScanned)
	}
	if exists(t, pool, `SELECT 1 FROM reelpin.processing_jobs WHERE id = $1`, devJob) {
		t.Error("a development job was copied")
	}

	entitlements := report.Tables["user_entitlements"]
	if entitlements.Copied != 1 || entitlements.SkippedByAction["skipped_invalid"] != 1 ||
		entitlements.SkippedByAction["skipped_unknown_reference"] != 1 {
		t.Errorf("user_entitlements = %+v, want one copied, one invalid id, one unknown user", entitlements)
	}
	var note string
	if err := pool.QueryRow(ctx, `SELECT note FROM reelpin.user_entitlements WHERE user_id = $1`,
		userA).Scan(&note); err != nil || note != "paid by upi" {
		t.Errorf("the entitlement note = %q (%v), want it carried", note, err)
	}

	var email string
	if err := pool.QueryRow(ctx, `SELECT email FROM reelpin.profiles WHERE user_id = $1`,
		userA).Scan(&email); err != nil || email != "a@example.com" {
		t.Errorf("the profile email = %q (%v)", email, err)
	}

	var latitude float64
	var tags []string
	if err := pool.QueryRow(ctx, `
		SELECT ST_Y(point::geometry), secondary_categories FROM reelpin.user_manual_pins
		WHERE id = $1 AND user_id = $2 AND google_place_id = 'place-1'`,
		manualPin, userA).Scan(&latitude, &tags); err != nil {
		t.Fatalf("reading the manual pin under its legacy id: %v", err)
	}
	if latitude != 15.55 || len(tags) != 1 || tags[0] != "food" {
		t.Errorf("manual pin latitude=%v tags=%v", latitude, tags)
	}

	// Nothing has located the reel's places yet, so the hidden pin has
	// nothing to hide and says so rather than vanishing.
	if action := auditAction(t, pool, "hidden_reel_map_pins", hiddenPin); action != "skipped_no_location" {
		t.Errorf("the hidden pin is audited %q, want skipped_no_location", action)
	}

	var saveID *string
	if err := pool.QueryRow(ctx, `SELECT save_id::text FROM reelpin.reminders WHERE id = $1`,
		placeReminder).Scan(&saveID); err != nil || saveID == nil || *saveID != placeReel {
		t.Errorf("the reminder's save = %v (%v), want the reel's save %s", saveID, err, placeReel)
	}
	if err := pool.QueryRow(ctx, `SELECT save_id::text FROM reelpin.reminders WHERE id = $1`,
		orphanReminder).Scan(&saveID); err != nil || saveID != nil {
		t.Errorf("a reminder on a reel that lost its conflict points at %v (%v), want nothing", saveID, err)
	}
	if exists(t, pool, `SELECT 1 FROM reelpin.reminders WHERE id = $1`, devReminder) {
		t.Error("a development reminder was copied")
	}
	if report.Tables["price_tracks"].SkippedByAction["skipped_invalid"] != 1 {
		t.Errorf("price_tracks = %+v, want the zero target rejected", report.Tables["price_tracks"])
	}
	if !exists(t, pool, `SELECT 1 FROM reelpin.feedback WHERE id = $1`, goodFeedback) ||
		exists(t, pool, `SELECT 1 FROM reelpin.feedback WHERE id = $1`, badTopic) {
		t.Error("feedback did not carry exactly the valid row")
	}

	// A bigserial id becomes a UUID that does not change between runs.
	var clickID string
	if err := pool.QueryRow(ctx, `
		SELECT id::text FROM reelpin.link_clicks WHERE target = 'play'`).Scan(&clickID); err != nil {
		t.Fatalf("reading the click: %v", err)
	}
	var derived string
	if err := pool.QueryRow(ctx, `SELECT md5('link_clicks:1')::uuid::text`).Scan(&derived); err != nil {
		t.Fatal(err)
	}
	if clickID != derived {
		t.Errorf("the click id = %s, want the stable %s", clickID, derived)
	}

	report1, err := NewVerifier(pool).Verify(ctx, VerifyOptions{SourceSchema: scratchSchema})
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	if report1.OK() {
		t.Error("the backfill verified while the map and search were missing data")
	}
	if report1.Embeddings.Legacy != 1 || report1.Embeddings.Canonical != 0 {
		t.Errorf("embeddings = %+v, want 1 legacy and 0 canonical", report1.Embeddings)
	}
	if report1.MapPoints.LegacyReelPoints != 1 || report1.MapPoints.CanonicalReelPoints != 0 {
		t.Errorf("map points = %+v, want 1 legacy reel point and none canonical", report1.MapPoints)
	}
	if report1.MapPoints.LegacyManualPins != 1 || report1.MapPoints.CanonicalManualPins != 1 {
		t.Errorf("manual pins = %+v, want 1 and 1", report1.MapPoints)
	}
	for _, table := range report1.Tables {
		if table.Unexplained != 0 || table.CarriedButMissing != 0 || table.UsersDiffering != 0 ||
			table.Expected != table.Present {
			t.Errorf("%s does not balance: %+v", table.SourceTable, table)
		}
	}

	// Locate the place and embed the reel the way the pipeline would, then
	// rerun the hidden pins from the start of their table.
	exec(t, pool, `
		INSERT INTO reelpin.content_locations (content_version_id, ordinal, name, point, source)
		SELECT c.current_version_id, 0, 'Cafe', ST_MakePoint(73.85, 18.52)::geography, 'extraction'
		FROM reelpin.user_saves s JOIN reelpin.contents c ON c.id = s.content_id WHERE s.id = $1`, placeReel)
	exec(t, pool, `
		INSERT INTO reelpin.content_embeddings
			(content_version_id, embedding, model, dimension, document_version, document_hash)
		SELECT c.current_version_id, array_fill(0.1, ARRAY[768])::public.vector,
		       'gemini-embedding-001', 768, 'v1', 'hash'
		FROM reelpin.user_saves s JOIN reelpin.contents c ON c.id = s.content_id WHERE s.id = $1`, placeReel)
	exec(t, pool, `
		DELETE FROM reelpin.backfill_progress WHERE source_table = 'hidden_reel_map_pins'`)

	rerun, err := New(pool, quiet()).Run(ctx, Options{Execute: true, SourceSchema: scratchSchema})
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if rerun.Tables["hidden_reel_map_pins"].Copied != 1 {
		t.Errorf("hidden pins on rerun = %+v, want 1 copied", rerun.Tables["hidden_reel_map_pins"])
	}
	if rerun.Tables["reminders"].Scanned != 0 {
		t.Errorf("the rerun rescanned reminders: %+v; their cursor is finished", rerun.Tables["reminders"])
	}

	verified, err := NewVerifier(pool).Verify(ctx, VerifyOptions{SourceSchema: scratchSchema})
	if err != nil {
		t.Fatalf("verifying again: %v", err)
	}
	if !verified.OK() {
		t.Errorf("the completed backfill does not verify: %+v", verified)
	}
}

func TestSinceCopiesOnlyNewerRowsAndKeepsTheNewerSave(t *testing.T) {
	pool := testPool(t)
	seedScratch(t, pool)
	ctx := context.Background()

	snapshot, err := New(pool, quiet()).Run(ctx, Options{Execute: true, SourceSchema: scratchSchema})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	mark := *snapshot.HighWaterMark

	var progressBefore string
	if err := pool.QueryRow(ctx, `
		SELECT string_agg(p::text, E'\n' ORDER BY source_table) FROM reelpin.backfill_progress p`).
		Scan(&progressBefore); err != nil {
		t.Fatal(err)
	}

	// During the dual run: the new app saved a reel, and later the same user
	// saved it again on the old app. The old app's save is the newer one.
	native := nativeSave(t, pool, userB, laterConflict, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	exec(t, pool, `
		INSERT INTO legacy.reels (id, user_id, url, title, created_at)
		VALUES ($1, $2, $3, 'Saved again on the old app', now())`, newerLegacyReel, userB, laterConflict)
	exec(t, pool, `
		INSERT INTO legacy.reminders (id, user_id, title, fire_at, created_at)
		VALUES ($1, $2, 'Set after the snapshot', '2027-02-01T09:00:00Z', now()),
		       ($3, $2, 'Back-dated', '2027-02-01T09:00:00Z', '2020-01-01T00:00:00Z')`,
		lateReminder, userA, backdated)
	exec(t, pool, `UPDATE legacy.profiles SET full_name = 'New Name', updated_at = now() WHERE id = $1`, userA)
	exec(t, pool, `UPDATE legacy.price_tracks SET last_price = 2500, last_checked_at = now() WHERE id = $1`,
		goodTrack)
	exec(t, pool, `INSERT INTO legacy.link_clicks (target) VALUES ('play')`)

	delta, err := New(pool, quiet()).Run(ctx, Options{Execute: true, SourceSchema: scratchSchema, Since: &mark})
	if err != nil {
		t.Fatalf("delta: %v", err)
	}
	if delta.ReelsScanned != 1 {
		t.Errorf("the delta scanned %d reels, want only the new one", delta.ReelsScanned)
	}
	if delta.ConflictsReplaced != 1 {
		t.Errorf("conflicts_replaced = %d, want the newer legacy save to win", delta.ConflictsReplaced)
	}
	if !exists(t, pool, `SELECT 1 FROM reelpin.user_saves WHERE id = $1 AND user_id = $2`, newerLegacyReel, userB) {
		t.Error("the newer legacy save is missing")
	}
	if exists(t, pool, `SELECT 1 FROM reelpin.user_saves WHERE id = $1`, native) {
		t.Error("the older native save survived beside the newer one")
	}

	if !exists(t, pool, `SELECT 1 FROM reelpin.reminders WHERE id = $1`, lateReminder) {
		t.Error("a reminder set after the snapshot was not copied")
	}
	if exists(t, pool, `SELECT 1 FROM reelpin.reminders WHERE id = $1`, backdated) {
		t.Error("a row older than the mark was copied by the delta")
	}
	if !exists(t, pool, `SELECT 1 FROM reelpin.profiles WHERE user_id = $1 AND full_name = 'New Name'`, userA) {
		t.Error("the profile update was not carried")
	}
	if !exists(t, pool, `SELECT 1 FROM reelpin.price_tracks WHERE id = $1 AND last_price = 2500`, goodTrack) {
		t.Error("the price check result was not carried")
	}
	if total := count(t, pool, "reelpin.link_clicks"); total != 3 {
		t.Errorf("%d link clicks after the delta, want 3", total)
	}

	var progressAfter string
	if err := pool.QueryRow(ctx, `
		SELECT string_agg(p::text, E'\n' ORDER BY source_table) FROM reelpin.backfill_progress p`).
		Scan(&progressAfter); err != nil {
		t.Fatal(err)
	}
	if progressAfter != progressBefore {
		t.Errorf("the delta moved the snapshot cursors:\nbefore %s\nafter  %s", progressBefore, progressAfter)
	}
	if delta.HighWaterMark == nil || !delta.HighWaterMark.After(mark) {
		t.Errorf("the delta's high-water mark %v does not move past %v", delta.HighWaterMark, mark)
	}

	// The same delta again changes nothing.
	again, err := New(pool, quiet()).Run(ctx, Options{Execute: true, SourceSchema: scratchSchema, Since: &mark})
	if err != nil {
		t.Fatalf("repeated delta: %v", err)
	}
	if again.SavesCreated != 0 || again.SavesAlreadyThere != 1 || again.Conflicts != 0 {
		t.Errorf("the repeated delta did work: %+v", again)
	}
	if total := count(t, pool, "reelpin.reminders"); total != 3 {
		t.Errorf("%d reminders after a repeated delta, want 3", total)
	}

	// The back-dated reminder is what a verify between deltas reports.
	report, err := NewVerifier(pool).Verify(ctx, VerifyOptions{SourceSchema: scratchSchema})
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	reminders := tableOf(t, report, "reminders")
	if reminders.Unexplained != 1 || len(reminders.UnexplainedIDs) != 1 || reminders.UnexplainedIDs[0] != backdated {
		t.Errorf("reminders unexplained = %d %v, want just the back-dated one",
			reminders.Unexplained, reminders.UnexplainedIDs)
	}
	if reminders.UsersDiffering != 1 || reminders.UserDiffs[0].UserID != userA {
		t.Errorf("reminders per user = %+v, want user a one short", reminders.UserDiffs)
	}
}

func TestAMissingSourceSchemaStopsTheRun(t *testing.T) {
	pool := testPool(t)
	if _, err := New(pool, quiet()).Run(context.Background(),
		Options{Execute: true, SourceSchema: "nowhere"}); err == nil {
		t.Fatal("a backfill ran against a schema with no legacy tables")
	}
	if _, err := New(pool, quiet()).Run(context.Background(),
		Options{SourceSchema: "public; DROP TABLE x"}); err == nil {
		t.Fatal("a schema name that is not an identifier was accepted")
	}
}
