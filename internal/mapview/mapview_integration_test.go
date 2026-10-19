//go:build integration

package mapview

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/geo"
	"github.com/XploY04/reelpin-go/internal/googleplaces"
	"github.com/XploY04/reelpin-go/internal/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	userA = "11111111-1111-4111-8111-111111111111"
	userB = "22222222-2222-4222-8222-222222222222"
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

	name := "reelpin_mapview_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
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
		CREATE TABLE auth.users (id UUID PRIMARY KEY, email TEXT, created_at TIMESTAMPTZ DEFAULT now())`); err != nil {
		t.Fatalf("creating auth.users: %v", err)
	}
	if _, err := migrations.Up(ctx, parsed.String()); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO auth.users (id) VALUES ($1), ($2)`, userA, userB); err != nil {
		t.Fatal(err)
	}
	return New(pool, time.Now), pool
}

// seedLocation creates content with one location and saves it for each user.
func seedLocation(t *testing.T, pool *pgxpool.Pool, sourceID, name string, lat, lon float64, savers ...string) string {
	t.Helper()
	ctx := context.Background()

	var contentID, versionID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO reelpin.contents
			(source_platform, source_content_type, source_content_id,
			 normalized_url, normalized_url_hash, access_scope_hash)
		VALUES ('instagram', 'reel', $1, 'https://example.com/'||$1, $1, 'public')
		RETURNING id::text`, sourceID).Scan(&contentID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO reelpin.content_versions
			(content_id, processor_version, prompt_version, schema_version, model_version, title)
		VALUES ($1, 'v1', 'p1', 's1', 'm1', $2)
		RETURNING id::text`, contentID, name).Scan(&versionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE reelpin.contents SET current_version_id = $2 WHERE id = $1`, contentID, versionID); err != nil {
		t.Fatal(err)
	}

	var locationID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO reelpin.content_locations
			(content_version_id, ordinal, name, point, source, confidence)
		VALUES ($1, 0, $2, ST_MakePoint($4, $3)::geography, 'extraction', 0.9)
		RETURNING id::text`, versionID, name, lat, lon).Scan(&locationID); err != nil {
		t.Fatal(err)
	}

	for _, user := range savers {
		if _, err := pool.Exec(ctx,
			`INSERT INTO reelpin.user_saves (user_id, content_id) VALUES ($1, $2)`,
			user, contentID); err != nil {
			t.Fatal(err)
		}
	}
	return locationID
}

func TestOnlyReachableLocationsAppear(t *testing.T) {
	service, pool := testService(t)
	ctx := context.Background()

	seedLocation(t, pool, "MINE1", "Artjuna", 15.58, 73.74, userA)
	seedLocation(t, pool, "THEIRS1", "Someone else's", 15.59, 73.75, userB)

	bounds, err := geo.NewBounds(15.0, 73.0, 16.0, 74.0)
	if err != nil {
		t.Fatal(err)
	}

	mine, err := service.Pins(ctx, userA, bounds)
	if err != nil {
		t.Fatalf("Pins: %v", err)
	}
	if len(mine) != 1 || mine[0].Name != "Artjuna" {
		t.Fatalf("pins = %+v; a location is visible only through the user's own save", mine)
	}

	theirs, err := service.Pins(ctx, userB, bounds)
	if err != nil {
		t.Fatal(err)
	}
	if len(theirs) != 1 || theirs[0].Name != "Someone else's" {
		t.Fatalf("the other user saw %+v", theirs)
	}
}

func TestTwoUsersSharingContentEachSeeItOnce(t *testing.T) {
	service, pool := testService(t)
	ctx := context.Background()

	seedLocation(t, pool, "SHARED1", "Shared cafe", 15.58, 73.74, userA, userB)
	bounds, _ := geo.NewBounds(15.0, 73.0, 16.0, 74.0)

	for _, user := range []string{userA, userB} {
		pins, err := service.Pins(ctx, user, bounds)
		if err != nil {
			t.Fatal(err)
		}
		if len(pins) != 1 {
			t.Fatalf("user %s saw %d pins for one shared location", user, len(pins))
		}
		if pins[0].ReelID == nil {
			t.Error("a content pin carries no reel id to open")
		}
	}
}

