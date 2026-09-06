//go:build integration

package pipeline

import (
	"context"
	"errors"
	"testing"

	"github.com/XploY04/reelpin-go/internal/enqueue"
	"github.com/XploY04/reelpin-go/internal/postgres"
	"github.com/XploY04/reelpin-go/internal/reels"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// legacyReelsTable is the public.reels shape Supabase owns. No migration in
// this repo creates it: this is the fixture that lets an integration test see
// what production already has. The columns are the ones internal/postgres
// selects, which is the read path the app is still served from.
const legacyReelsTable = `
CREATE TABLE public.reels (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL,
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
`

const submittedReel = "https://www.instagram.com/reel/SUBMIT1/"

// submitAndRun is the whole v2 happy path up to the read: a real submission
// through enqueue, then the run that submission created.
func submitAndRun(t *testing.T, h *harness, userID string) string {
	t.Helper()
	ctx := context.Background()

	service := enqueue.New(postgres.NewEnqueue(h.pool), &sourceidentity.Resolver{}, nil)
	result, err := service.Submit(ctx, enqueue.Request{
		UserID:         userID,
		URL:            submittedReel,
		IdempotencyKey: uuid.NewString(),
		Endpoint:       "processing-jobs/reels",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if result.Job == nil {
		t.Fatalf("submit answered with no job to poll")
	}

	if err := h.pool.QueryRow(ctx,
		`SELECT run_id::text FROM reelpin.processing_jobs WHERE id = $1`,
		result.Job.ID).Scan(&h.runID); err != nil {
		t.Fatalf("reading the submitted run: %v", err)
	}
	if _, err := h.handle(t); err != nil {
		t.Fatalf("handle: %v", err)
	}
	return result.Job.ID
}

func saveID(t *testing.T, pool *pgxpool.Pool, jobID string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`SELECT user_save_id FROM reelpin.processing_jobs WHERE id = $1`,
		jobID).Scan(&id); err != nil {
		t.Fatalf("reading the completed job: %v", err)
	}
	return id
}

// TestASubmittedReelIsReadableAfterItsRun is the end of the happy path nobody
// tested: the id the completed job hands the app has to resolve through the
// reel reader, which still reads the legacy table.
func TestASubmittedReelIsReadableAfterItsRun(t *testing.T) {
	h := newBareHarness(t)
	jobID := submitAndRun(t, h, userA)

	id := saveID(t, h.pool, jobID)
	record, err := postgres.NewReels(h.pool).Get(context.Background(), userA, id)
	if err != nil {
		t.Fatalf("GET /api/v2/reels/%s: %v", id, err)
	}
	if record.Title != "Artjuna cafe" || record.Category != "Food" {
		t.Fatalf("read back title=%q category=%q", record.Title, record.Category)
	}
	if record.Transcript == "" {
		t.Error("the detail read carries no transcript")
	}
	if record.UserID != userA {
		t.Errorf("read back user %q, want %q", record.UserID, userA)
	}
}

// TestTheLegacyRowKeepsTheCanonicalSaveID pins the identity the whole coexistence
// window rests on: deep links, app caches and the backfill all hold this id.
func TestTheLegacyRowKeepsTheCanonicalSaveID(t *testing.T) {
	h := newBareHarness(t)
	jobID := submitAndRun(t, h, userA)

	var canonical, legacy uuid.UUID
	if err := h.pool.QueryRow(context.Background(), `
		SELECT s.id, r.id
		FROM reelpin.user_saves s
		JOIN reelpin.processing_jobs j ON j.user_save_id = s.id
		JOIN public.reels r ON r.id = s.id
		WHERE j.id = $1`, jobID).Scan(&canonical, &legacy); err != nil {
		t.Fatalf("matching the two rows: %v", err)
	}
	if canonical != legacy {
		t.Fatalf("canonical %s, legacy %s", canonical, legacy)
	}
}

func TestRedeliveryDoesNotDuplicateTheLegacyRow(t *testing.T) {
	h := newBareHarness(t)
	jobID := submitAndRun(t, h, userA)

	// The broker is at-least-once: the same message arrives again.
	if _, err := h.handle(t); err != nil {
		t.Fatalf("redelivery: %v", err)
	}

	var rows int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM public.reels WHERE user_id = $1`, userA).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("legacy rows = %d after redelivery, want 1", rows)
	}

	// And the id the app already holds is still the one it holds.
	id := saveID(t, h.pool, jobID)
	if _, err := postgres.NewReels(h.pool).Get(context.Background(), userA, id); err != nil {
		t.Fatalf("reading back after redelivery: %v", err)
	}
	if _, err := postgres.NewReels(h.pool).Get(context.Background(), userB, id); !errors.Is(err, reels.ErrNotFound) {
		t.Fatalf("another user read the save: %v", err)
	}
}
