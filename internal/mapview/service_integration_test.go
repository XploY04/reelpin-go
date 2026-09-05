//go:build integration

package mapview

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
CREATE TABLE public.manual_map_pins (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id TEXT NOT NULL,
    google_place_id TEXT,
    name TEXT NOT NULL,
    address TEXT,
    latitude DOUBLE PRECISION NOT NULL,
    longitude DOUBLE PRECISION NOT NULL,
    google_maps_url TEXT,
    category TEXT,
    subcategory TEXT,
    secondary_categories JSONB DEFAULT '[]'::jsonb,
    place_types JSONB DEFAULT '[]'::jsonb,
    metadata JSONB DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE public.hidden_reel_map_pins (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id TEXT NOT NULL,
    reel_id UUID NOT NULL REFERENCES public.reels(id) ON DELETE CASCADE,
    location_index INT NOT NULL,
    location_fingerprint TEXT,
    hidden_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
`

const (
	userA = "11111111-1111-4111-8111-111111111111"
	userB = "22222222-2222-4222-8222-222222222222"
)

// stubPlaces stands in for Google. No test calls a provider.
type stubPlaces struct {
	results     []Place
	details     Place
	err         error
	calls       int
	lastSession string
}

func (s *stubPlaces) Search(_ context.Context, _, sessionToken string, _ int) ([]Place, error) {
	s.calls++
	s.lastSession = sessionToken
	return s.results, s.err
}

func (s *stubPlaces) Details(_ context.Context, placeID, sessionToken string) (Place, error) {
	s.calls++
	s.lastSession = sessionToken
	if s.err != nil {
		return Place{}, s.err
	}
	details := s.details
	details.PlaceID = placeID
	return details, nil
}

func testService(t *testing.T) (*Service, *pgxpool.Pool, *stubPlaces) {
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

	name := "reelpin_map_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
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

	places := &stubPlaces{}
	return NewService(pool, places, func() time.Time { return time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC) }), pool, places
}

func seedReel(t *testing.T, pool *pgxpool.Pool, userID, title, category, locations string, daysAgo int) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO public.reels (user_id, url, title, category, subcategory, source_platform, locations, created_at)
		VALUES ($1, $2, $3, $4, 'cafes', 'instagram', $5::jsonb, now() - make_interval(days => $6))
		RETURNING id::text`,
		userID, "https://example.com/"+title, title, category, locations, daysAgo).Scan(&id); err != nil {
		t.Fatalf("seeding a reel: %v", err)
	}
	return id
}

const artjuna = `[{"name":"Artjuna","city":"Anjuna","country":"India","latitude":15.58,"longitude":73.74}]`
const twoPlaces = `[{"name":"Artjuna","city":"Anjuna","latitude":15.58,"longitude":73.74},
                    {"name":"Bomras","city":"Anjuna","latitude":15.55,"longitude":73.75}]`

func TestMapMergesReelsAndManualPins(t *testing.T) {
	service, pool, _ := testService(t)
	ctx := context.Background()

	seedReel(t, pool, userA, "cafes", "food", twoPlaces, 1)
	seedReel(t, pool, userB, "not mine", "food", artjuna, 1)
	if _, err := pool.Exec(ctx, `
		INSERT INTO public.manual_map_pins (user_id, google_place_id, name, address, latitude, longitude)
		VALUES ($1, 'place-1', 'Bakery', 'Panaji', 15.49, 73.82)`, userA); err != nil {
		t.Fatalf("seeding a pin: %v", err)
	}

	response, err := service.Map(ctx, userA, "", nil)
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if len(response.MapItems) != 3 {
		t.Fatalf("items = %d, want two reel places and one manual pin", len(response.MapItems))
	}
	for _, item := range response.MapItems {
		if item.ReelID != "" && strings.Contains(item.Title, "not mine") {
			t.Fatal("another user's pin appeared on this map")
		}
	}
}

