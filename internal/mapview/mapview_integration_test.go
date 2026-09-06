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
