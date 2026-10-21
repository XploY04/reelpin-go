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
	lateReel        = "bbbbbbbb-0000-4000-8000-000000000004"

	liveJob = "dddddddd-0000-4000-8000-000000000001"
	devJob  = "dddddddd-0000-4000-8000-000000000002"

	manualPin = "eeeeeeee-0000-4000-8000-000000000001"
	hiddenPin = "eeeeeeee-0000-4000-8000-000000000002"

	placeReminder  = "ffffffff-0000-4000-8000-000000000001"
	orphanReminder = "ffffffff-0000-4000-8000-000000000002"
	devReminder    = "ffffffff-0000-4000-8000-000000000003"
	lateReminder   = "ffffffff-0000-4000-8000-000000000004"
	backdated      = "ffffffff-0000-4000-8000-000000000005"
	mergedReminder = "ffffffff-0000-4000-8000-000000000006"

	goodTrack = "99999999-0000-4000-8000-000000000001"
	zeroTrack = "99999999-0000-4000-8000-000000000002"

	dualReel     = "bbbbbbbb-0000-4000-8000-000000000005"
	frozenReel   = "bbbbbbbb-0000-4000-8000-000000000006"
	dualReminder = "ffffffff-0000-4000-8000-000000000007"

	keptReel      = "bbbbbbbb-0000-4000-8000-000000000007"
	mergedReel    = "bbbbbbbb-0000-4000-8000-000000000008"
	mergedTrack   = "99999999-0000-4000-8000-000000000003"
	keptReminder  = "ffffffff-0000-4000-8000-000000000008"
	mergedContent = "https://www.instagram.com/reel/MERGE9/"

	goodFeedback   = "88888888-0000-4000-8000-000000000001"
	badTopic       = "88888888-0000-4000-8000-000000000002"
	sharedConflict = "https://www.instagram.com/reel/SHARED9/"
	laterConflict  = "https://www.instagram.com/reel/LATER9/"
	lateEmbedded   = "https://www.instagram.com/reel/EMBED9/"
)