func TestHidingAPlaceAndRemovingAPin(t *testing.T) {
	service, pool, _ := testService(t)
	ctx := context.Background()

	reelID := seedReel(t, pool, userA, "cafes", "food", twoPlaces, 1)
	var pinID string
	pool.QueryRow(ctx, `
		INSERT INTO public.manual_map_pins (user_id, google_place_id, name, latitude, longitude)
		VALUES ($1, 'place-1', 'Bakery', 15.49, 73.82) RETURNING id::text`, userA).Scan(&pinID)

	if err := service.HideOrRemove(ctx, userA, "reel:"+reelID+":0"); err != nil {
		t.Fatalf("hiding: %v", err)
	}
	// Hiding twice is the same fact, not an error.
	if err := service.HideOrRemove(ctx, userA, "reel:"+reelID+":0"); err != nil {
		t.Fatalf("hiding twice: %v", err)
	}

	var fingerprint *string
	pool.QueryRow(ctx, `SELECT location_fingerprint FROM public.hidden_reel_map_pins`).Scan(&fingerprint)
	if fingerprint == nil || *fingerprint == "" {
		t.Error("hiding recorded no fingerprint, so reprocessing would unhide it")
	}

	if err := service.HideOrRemove(ctx, userA, "manual:"+pinID); err != nil {
		t.Fatalf("removing a pin: %v", err)
	}

	response, err := service.Map(ctx, userA, "", nil)
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if len(response.MapItems) != 1 {
		t.Fatalf("items = %d, want only the surviving place", len(response.MapItems))
	}

	// Another user cannot hide or remove this user's things.
	if err := service.HideOrRemove(ctx, userB, "reel:"+reelID+":1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-user hide = %v, want ErrNotFound", err)
	}
	if err := service.HideOrRemove(ctx, userA, "reel:"+reelID+":99"); !errors.Is(err, ErrNotFound) {
		t.Errorf("hiding a place that does not exist = %v, want ErrNotFound", err)
	}
}

func TestSearchPrefersSavedPlacesAndThenAsksTheProvider(t *testing.T) {
	service, pool, places := testService(t)
	ctx := context.Background()

	seedReel(t, pool, userA, "cafes", "food", artjuna, 1)
	places.results = []Place{{PlaceID: "place-9", Name: "Artjuna Bakery", Address: "Anjuna"}}

	response, err := service.Search(ctx, userA, "artjuna", "", "session-1", 8)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(response.Results) != 2 {
		t.Fatalf("results = %d, want the saved place then the provider's", len(response.Results))
	}
	if response.Results[0].ResultType != "saved" || response.Results[0].CanPin {
		t.Errorf("first result = %+v, want a saved place that cannot be pinned again", response.Results[0])
	}
	if response.Results[1].ResultType != "place" || !response.Results[1].CanPin {
		t.Errorf("second result = %+v", response.Results[1])
	}
	if places.lastSession != "session-1" {
		t.Errorf("session token = %q, want it forwarded so the pair bills as one session", places.lastSession)
	}

	// A short query never reaches the provider.
	before := places.calls
	short, err := service.Search(ctx, userA, "a", "", "", 8)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if short.SearchMode != "empty" || places.calls != before {
		t.Errorf("a one-character query cost a provider call")
	}
}

func TestSearchSurvivesAProviderOutage(t *testing.T) {
	service, pool, places := testService(t)
	ctx := context.Background()

	seedReel(t, pool, userA, "cafes", "food", artjuna, 1)
	places.err = errors.New("places is down")

	response, err := service.Search(ctx, userA, "artjuna", "", "", 8)
	if err != nil {
		t.Fatalf("a provider outage failed the search: %v", err)
	}
	if len(response.Results) != 1 {
		t.Fatalf("results = %d, want the user's own place", len(response.Results))
	}
}

