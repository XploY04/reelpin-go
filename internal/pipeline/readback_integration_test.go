//go:build integration

package pipeline

import (
	"context"
	"errors"
	"testing"

	"github.com/XploY04/reelpin-go/internal/ai"
	"github.com/XploY04/reelpin-go/internal/enqueue"
	"github.com/XploY04/reelpin-go/internal/geo"
	"github.com/XploY04/reelpin-go/internal/mapview"
	"github.com/XploY04/reelpin-go/internal/postgres"
	"github.com/XploY04/reelpin-go/internal/reels"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

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

// TestASubmittedReelIsReadableWithNoLegacyTables runs on a database shaped
// like Neon: the migrations alone, no public.reels and no other Python table.
// The id the completed job hands the app has to resolve through the reel
// reader, the list and the facets, with every field the run produced.
func TestASubmittedReelIsReadableWithNoLegacyTables(t *testing.T) {
	h := newBareHarness(t)
	ctx := context.Background()
	latitude, longitude := 15.5834, 73.7407
	h.extractor.extraction.PeopleMentioned = []string{"Artjuna"}
	h.extractor.extraction.ActionableItems = []string{"Visit for breakfast"}
	h.extractor.extraction.Locations = []ai.Location{{
		Name: "Artjuna", City: "Anjuna", Country: "India",
		Latitude: &latitude, Longitude: &longitude,
	}}

	var legacyTables int
	if err := h.pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_tables
		WHERE schemaname = 'public' AND tablename IN ('reels', 'processing_jobs', 'user_entitlements')`,
	).Scan(&legacyTables); err != nil {
		t.Fatal(err)
	}
	if legacyTables != 0 {
		t.Fatalf("the database has %d Python tables, want none", legacyTables)
	}

	jobID := submitAndRun(t, h, userA)
	id := saveID(t, h.pool, jobID)

	reader := postgres.NewReels(h.pool)
	record, err := reader.Get(ctx, userA, id)
	if err != nil {
		t.Fatalf("GET /api/v2/reels/%s: %v", id, err)
	}
	if record.ID != id.String() || record.UserID != userA {
		t.Errorf("read back id=%s user=%s", record.ID, record.UserID)
	}
	if record.Title != "Artjuna cafe" || record.Category != "Food" || record.Subcategory != "Cafes" {
		t.Errorf("read back title=%q category=%q subcategory=%q",
			record.Title, record.Category, record.Subcategory)
	}
	if record.URL != submittedReel {
		t.Errorf("url = %q, want the submitted link", record.URL)
	}
	if record.ThumbnailURL == nil || *record.ThumbnailURL != "https://example.com/thumb.jpg" {
		t.Errorf("thumbnail_url = %v", record.ThumbnailURL)
	}
	if len(record.SecondaryCategories) != 2 || record.SecondaryCategories[0] != "cafe" {
		t.Errorf("secondary_categories = %v", record.SecondaryCategories)
	}
	if len(record.KeyFacts) != 1 || record.KeyFacts[0] != "Opens at eight" {
		t.Errorf("key_facts = %v", record.KeyFacts)
	}
	if len(record.PeopleMentioned) != 1 || len(record.ActionableItems) != 1 {
		t.Errorf("people/actions = %v/%v", record.PeopleMentioned, record.ActionableItems)
	}
	if len(record.Locations) != 1 || record.Locations[0].Latitude == nil ||
		*record.Locations[0].Latitude != latitude {
		t.Fatalf("locations = %+v", record.Locations)
	}
	mapReader := mapview.New(h.pool, nil)
	bounds, err := geo.NewBounds(15, 73, 16, 74)
	if err != nil {
		t.Fatal(err)
	}
	pins, _, err := mapReader.Pins(ctx, userA, bounds, "")
	if err != nil || len(pins) != 1 {
		t.Fatalf("map pins = %+v, error = %v", pins, err)
	}
	if pins[0].ReelID == nil || *pins[0].ReelID != id.String() || pins[0].Latitude != latitude {
		t.Errorf("map pin = %+v", pins[0])
	}
	otherPins, _, err := mapReader.Pins(ctx, userB, bounds, "")
	if err != nil || len(otherPins) != 0 {
		t.Fatalf("other user's pins = %+v, error = %v", otherPins, err)
	}
	if record.ParseStatus == nil || *record.ParseStatus != "parsed" {
		t.Errorf("parse_status = %v, want parsed", record.ParseStatus)
	}
	if record.ProcessingVersion == nil || *record.ProcessingVersion != ProcessorVersion {
		t.Errorf("processing_version = %v, want %s", record.ProcessingVersion, ProcessorVersion)
	}
	if record.Transcript == "" {
		t.Error("the detail read carries no transcript")
	}

	listed, err := reader.List(ctx, userA, reels.ListOptions{Limit: 20, Category: "Food"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != id.String() {
		t.Fatalf("listed = %+v, want the one save", listed)
	}
	facets, err := reader.Facets(ctx, userA)
	if err != nil {
		t.Fatalf("Facets: %v", err)
	}
	if len(facets) != 1 || facets[0].Count != 1 {
		t.Fatalf("facets = %+v, want one row of one", facets)
	}
}

func TestRedeliveryKeepsOneSaveAndItsID(t *testing.T) {
	h := newBareHarness(t)
	jobID := submitAndRun(t, h, userA)
	id := saveID(t, h.pool, jobID)

	// The broker is at-least-once: the same message arrives again.
	if _, err := h.handle(t); err != nil {
		t.Fatalf("redelivery: %v", err)
	}

	reader := postgres.NewReels(h.pool)
	listed, err := reader.List(context.Background(), userA, reels.ListOptions{Limit: 20})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != id.String() {
		t.Fatalf("listed = %+v after redelivery, want the one save %s", listed, id)
	}
	if _, err := reader.Get(context.Background(), userB, id); !errors.Is(err, reels.ErrNotFound) {
		t.Fatalf("another user read the save: %v", err)
	}
}