// scratchLegacySchema is the Python shape of every table the backfill reads,
// trimmed to the columns it reads, in a schema of its own the way a Neon
// database holds them. reminders carries an environment column the real one
// does not, so the filter is proven on a wave 1 table as well as on jobs.
// users starts as every user auth.users has, as Supabase would.
const scratchLegacySchema = `
CREATE SCHEMA legacy;
CREATE TABLE legacy.users (id UUID PRIMARY KEY, email TEXT, created_at TIMESTAMPTZ);
INSERT INTO legacy.users (id, created_at) SELECT id, created_at FROM auth.users;
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
    embedding_model TEXT,
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
	exec(t, pool, `INSERT INTO legacy.users (id) VALUES ($1)`, strangerUser)

	exec(t, pool, `
		INSERT INTO legacy.reels (id, user_id, url, title, created_at)
		VALUES ($1, $2, $3, 'Saved on the old app first', '2026-01-01T00:00:00Z')`,
		olderLegacyReel, userA, sharedConflict)
	// Two geocoded places either side of one Python could not locate, so the
	// second place's ordinal is its array index, 2, not its rank among pins.
	exec(t, pool, `
		INSERT INTO legacy.reels (id, user_id, url, title, locations, embedding, embedding_model, created_at)
		VALUES ($1, $2, 'https://www.instagram.com/reel/PLACE9/', 'A place',
		        '[{"name":"Cafe","city":"Pune","country":"India","latitude":18.52,"longitude":73.85},
		          {"name":"Somewhere vague"},
		          {"name":"Fort","address":"Fort Rd","city":"Pune","latitude":18.53,"longitude":73.86}]'::jsonb,
		        array_fill(0.1, ARRAY[768])::public.vector, 'gemini-embedding-001', '2026-02-01T00:00:00Z')`,
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
		VALUES ($1, $2, $3, 2, '2026-02-02T00:00:00Z')`, hiddenPin, userA, placeReel)
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

	dry, err := New(pool, quiet()).Run(ctx, Options{LegacySchema: scratchSchema})
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

	report, err := New(pool, quiet()).Run(ctx, Options{Execute: true, DumpedAt: dumpedNow(), LegacySchema: scratchSchema})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if report.HighWaterMark == nil {
		t.Fatal("a finished run printed no high-water mark")
	}

	if report.Conflicts != 1 || report.ConflictsMerged != 0 {
		t.Errorf("conflicts=%d merged=%d, want 1 kept as the newer native save",
			report.Conflicts, report.ConflictsMerged)
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

	// Both geocoded places land at their array index, the vague one does not,
	// and the embedding keeps the model that made it.
	version := versionOf(t, pool, placeReel)
	if report.LocationsCopied != 2 || report.EmbeddingsCopied != 1 {
		t.Errorf("locations_copied=%d embeddings_copied=%d, want 2 and 1",
			report.LocationsCopied, report.EmbeddingsCopied)
	}
	var ordinals []int32
	if err := pool.QueryRow(ctx, `
		SELECT array_agg(ordinal ORDER BY ordinal) FROM reelpin.content_locations
		WHERE content_version_id = $1`, version).Scan(&ordinals); err != nil {
		t.Fatalf("reading the places: %v", err)
	}
	if len(ordinals) != 2 || ordinals[0] != 0 || ordinals[1] != 2 {
		t.Errorf("place ordinals = %v, want [0 2]", ordinals)
	}
	if !exists(t, pool, `
		SELECT 1 FROM reelpin.content_locations
		WHERE content_version_id = $1 AND ordinal = 2 AND name = 'Fort' AND address = 'Fort Rd'
		  AND locality = 'Pune' AND source = 'provider'
		  AND abs(ST_Y(point::geometry) - 18.53) < 1e-9 AND abs(ST_X(point::geometry) - 73.86) < 1e-9`,
		version) {
		t.Error("the second place did not carry its fields and point")
	}
	if !exists(t, pool, `
		SELECT 1 FROM reelpin.content_embeddings e, legacy.reels l
		WHERE e.content_version_id = $1 AND l.id = $2
		  AND e.model = 'gemini-embedding-001' AND e.dimension = 768
		  AND e.embedding::text = l.embedding::text`, version, placeReel) {
		t.Error("the embedding did not carry under its model")
	}

	// The hidden pin names index 2, which now exists.
	if action := auditAction(t, pool, "hidden_reel_map_pins", hiddenPin); action != "copied" {
		t.Errorf("the hidden pin is audited %q, want copied", action)
	}
	if !hiddenAt(t, pool, version, 2) {
		t.Error("the hidden pin does not hide the place at ordinal 2")
	}

	var saveID *string
	if err := pool.QueryRow(ctx, `SELECT save_id::text FROM reelpin.reminders WHERE id = $1`,
		placeReminder).Scan(&saveID); err != nil || saveID == nil || *saveID != placeReel {
		t.Errorf("the reminder's save = %v (%v), want the reel's save %s", saveID, err, placeReel)
	}
	if err := pool.QueryRow(ctx, `SELECT save_id::text FROM reelpin.reminders WHERE id = $1`,
		orphanReminder).Scan(&saveID); err != nil || saveID == nil || *saveID != native {
		t.Errorf("a reminder on a reel that lost its conflict points at %v (%v), want the kept save %s",
			saveID, err, native)
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

	verified, err := NewVerifier(pool).Verify(ctx, VerifyOptions{LegacySchema: scratchSchema})
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	if !verified.OK() {
		t.Errorf("the completed backfill does not verify: %+v", verified)
	}
	if verified.Embeddings.Legacy != 1 || verified.Embeddings.Canonical != 1 {
		t.Errorf("embeddings = %+v, want 1 and 1", verified.Embeddings)
	}
	if len(verified.Embeddings.Models) != 1 || verified.Embeddings.Models["gemini-embedding-001"] != 1 {
		t.Errorf("embedding models = %v, want the one carried vector under gemini-embedding-001",
			verified.Embeddings.Models)
	}
	if verified.MapPoints.LegacyReelPoints != 2 || verified.MapPoints.CanonicalReelPoints != 2 ||
		verified.MapPoints.LegacyManualPins != 1 || verified.MapPoints.CanonicalManualPins != 1 {
		t.Errorf("map points = %+v, want 2 reel points and 1 manual pin on each side", verified.MapPoints)
	}

	// Python re-geocodes a place without moving any stamp. The next run moves
	// the point and keeps the location row, so the hidden pin still hides it.
	var locationBefore string
	if err := pool.QueryRow(ctx, `
		SELECT id::text FROM reelpin.content_locations WHERE content_version_id = $1 AND ordinal = 2`,
		version).Scan(&locationBefore); err != nil {
		t.Fatal(err)
	}
	exec(t, pool, `
		UPDATE legacy.reels SET locations = jsonb_set(locations, '{2,latitude}', '18.54') WHERE id = $1`,
		placeReel)

	rerun, err := New(pool, quiet()).Run(ctx, Options{Execute: true, DumpedAt: dumpedNow(), LegacySchema: scratchSchema})
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if rerun.LocationsCopied != 1 || rerun.EmbeddingsCopied != 0 {
		t.Errorf("rerun locations_copied=%d embeddings_copied=%d, want only the moved place",
			rerun.LocationsCopied, rerun.EmbeddingsCopied)
	}
	if rerun.Tables["reminders"].Scanned != 0 {
		t.Errorf("the rerun rescanned reminders: %+v; their cursor is finished", rerun.Tables["reminders"])
	}
	if !exists(t, pool, `
		SELECT 1 FROM reelpin.content_locations
		WHERE id = $1 AND abs(ST_Y(point::geometry) - 18.54) < 1e-9`, locationBefore) {
		t.Error("the re-geocoded place was not updated in place")
	}
	if !hiddenAt(t, pool, version, 2) {
		t.Error("the hidden pin stopped resolving after the place moved")
	}

	again, err := NewVerifier(pool).Verify(ctx, VerifyOptions{LegacySchema: scratchSchema})
	if err != nil {
		t.Fatalf("verifying again: %v", err)
	}
	if !again.OK() {
		t.Errorf("the backfill does not verify after the place moved: %+v", again)
	}
}

func versionOf(t *testing.T, pool *pgxpool.Pool, saveID string) string {
	t.Helper()
	var version string
	if err := pool.QueryRow(context.Background(), `
		SELECT c.current_version_id::text FROM reelpin.user_saves s
		JOIN reelpin.contents c ON c.id = s.content_id WHERE s.id = $1`, saveID).Scan(&version); err != nil {
		t.Fatalf("reading the version of save %s: %v", saveID, err)
	}
	return version
}

func hiddenAt(t *testing.T, pool *pgxpool.Pool, version string, ordinal int) bool {
	t.Helper()
	return exists(t, pool, `
		SELECT 1 FROM reelpin.user_pin_preferences p
		JOIN reelpin.content_locations cl ON cl.id = p.location_id
		WHERE p.user_id = $1 AND p.hidden AND cl.content_version_id = $2 AND cl.ordinal = $3`,
		userA, version, ordinal)
}

func TestSinceCopiesOnlyNewerRowsAndKeepsTheNewerSave(t *testing.T) {
	pool := testPool(t)
	seedScratch(t, pool)
	ctx := context.Background()

	snapshot, err := New(pool, quiet()).Run(ctx, Options{Execute: true, DumpedAt: dumpedNow(), LegacySchema: scratchSchema})
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

	// During the dual run: the new app saved a reel and hung a collection
	// item, a reminder and a price track off it. Later the same user saved it
	// again on the old app, with a reminder of its own. The old app's save is
	// the newer one.
	native := nativeSave(t, pool, userB, laterConflict, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	var collection string
	if err := pool.QueryRow(ctx, `
		INSERT INTO reelpin.collections (owner_id, name) VALUES ($1, 'Trips') RETURNING id::text`,
		userB).Scan(&collection); err != nil {
		t.Fatal(err)
	}
	exec(t, pool, `INSERT INTO reelpin.collection_items (collection_id, save_id) VALUES ($1, $2)`,
		collection, native)
	exec(t, pool, `
		INSERT INTO reelpin.reminders (user_id, save_id, title, fire_at)
		VALUES ($1, $2, 'Set on the new app', '2027-03-01T09:00:00Z')`, userB, native)
	exec(t, pool, `
		INSERT INTO reelpin.price_tracks (user_id, save_id, url, target_price)
		VALUES ($1, $2, 'https://shop.example.com/p/3', 100)`, userB, native)
	exec(t, pool, `
		INSERT INTO legacy.reels (id, user_id, url, title, created_at)
		VALUES ($1, $2, $3, 'Saved again on the old app', now()),
		       ($4, $5, $6, 'Embedded later', now())`,
		newerLegacyReel, userB, laterConflict, lateReel, userA, lateEmbedded)
	exec(t, pool, `
		INSERT INTO legacy.reminders (id, user_id, reel_id, title, fire_at, created_at)
		VALUES ($1, $2, NULL, 'Set after the snapshot', '2027-02-01T09:00:00Z', now()),
		       ($3, $2, NULL, 'Back-dated', '2027-02-01T09:00:00Z', '2020-01-01T00:00:00Z'),
		       ($4, $5, $6, 'On the old app', '2027-02-01T09:00:00Z', now())`,
		lateReminder, userA, backdated, mergedReminder, userB, newerLegacyReel)
	exec(t, pool, `UPDATE legacy.profiles SET full_name = 'New Name', updated_at = now() WHERE id = $1`, userA)
	exec(t, pool, `UPDATE legacy.price_tracks SET last_price = 2500, last_checked_at = now() WHERE id = $1`,
		goodTrack)
	exec(t, pool, `INSERT INTO legacy.link_clicks (target) VALUES ('play')`)

	delta, err := New(pool, quiet()).Run(ctx, Options{Execute: true, DumpedAt: dumpedNow(), LegacySchema: scratchSchema, Since: &mark})
	if err != nil {
		t.Fatalf("delta: %v", err)
	}
	if delta.ReelsScanned != 2 {
		t.Errorf("the delta scanned %d reels, want only the two new ones", delta.ReelsScanned)
	}
	if delta.ConflictsMerged != 1 {
		t.Errorf("conflicts_merged = %d, want the newer legacy save merged", delta.ConflictsMerged)
	}

	// The native save stays under its own id with the newer saved_at, and
	// nothing that hangs off it is lost.
	if !exists(t, pool, `
		SELECT 1 FROM reelpin.user_saves s, legacy.reels l
		WHERE s.id = $1 AND s.user_id = $2 AND l.id = $3 AND s.saved_at = l.created_at`,
		native, userB, newerLegacyReel) {
		t.Error("the native save is gone or did not take the newer legacy saved_at")
	}
	if exists(t, pool, `SELECT 1 FROM reelpin.user_saves WHERE id = $1`, newerLegacyReel) {
		t.Error("the newer legacy reel became a second save of the same content")
	}
	if action := auditAction(t, pool, sourceReels, newerLegacyReel); action != "skipped_conflict" {
		t.Errorf("the merged legacy reel is audited %q, want skipped_conflict", action)
	}
	if !exists(t, pool, `SELECT 1 FROM reelpin.collection_items WHERE collection_id = $1 AND save_id = $2`,
		collection, native) {
		t.Error("the collection item on the native save was lost")
	}
	if !exists(t, pool, `SELECT 1 FROM reelpin.reminders WHERE save_id = $1 AND title = 'Set on the new app'`,
		native) {
		t.Error("the reminder on the native save was lost")
	}
	if !exists(t, pool, `SELECT 1 FROM reelpin.price_tracks WHERE save_id = $1 AND target_price = 100`, native) {
		t.Error("the price track on the native save was lost")
	}
	if !exists(t, pool, `SELECT 1 FROM reelpin.reminders WHERE id = $1 AND save_id = $2`, mergedReminder, native) {
		t.Error("the merged legacy reel's reminder does not point at the save it merged into")
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
	again, err := New(pool, quiet()).Run(ctx, Options{Execute: true, DumpedAt: dumpedNow(), LegacySchema: scratchSchema, Since: &mark})
	if err != nil {
		t.Fatalf("repeated delta: %v", err)
	}
	if again.SavesCreated != 0 || again.SavesAlreadyThere != 1 || again.ConflictsMerged != 0 ||
		again.LocationsCopied != 0 || again.EmbeddingsCopied != 0 {
		t.Errorf("the repeated delta did work: %+v", again)
	}
	if total := count(t, pool, "reelpin.reminders"); total != 5 {
		t.Errorf("%d reminders after a repeated delta, want 5", total)
	}

	// The back-dated reminder is what a verify between deltas reports.
	report, err := NewVerifier(pool).Verify(ctx, VerifyOptions{LegacySchema: scratchSchema})
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

	// Python embeds a reel after its row exists, and a Pro grant is extended
	// by hand; neither moves a stamp. A delta whose mark is past every row
	// still carries both.
	exec(t, pool, `
		UPDATE legacy.reels SET embedding = array_fill(0.2, ARRAY[768])::public.vector WHERE id = $1`, lateReel)
	exec(t, pool, `
		UPDATE legacy.user_entitlements SET pro_expires_at = '2028-01-01T00:00:00Z' WHERE user_id = $1`, userA)
	future := time.Now().Add(time.Hour)
	dumpedLater := future.Add(time.Hour)
	late, err := New(pool, quiet()).Run(ctx, Options{Execute: true, DumpedAt: &dumpedLater, LegacySchema: scratchSchema, Since: &future})
	if err != nil {
		t.Fatalf("late delta: %v", err)
	}
	if late.ReelsScanned != 0 || late.EmbeddingsCopied != 1 {
		t.Errorf("late delta reels_scanned=%d embeddings_copied=%d, want 0 and 1",
			late.ReelsScanned, late.EmbeddingsCopied)
	}
	if !exists(t, pool, `
		SELECT 1 FROM reelpin.content_embeddings WHERE content_version_id = $1 AND model = 'gemini-embedding-001'`,
		versionOf(t, pool, lateReel)) {
		t.Error("an embedding Python wrote after the reel was carried did not arrive")
	}
	if late.Tables["user_entitlements"].Scanned != 3 {
		t.Errorf("user_entitlements = %+v, want the whole table read", late.Tables["user_entitlements"])
	}
	if !exists(t, pool, `
		SELECT 1 FROM reelpin.user_entitlements WHERE user_id = $1 AND pro_expires_at = '2028-01-01T00:00:00Z'`,
		userA) {
		t.Error("a hand-extended Pro grant did not arrive")
	}
}

func TestAMissingLegacySchemaStopsTheRun(t *testing.T) {
	pool := testPool(t)
	if _, err := New(pool, quiet()).Run(context.Background(),
		Options{Execute: true, DumpedAt: dumpedNow(), LegacySchema: "nowhere"}); err == nil {
		t.Fatal("a backfill ran against a schema with no legacy tables")
	}
	if _, err := New(pool, quiet()).Run(context.Background(),
		Options{LegacySchema: "public; DROP TABLE x"}); err == nil {
		t.Fatal("a schema name that is not an identifier was accepted")
	}
}

// During the dual run user a has moved to the new app and user b has not. A
// delta has to leave everything of user a's alone, even what the Python worker
// still touches, while carrying user b's changes in full. The final delta
// then carries user b's deletions and none for user a.
func TestDeltasLeaveUsersGoHasServedAlone(t *testing.T) {
	pool := testPool(t)
	seedScratch(t, pool)
	ctx := context.Background()

	snapshot, err := New(pool, quiet()).Run(ctx, Options{Execute: true, DumpedAt: dumpedNow(), LegacySchema: scratchSchema})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	mark := *snapshot.HighWaterMark

	// User a opens the new app and changes the reminder there. The Python
	// worker still fires its frozen copy, and the daily price check still
	// writes the frozen track.
	exec(t, pool, `INSERT INTO reelpin.user_migrations (user_id) VALUES ($1)`, userA)
	exec(t, pool, `UPDATE reelpin.reminders SET title = 'Moved on the new app' WHERE id = $1`, placeReminder)
	exec(t, pool, `UPDATE legacy.reminders SET status = 'sent', sent_at = now() WHERE id = $1`, placeReminder)
	exec(t, pool, `UPDATE legacy.price_tracks SET last_price = 1, last_checked_at = now() WHERE id = $1`, goodTrack)
	exec(t, pool, `
		INSERT INTO legacy.reels (id, user_id, url, title, created_at)
		VALUES ($1, $2, 'https://www.instagram.com/reel/FROZEN9/', 'Saved on a stale install', now()),
		       ($3, $4, 'https://www.instagram.com/reel/DUAL9/', 'Still on the old app', now())`,
		frozenReel, userA, dualReel, userB)
	exec(t, pool, `
		INSERT INTO legacy.reminders (id, user_id, reel_id, title, fire_at, created_at)
		VALUES ($1, $2, NULL, 'Set on the old app', '2027-02-01T09:00:00Z', now())`, dualReminder, userB)

	delta, err := New(pool, quiet()).Run(ctx, Options{
		Execute: true, DumpedAt: dumpedNow(), LegacySchema: scratchSchema, Since: &mark,
	})
	if err != nil {
		t.Fatalf("delta: %v", err)
	}
	if delta.MigratedUsers != 1 {
		t.Errorf("migrated_users = %d, want 1", delta.MigratedUsers)
	}
	if !exists(t, pool, `
		SELECT 1 FROM reelpin.reminders
		WHERE id = $1 AND title = 'Moved on the new app' AND status = 'pending' AND sent_at IS NULL`,
		placeReminder) {
		t.Error("the Python worker's sent_at bump overwrote the reminder user a changed in Go")
	}
	if !exists(t, pool, `SELECT 1 FROM reelpin.price_tracks WHERE id = $1 AND last_price IS NULL`, goodTrack) {
		t.Error("the frozen legacy price check overwrote user a's track")
	}
	if exists(t, pool, `SELECT 1 FROM reelpin.user_saves WHERE id = $1`, frozenReel) {
		t.Error("a legacy save of a user Go has served was carried")
	}
	if !exists(t, pool, `SELECT 1 FROM reelpin.user_saves WHERE id = $1 AND user_id = $2`, dualReel, userB) {
		t.Error("user b's new legacy save was not carried")
	}
	if !exists(t, pool, `SELECT 1 FROM reelpin.reminders WHERE id = $1 AND user_id = $2`, dualReminder, userB) {
		t.Error("user b's new legacy reminder was not carried")
	}

	// The final delta, with Python stopped. User b deleted a save and a
	// reminder; user a's legacy rows are gone too, which must change nothing
	// in Go.
	exec(t, pool, `DELETE FROM legacy.reels WHERE id IN ($1, $2)`, dualReel, placeReel)
	exec(t, pool, `DELETE FROM legacy.reminders WHERE id = $1`, dualReminder)
	exec(t, pool, `DELETE FROM legacy.price_tracks WHERE id = $1`, goodTrack)
	exec(t, pool, `DELETE FROM legacy.hidden_reel_map_pins WHERE id = $1`, hiddenPin)

	final, err := New(pool, quiet()).Run(ctx, Options{
		Execute: true, DumpedAt: dumpedNow(), LegacySchema: scratchSchema, Since: delta.HighWaterMark,
		PropagateDeletes: true,
	})
	if err != nil {
		t.Fatalf("final delta: %v", err)
	}
	if final.Deleted["reels"] != 1 || final.Deleted["reminders"] != 1 || final.Deleted["price_tracks"] != 0 ||
		final.Deleted["hidden_reel_map_pins"] != 0 {
		t.Errorf("deleted = %v, want user b's save and reminder only", final.Deleted)
	}
	if exists(t, pool, `SELECT 1 FROM reelpin.user_saves WHERE id = $1`, dualReel) {
		t.Error("user b's save deleted in Python survived the final delta")
	}
	if exists(t, pool, `SELECT 1 FROM reelpin.reminders WHERE id = $1`, dualReminder) {
		t.Error("user b's reminder deleted in Python survived the final delta")
	}
	if !exists(t, pool, `SELECT 1 FROM reelpin.user_saves WHERE id = $1`, placeReel) {
		t.Error("the final delta deleted a save of a user Go has served")
	}
	if !exists(t, pool, `SELECT 1 FROM reelpin.price_tracks WHERE id = $1`, goodTrack) {
		t.Error("the final delta deleted a price track of a user Go has served")
	}
	if !hiddenAt(t, pool, versionOf(t, pool, placeReel), 2) {
		t.Error("the final delta unhid a pin of a user Go has served")
	}
	if action := auditAction(t, pool, "hidden_reel_map_pins", hiddenPin); action != "copied" {
		t.Errorf("the hidden pin of a user Go has served is audited %q, want it left copied", action)
	}

	verified, err := NewVerifier(pool).Verify(ctx, VerifyOptions{LegacySchema: scratchSchema})
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	if !verified.OK() {
		t.Errorf("the final delta does not verify: %+v", verified)
	}
	if reminders := tableOf(t, verified, "reminders"); reminders.Migrated != 2 {
		t.Errorf("reminders migrated = %d, want user a's two legacy reminders", reminders.Migrated)
	}
}

// A user still on Python who unhid a pin there gets it back on the final
// delta. The preference has no id of its own to look the legacy row up by.
func TestTheFinalDeltaUnhidesAPinUnhiddenInPython(t *testing.T) {
	pool := testPool(t)
	seedScratch(t, pool)
	ctx := context.Background()

	snapshot, err := New(pool, quiet()).Run(ctx, Options{Execute: true, DumpedAt: dumpedNow(), LegacySchema: scratchSchema})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	exec(t, pool, `DELETE FROM legacy.hidden_reel_map_pins WHERE id = $1`, hiddenPin)

	dry, err := New(pool, quiet()).Run(ctx, Options{
		DumpedAt: dumpedNow(), LegacySchema: scratchSchema, Since: snapshot.HighWaterMark, PropagateDeletes: true,
	})
	if err != nil {
		t.Fatalf("dry final delta: %v", err)
	}
	if dry.Deleted["hidden_reel_map_pins"] != 1 || !hiddenAt(t, pool, versionOf(t, pool, placeReel), 2) {
		t.Errorf("a dry run deleted = %v and must change nothing", dry.Deleted)
	}

	final, err := New(pool, quiet()).Run(ctx, Options{
		Execute: true, DumpedAt: dumpedNow(), LegacySchema: scratchSchema, Since: snapshot.HighWaterMark,
		PropagateDeletes: true,
	})
	if err != nil {
		t.Fatalf("final delta: %v", err)
	}
	if final.Deleted["hidden_reel_map_pins"] != 1 || hiddenAt(t, pool, versionOf(t, pool, placeReel), 2) {
		t.Errorf("deleted = %v, want the pin unhidden", final.Deleted)
	}
	if action := auditAction(t, pool, "hidden_reel_map_pins", hiddenPin); action != deletedInLegacy {
		t.Errorf("the unhidden pin is audited %q, want %s", action, deletedInLegacy)
	}

	verified, err := NewVerifier(pool).Verify(ctx, VerifyOptions{LegacySchema: scratchSchema})
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	if !verified.OK() {
		t.Errorf("the final delta does not verify: %+v", verified)
	}
}

// A user still on Python saved the same reel twice. The second legacy reel
// merged into the first one's save and hangs a reminder and a price track off
// it. Deleting the first reel in Python leaves the user's save, so the final
// delta must keep it until the second reel is gone too.
func TestTheFinalDeltaKeepsASaveAnotherLegacyReelResolvesTo(t *testing.T) {
	pool := testPool(t)
	exec(t, pool, scratchLegacySchema)
	ctx := context.Background()

	exec(t, pool, `
		INSERT INTO legacy.reels (id, user_id, url, title, created_at)
		VALUES ($1, $2, $3, 'Saved first', '2026-01-01T00:00:00Z'),
		       ($4, $2, $3, 'Saved again', '2026-02-01T00:00:00Z')`,
		keptReel, userB, mergedContent, mergedReel)
	exec(t, pool, `
		INSERT INTO legacy.reminders (id, user_id, reel_id, title, fire_at, created_at)
		VALUES ($1, $2, $3, 'From the second save', '2027-02-01T09:00:00Z', '2026-02-02T00:00:00Z')`,
		keptReminder, userB, mergedReel)
	exec(t, pool, `
		INSERT INTO legacy.price_tracks (id, user_id, reel_id, url, title, target_price, currency, created_at)
		VALUES ($1, $2, $3, 'https://shop.example.com/p/9', 'Lamp', 999, 'INR', '2026-02-02T00:00:00Z')`,
		mergedTrack, userB, mergedReel)

	snapshot, err := New(pool, quiet()).Run(ctx, Options{Execute: true, DumpedAt: dumpedNow(), LegacySchema: scratchSchema})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snapshot.ConflictsMerged != 1 || auditAction(t, pool, sourceReels, mergedReel) != "skipped_conflict" {
		t.Fatalf("the second reel did not merge into the first: %+v", snapshot)
	}
	if !exists(t, pool, `SELECT 1 FROM reelpin.reminders WHERE id = $1 AND save_id = $2`, keptReminder, keptReel) ||
		!exists(t, pool, `SELECT 1 FROM reelpin.price_tracks WHERE id = $1 AND save_id = $2`, mergedTrack, keptReel) {
		t.Fatal("the merged reel's reminder and price track do not point at the kept save")
	}

	exec(t, pool, `DELETE FROM legacy.reels WHERE id = $1`, keptReel)
	final, err := New(pool, quiet()).Run(ctx, Options{
		Execute: true, DumpedAt: dumpedNow(), LegacySchema: scratchSchema, Since: snapshot.HighWaterMark,
		PropagateDeletes: true,
	})
	if err != nil {
		t.Fatalf("final delta: %v", err)
	}
	if final.Deleted["reels"] != 0 {
		t.Errorf("deleted = %v, want the save kept", final.Deleted)
	}
	if !exists(t, pool, `SELECT 1 FROM reelpin.user_saves WHERE id = $1 AND user_id = $2`, keptReel, userB) {
		t.Error("the final delta deleted a save the merged legacy reel still resolves to")
	}
	if !exists(t, pool, `SELECT 1 FROM reelpin.reminders WHERE id = $1 AND save_id = $2`, keptReminder, keptReel) {
		t.Error("the merged reel's reminder did not survive the final delta")
	}
	if !exists(t, pool, `SELECT 1 FROM reelpin.price_tracks WHERE id = $1 AND save_id = $2`, mergedTrack, keptReel) {
		t.Error("the merged reel's price track did not survive the final delta")
	}
	if action := auditAction(t, pool, sourceReels, keptReel); action != "saved" {
		t.Errorf("the kept save is audited %q, want it still saved", action)
	}

	verified, err := NewVerifier(pool).Verify(ctx, VerifyOptions{LegacySchema: scratchSchema})
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	if !verified.OK() {
		t.Errorf("the final delta does not verify: %+v", verified)
	}

	// Once the second reel is gone as well, nothing in Python names the save.
	exec(t, pool, `DELETE FROM legacy.reels WHERE id = $1`, mergedReel)
	again, err := New(pool, quiet()).Run(ctx, Options{
		Execute: true, DumpedAt: dumpedNow(), LegacySchema: scratchSchema, Since: snapshot.HighWaterMark,
		PropagateDeletes: true,
	})
	if err != nil {
		t.Fatalf("second final delta: %v", err)
	}
	if again.Deleted["reels"] != 1 || exists(t, pool, `SELECT 1 FROM reelpin.user_saves WHERE id = $1`, keptReel) {
		t.Errorf("deleted = %v, want the save gone once no legacy reel resolves to it", again.Deleted)
	}
}

// A snapshot resumed, or rerun, after the dual run started is not a delta, and
// still has to leave a user Go has served alone.
func TestEveryRunLeavesUsersGoHasServedAlone(t *testing.T) {
	pool := testPool(t)
	seedScratch(t, pool)
	ctx := context.Background()

	partial, err := New(pool, quiet()).Run(ctx, Options{
		Execute: true, DumpedAt: dumpedNow(), LegacySchema: scratchSchema, MaxRows: 1,
	})
	if err != nil {
		t.Fatalf("partial snapshot: %v", err)
	}
	if partial.SavesCreated != 1 {
		t.Fatalf("the partial snapshot created %d saves, want 1", partial.SavesCreated)
	}

	exec(t, pool, `INSERT INTO reelpin.user_migrations (user_id) VALUES ($1)`, userA)

	resumed, err := New(pool, quiet()).Run(ctx, Options{Execute: true, DumpedAt: dumpedNow(), LegacySchema: scratchSchema})
	if err != nil {
		t.Fatalf("resumed snapshot: %v", err)
	}
	if resumed.ReelsScanned != 0 || resumed.JobsScanned != 0 {
		t.Errorf("the resumed snapshot read %d reels and %d jobs of a user Go has served",
			resumed.ReelsScanned, resumed.JobsScanned)
	}
	if exists(t, pool, `SELECT 1 FROM reelpin.user_saves WHERE id = $1`, placeReel) {
		t.Error("the resumed snapshot carried a save of a user Go has served")
	}
	for _, table := range []string{
		"reelpin.reminders", "reelpin.price_tracks", "reelpin.user_manual_pins",
		"reelpin.user_pin_preferences", "reelpin.user_entitlements",
	} {
		if exists(t, pool, `SELECT 1 FROM `+table+` WHERE user_id = $1`, userA) {
			t.Errorf("the resumed snapshot wrote %s for a user Go has served", table)
		}
	}
	if exists(t, pool, `SELECT 1 FROM reelpin.profiles WHERE user_id = $1`, userA) {
		t.Error("the resumed snapshot wrote the profile of a user Go has served")
	}

	// Pro granted in Go after the move. user_entitlements is copied whole on
	// every run, so a rerun is where it would be overwritten.
	exec(t, pool, `
		INSERT INTO reelpin.user_entitlements (user_id, pro_expires_at, store, note)
		VALUES ($1, '2030-01-01T00:00:00Z', 'manual', 'granted in go')`, userA)
	if _, err := New(pool, quiet()).Run(ctx, Options{Execute: true, DumpedAt: dumpedNow(), LegacySchema: scratchSchema}); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if !exists(t, pool, `
		SELECT 1 FROM reelpin.user_entitlements
		WHERE user_id = $1 AND pro_expires_at = '2030-01-01T00:00:00Z' AND note = 'granted in go'`, userA) {
		t.Error("a rerun overwrote the Pro grant of a user Go has served")
	}
}

// A user can move between the read of a page and the write of a row on it.
// The page here is read first, then the user moves, then the rows are
// written: none of them may land.
func TestAUserWhoMovesAfterThePageWasReadIsNotWritten(t *testing.T) {
	pool := testPool(t)
	seedScratch(t, pool)
	ctx := context.Background()

	b := New(pool, quiet())
	var err error
	if b.source, err = newSource(scratchSchema); err != nil {
		t.Fatal(err)
	}
	if err := b.source.describe(ctx, pool); err != nil {
		t.Fatal(err)
	}
	b.seen = map[string]bool{}

	var reminders tableMapping
	for _, mapping := range wave1Mappings {
		if mapping.name == "reminders" {
			reminders = mapping
		}
	}
	shape := b.source.shape(reminders.name)
	options := Options{Execute: true, BatchSize: 10, DumpedAt: dumpedNow(), LegacySchema: scratchSchema}

	reels, err := b.readReels(ctx, "", options)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := b.readKeys(ctx, reminders, shape, "", options)
	if err != nil {
		t.Fatal(err)
	}
	if len(reels) != 2 || len(keys) != 2 {
		t.Fatalf("read %d reels and %d reminders, want 2 of each", len(reels), len(keys))
	}

	exec(t, pool, `INSERT INTO reelpin.user_migrations (user_id) VALUES ($1)`, userA)

	report := Report{Tables: map[string]TableReport{}}
	for _, row := range reels {
		if err := b.backfillOneReel(ctx, options, 1, row, &report); err != nil {
			t.Fatalf("reel %s: %v", row.ID, err)
		}
	}
	statement, err := b.upsertStatement(reminders, shape)
	if err != nil {
		t.Fatal(err)
	}
	counts := TableReport{SkippedByAction: map[string]int{}}
	for _, key := range keys {
		if err := b.copyOneRow(ctx, options, 1, reminders, statement, key, &counts); err != nil {
			t.Fatalf("reminder %s: %v", key.id, err)
		}
	}

	if total := count(t, pool, "reelpin.user_saves"); total != 0 {
		t.Errorf("%d saves written for a user who moved after the page was read", total)
	}
	if total := count(t, pool, "reelpin.reminders"); total != 0 {
		t.Errorf("%d reminders written for a user who moved after the page was read", total)
	}
	if counts.Copied != 0 || counts.Skipped != 0 || count(t, pool, "reelpin.backfill_audit") != 0 {
		t.Errorf("counts = %+v and audit rows were written for a user who moved", counts)
	}
}