func TestHidingAPinIsPersonal(t *testing.T) {
	service, pool := testService(t)
	ctx := context.Background()

	locationID := seedLocation(t, pool, "SHARED2", "Shared cafe", 15.58, 73.74, userA, userB)
	bounds, _ := geo.NewBounds(15.0, 73.0, 16.0, 74.0)

	if err := service.HidePin(ctx, userA, locationID, true); err != nil {
		t.Fatalf("hide: %v", err)
	}

	mine, _ := service.Pins(ctx, userA, bounds)
	if len(mine) != 0 {
		t.Fatalf("a hidden pin still appeared: %+v", mine)
	}
	theirs, _ := service.Pins(ctx, userB, bounds)
	if len(theirs) != 1 {
		t.Fatal("one user's hidden pin removed it from another user's map")
	}

	// Unhiding brings it back for that user only.
	if err := service.HidePin(ctx, userA, locationID, false); err != nil {
		t.Fatal(err)
	}
	mine, _ = service.Pins(ctx, userA, bounds)
	if len(mine) != 1 {
		t.Fatal("unhiding did not restore the pin")
	}
}

func TestAStrangerCannotHideOrEnumerate(t *testing.T) {
	service, pool := testService(t)
	ctx := context.Background()

	locationID := seedLocation(t, pool, "MINE2", "Artjuna", 15.58, 73.74, userA)

	// userB has no save for this content, so the id is not theirs to touch.
	if err := service.HidePin(ctx, userB, locationID, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want not found: hiding must not confirm an id exists", err)
	}
}

func TestManualPinsAreOwnedAndPrivate(t *testing.T) {
	service, _ := testService(t)
	ctx := context.Background()

	point, _ := geo.NewPoint(15.58, 73.74)
	pin, err := service.CreateManualPin(ctx, userA, "My spot", nil, point)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if pin.Kind != "manual" || pin.ReelID != nil {
		t.Fatalf("pin = %+v", pin)
	}

	bounds, _ := geo.NewBounds(15.0, 73.0, 16.0, 74.0)
	theirs, _ := service.Pins(ctx, userB, bounds)
	if len(theirs) != 0 {
		t.Fatal("a manual pin was visible to another user")
	}

	if err := service.DeleteManualPin(ctx, userB, pin.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another user deleted a manual pin: %v", err)
	}
	if err := service.DeleteManualPin(ctx, userA, pin.ID); err != nil {
		t.Fatalf("the owner could not delete it: %v", err)
	}
}

func TestABoxCrossingTheAntimeridianFindsThePacific(t *testing.T) {
	service, pool := testService(t)
	ctx := context.Background()

	// Either side of the 180th meridian, plus one far away that must not match.
	seedLocation(t, pool, "FIJI1", "Fiji", -18.0, 178.0, userA)
	seedLocation(t, pool, "SAMOA1", "Samoa", -13.8, -172.0, userA)
	seedLocation(t, pool, "GOA1", "Goa", 15.58, 73.74, userA)

	pacific, err := geo.NewBounds(-25, 170, -5, -170)
	if err != nil {
		t.Fatal(err)
	}
	if !pacific.CrossesAntimeridian() {
		t.Fatal("the test box does not cross the antimeridian")
	}

	pins, err := service.Pins(ctx, userA, pacific)
	if err != nil {
		t.Fatalf("Pins: %v", err)
	}
	if len(pins) != 2 {
		names := []string{}
		for _, pin := range pins {
			names = append(names, pin.Name)
		}
		t.Fatalf("pins = %v, want both sides of the meridian and nothing else", names)
	}
	for _, pin := range pins {
		if pin.Name == "Goa" {
			t.Fatal("the query returned the rest of the world instead of the Pacific")
		}
	}
}

func TestNearbyOrdersByRealDistance(t *testing.T) {
	service, pool := testService(t)
	ctx := context.Background()

	seedLocation(t, pool, "NEAR1", "Near", 15.58, 73.74, userA)
	seedLocation(t, pool, "MID1", "Middle", 15.68, 73.74, userA)
	seedLocation(t, pool, "FAR1", "Far", 16.58, 73.74, userA)

	centre, _ := geo.NewPoint(15.58, 73.74)
	pins, err := service.Nearby(ctx, userA, centre, 50_000, 10)
	if err != nil {
		t.Fatalf("Nearby: %v", err)
	}
	if len(pins) != 2 {
		t.Fatalf("pins = %d, want the two inside 50km", len(pins))
	}
	if pins[0].Name != "Near" || pins[1].Name != "Middle" {
		t.Fatalf("order = %s, %s; want nearest first", pins[0].Name, pins[1].Name)
	}
}