func TestCreatePinIsIdempotent(t *testing.T) {
	service, pool, places := testService(t)
	ctx := context.Background()
	places.details = Place{Name: "Bakery", Address: "Panaji", Latitude: 15.49, Longitude: 73.82,
		GoogleMapsURL: "https://maps.example.com/1", Types: []string{"bakery"}}

	first, err := service.CreatePin(ctx, userA, "place-1", "session-1")
	if err != nil {
		t.Fatalf("CreatePin: %v", err)
	}
	if first.SourceType != "manual_pin" || first.PlaceName != "Bakery" {
		t.Fatalf("pin = %+v", first)
	}

	// Tapping save twice must not produce two pins, and must not pay for a
	// second details call.
	before := places.calls
	second, err := service.CreatePin(ctx, userA, "place-1", "session-1")
	if err != nil {
		t.Fatalf("second CreatePin: %v", err)
	}
	if *second.MapItemID != *first.MapItemID {
		t.Errorf("a second pin was created: %v vs %v", second.MapItemID, first.MapItemID)
	}
	if places.calls != before {
		t.Errorf("the provider was called again for a place already saved")
	}

	var count int
	pool.QueryRow(ctx, `SELECT count(*) FROM public.manual_map_pins`).Scan(&count)
	if count != 1 {
		t.Fatalf("pins = %d, want 1", count)
	}
}

func TestDiscover(t *testing.T) {
	service, pool, _ := testService(t)
	ctx := context.Background()

	seedReel(t, pool, userA, "today", "food", artjuna, 0)
	seedReel(t, pool, userA, "yesterday", "travel", artjuna, 1)
	seedReel(t, pool, userA, "older", "food", "[]", 5)
	seedReel(t, pool, userB, "not mine", "food", artjuna, 0)

	response, err := service.Discover(ctx, userA, 0, 2, "")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	if response.RecentSavesCount != 3 {
		t.Errorf("recent_saves_count = %d, want this user's three", response.RecentSavesCount)
	}
	if len(response.RecentSaves) != 2 || !response.Pagination.HasMore {
		t.Errorf("page = %d, has_more = %v", len(response.RecentSaves), response.Pagination.HasMore)
	}
	if len(response.SavedDates) != 3 {
		t.Errorf("saved_dates = %v, want one per day", response.SavedDates)
	}
	if len(response.SavedDates) > 1 && response.SavedDates[0] < response.SavedDates[1] {
		t.Error("saved dates are not newest first")
	}
	if len(response.CategoryGrid) != 2 {
		t.Errorf("category grid = %+v, want food and travel", response.CategoryGrid)
	}
	if len(response.QuickSearchPrompts) == 0 {
		t.Error("no quick search prompts were produced")
	}
	// Folder discovery has been dead in Python since a shadowed function
	// stopped returning anything, and no client reads it.
	if response.Folders == nil || len(response.Folders) != 0 {
		t.Errorf("folders = %v, want an empty list", response.Folders)
	}

	selected := response.SavedDates[0]
	filtered, err := service.Discover(ctx, userA, 0, 20, selected)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if filtered.SelectedDate == nil || *filtered.SelectedDate != selected {
		t.Errorf("selected_date = %v", filtered.SelectedDate)
	}
	if len(filtered.ReelsForSelectedDate) != 1 {
		t.Errorf("reels for %s = %d, want one", selected, len(filtered.ReelsForSelectedDate))
	}
}

func TestDiscoverOnAnEmptyLibrary(t *testing.T) {
	service, _, _ := testService(t)

	response, err := service.Discover(context.Background(), userA, 0, 20, "")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if response.RecentSaves == nil || response.SavedDates == nil ||
		response.CategoryGrid == nil || response.QuickSearchPrompts == nil || response.Folders == nil {
		t.Fatalf("an empty library produced nil lists, which serialize as null: %+v", response)
	}
	if response.Pagination.HasMore {
		t.Error("an empty library has more pages")
	}
}