func TestTheSpatialIndexIsUsed(t *testing.T) {
	service, pool := testService(t)
	ctx := context.Background()
	_ = service

	// Enough rows that the planner would notice a sequential scan. Each needs
	// its own source identity: the unique index is doing its job otherwise.
	for i := 0; i < 400; i++ {
		seedLocation(t, pool, fmt.Sprintf("BULK%04d", i), "Bulk",
			15.0+float64(i%80)/100, 73.0+float64(i%80)/100, userA)
	}
	if _, err := pool.Exec(ctx, `ANALYZE reelpin.content_locations`); err != nil {
		t.Fatal(err)
	}

	var plan string
	rows, err := pool.Query(ctx, `
		EXPLAIN SELECT id FROM reelpin.content_locations
		WHERE ST_Intersects(point, ST_MakeEnvelope(73.0, 15.0, 73.2, 15.2, 4326)::geography)`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		plan += line + "\n"
	}
	rows.Close()

	if !strings.Contains(plan, "content_locations_point_idx") {
		t.Fatalf("the spatial index is not used:\n%s", plan)
	}
}

func TestAPlacePinIsOnePerPlaceAndPerUser(t *testing.T) {
	service, _ := testService(t)
	ctx := context.Background()

	place := PlacePin{
		Place: googleplaces.Place{
			ID: "ChIJ-artjuna", Name: "Artjuna", Address: "Anjuna, Goa",
			Latitude: 15.5763, Longitude: 73.7408, MapsURL: "https://maps.google.com/?cid=1",
			Types: []string{"cafe"},
		},
		Category: "Other", Subcategory: "Other",
	}
	first, err := service.UpsertPlacePin(ctx, userA, place)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if first.Kind != "manual" || first.GooglePlaceID == nil || *first.GooglePlaceID != "ChIJ-artjuna" ||
		first.Category == nil || *first.Category != "Other" || len(first.PlaceTypes) != 1 {
		t.Fatalf("pin = %+v", first)
	}

	place.Place.Name = "Artjuna Cafe"
	second, err := service.UpsertPlacePin(ctx, userA, place)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if second.ID != first.ID || second.Name != "Artjuna Cafe" {
		t.Fatalf("second pin = %+v, want the first refreshed", second)
	}

	found, err := service.PinByPlace(ctx, userA, "ChIJ-artjuna")
	if err != nil || found.ID != first.ID {
		t.Fatalf("PinByPlace = %+v, %v", found, err)
	}
	if _, err := service.PinByPlace(ctx, userB, "ChIJ-artjuna"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another user found the pin: %v", err)
	}

	theirs, err := service.UpsertPlacePin(ctx, userB, place)
	if err != nil {
		t.Fatalf("another user pinning the same place: %v", err)
	}
	if theirs.ID == first.ID {
		t.Fatal("two users share one pin")
	}

	// A pin dropped by hand has no place id, so any number of them coexist
	// with the unique constraint.
	point, _ := geo.NewPoint(15.58, 73.74)
	for range 2 {
		if _, err := service.CreateManualPin(ctx, userA, "My spot", nil, point); err != nil {
			t.Fatalf("a hand-dropped pin: %v", err)
		}
	}

	bounds, _ := geo.NewBounds(15.0, 73.0, 16.0, 74.0)
	pins, err := service.Pins(ctx, userA, bounds)
	if err != nil {
		t.Fatal(err)
	}
	withPlace := 0
	for _, pin := range pins {
		if pin.PlaceTypes == nil {
			t.Errorf("pin %s has null place types", pin.ID)
		}
		if pin.GooglePlaceID != nil {
			withPlace++
		}
	}
	if len(pins) != 3 || withPlace != 1 {
		t.Fatalf("viewport has %d pins, %d from a place; want 3 and 1", len(pins), withPlace)
	}
}

func TestPlaceCandidatesAreTheUsersVisiblePins(t *testing.T) {
	service, pool := testService(t)
	ctx := context.Background()

	seedLocation(t, pool, "MINE3", "Artjuna", 15.58, 73.74, userA)
	hidden := seedLocation(t, pool, "MINE4", "Hidden cafe", 15.59, 73.75, userA)
	seedLocation(t, pool, "THEIRS3", "Someone else's", 15.60, 73.76, userB)
	if err := service.HidePin(ctx, userA, hidden, true); err != nil {
		t.Fatal(err)
	}
	address := "Small Vagator, Goa"
	point, _ := geo.NewPoint(15.60, 73.73)
	if _, err := service.CreateManualPin(ctx, userA, "Thalassa", &address, point); err != nil {
		t.Fatal(err)
	}

	candidates, err := service.PlaceCandidates(ctx, userA)
	if err != nil {
		t.Fatalf("PlaceCandidates: %v", err)
	}
	byName := map[string]Candidate{}
	for _, candidate := range candidates {
		byName[candidate.Name] = candidate
	}
	if len(candidates) != 2 {
		t.Fatalf("candidates = %v, want the visible content pin and the manual pin", byName)
	}
	content, manual := byName["Artjuna"], byName["Thalassa"]
	if content.Kind != "content" || content.Title != "Artjuna" || content.ReelID == nil {
		t.Errorf("content candidate = %+v", content)
	}
	if manual.Kind != "manual" || manual.Title != "Thalassa" || manual.Summary != address {
		t.Errorf("manual candidate = %+v", manual)
	}
}

func TestDeleteItemHidesReelPinsAndDeletesManualOnes(t *testing.T) {
	service, pool := testService(t)
	ctx := context.Background()

	seedLocation(t, pool, "ITEM1", "Shared cafe", 15.58, 73.74, userA, userB)
	var saveID string
	if err := pool.QueryRow(ctx,
		`SELECT id::text FROM reelpin.user_saves WHERE user_id = $1`, userA).Scan(&saveID); err != nil {
		t.Fatal(err)
	}
	bounds, _ := geo.NewBounds(15.0, 73.0, 16.0, 74.0)

	if err := service.DeleteItem(ctx, userA, "reel:"+saveID+":0"); err != nil {
		t.Fatalf("hiding a reel item: %v", err)
	}
	if mine, _ := service.Pins(ctx, userA, bounds); len(mine) != 0 {
		t.Fatalf("a removed reel item still appeared: %+v", mine)
	}
	if theirs, _ := service.Pins(ctx, userB, bounds); len(theirs) != 1 {
		t.Fatal("removing an item changed another user's map")
	}

	for _, id := range []string{"reel:" + saveID + ":7", "reel:" + saveID + ":x"} {
		if err := service.DeleteItem(ctx, userA, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", id, err)
		}
	}
	if err := service.DeleteItem(ctx, userB, "reel:"+saveID+":0"); !errors.Is(err, ErrNotFound) {
		t.Errorf("another user's save: err = %v, want ErrNotFound", err)
	}

	point, _ := geo.NewPoint(15.5, 73.7)
	pin, err := service.CreateManualPin(ctx, userA, "Mine", nil, point)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.DeleteItem(ctx, userB, "manual:"+pin.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("another user's manual pin: err = %v, want ErrNotFound", err)
	}
	if err := service.DeleteItem(ctx, userA, "manual:"+pin.ID); err != nil {
		t.Fatalf("deleting a manual item: %v", err)
	}
}

func TestDeleteItemHidesABareLocationIDForTheCallerOnly(t *testing.T) {
	service, pool := testService(t)
	ctx := context.Background()

	shared := seedLocation(t, pool, "BARE1", "Shared cafe", 15.58, 73.74, userA, userB)
	mineOnly := seedLocation(t, pool, "BARE2", "Artjuna", 15.59, 73.75, userA)
	bounds, _ := geo.NewBounds(15.0, 73.0, 16.0, 74.0)

	pins, err := service.Pins(ctx, userA, bounds)
	if err != nil {
		t.Fatal(err)
	}
	listed := false
	for _, pin := range pins {
		listed = listed || pin.ID == shared
	}
	if !listed {
		t.Fatalf("the map list does not return the bare location id %s: %+v", shared, pins)
	}

	if err := service.DeleteItem(ctx, userA, shared); err != nil {
		t.Fatalf("hiding by bare location id: %v", err)
	}
	if mine, _ := service.Pins(ctx, userA, bounds); len(mine) != 1 || mine[0].ID != mineOnly {
		t.Fatalf("pins after the delete = %+v, want only %s", mine, mineOnly)
	}
	if theirs, _ := service.Pins(ctx, userB, bounds); len(theirs) != 1 || theirs[0].ID != shared {
		t.Fatal("removing an item changed another user's map")
	}

	for name, id := range map[string]string{
		"another user's location": mineOnly,
		"an unknown location":     "99999999-9999-4999-8999-999999999999",
	} {
		if err := service.DeleteItem(ctx, userB, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", name, err)
		}
	}
	if mine, _ := service.Pins(ctx, userA, bounds); len(mine) != 1 {
		t.Fatal("a stranger's delete hid the owner's pin")
	}
}
