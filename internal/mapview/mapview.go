// Package mapview answers "what is on my map". Every query starts from the
// authenticated user's saves: a location is only visible because the user
// saved the content it came from, so ownership is a join, not a filter added
// afterwards.
package mapview

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/geo"
	"github.com/XploY04/reelpin-go/internal/reels"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is a pin that is not this user's to see or change.
var ErrNotFound = errors.New("pin not found")

// MaxPins bounds one viewport's answer. A user panning out to the whole world
// gets the nearest cluster, not every save they have ever made.
const MaxPins = 500

// Pin is one thing on the map. Manual pins and content pins share a shape so
// the client draws one list.
type Pin struct {
	ID        string  `json:"id"`
	Kind      string  `json:"kind"`
	Name      string  `json:"name"`
	Address   *string `json:"address"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	// ReelID is the save this pin came from. Null for a manual pin.
	ReelID     *string  `json:"reel_id"`
	Confidence *float64 `json:"confidence"`
	// Category comes from the save's extraction for a content pin and is
	// assigned when a manual pin is made from a Google place.
	Category *string `json:"category"`
	// SubCategory is the name every reel-bearing response uses. Subcategory
	// repeats it under the name pins sent first; it is deprecated and goes
	// after one release.
	SubCategory *string `json:"sub_category"`
	Subcategory *string `json:"subcategory"`
	// The Google fields are set only on a manual pin made from a place.
	GooglePlaceID *string  `json:"google_place_id"`
	GoogleMapsURL *string  `json:"google_maps_url"`
	PlaceTypes    []string `json:"place_types"`
}

// contentPinColumns and manualPinColumns are one row shape in the order
// pinTargets scans it, so every query that answers pins answers the same Pin.
// A content row needs v, the version its location came from.
const (
	contentPinColumns = `l.id::text, 'content', l.name, l.address,
		ST_Y(l.point::geometry), ST_X(l.point::geometry), s.id::text, l.confidence,
		v.raw_extraction->>'category', v.raw_extraction->>'subcategory',
		NULL::text, NULL::text, '{}'::text[]`
	manualPinColumns = `m.id::text, 'manual', m.name, m.address,
		ST_Y(m.point::geometry), ST_X(m.point::geometry), NULL::text, NULL::real,
		m.category, m.subcategory,
		m.google_place_id, m.google_maps_url, m.place_types`
)

func pinTargets(pin *Pin) []any {
	return []any{&pin.ID, &pin.Kind, &pin.Name, &pin.Address,
		&pin.Latitude, &pin.Longitude, &pin.ReelID, &pin.Confidence,
		&pin.Category, &pin.Subcategory,
		&pin.GooglePlaceID, &pin.GoogleMapsURL, &pin.PlaceTypes}
}

// collectPins reads rows of the shared shape. A list field is never null on
// the wire.
func collectPins(rows pgx.Rows) ([]Pin, error) {
	defer rows.Close()
	pins := []Pin{}
	for rows.Next() {
		var pin Pin
		if err := rows.Scan(pinTargets(&pin)...); err != nil {
			return nil, fmt.Errorf("reading a pin: %w", err)
		}
		pins = append(pins, pin.withLists())
	}
	return pins, rows.Err()
}

func (p Pin) withLists() Pin {
	if p.PlaceTypes == nil {
		p.PlaceTypes = []string{}
	}
	p.SubCategory = p.Subcategory
	return p
}

type Service struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func New(pool *pgxpool.Pool, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{pool: pool, now: now}
}

// Pins answers one viewport. Hidden pins are the user's own choice and are
// excluded here rather than by the client, so a hidden pin never reaches the
// wire at all.
//
// A category keeps only pins whose category is exactly that one, an empty
// category reading as "Other", the same rule the Python map and the place
// search apply. It is applied before the cap, so a filtered map is not the
// first MaxPins of the unfiltered one with most of them thrown away. Empty
// means every category.
//
// Content and manual pins are ordered together, newest first and then by id,
// so which pins survive the cap is the same on every request and a manual pin
// is not dropped just for coming from the second arm. The bool reports that
// more than MaxPins matched and the answer was cut.
func (s *Service) Pins(ctx context.Context, userID string, bounds geo.Bounds, category string) ([]Pin, bool, error) {
	// A box crossing the antimeridian is two boxes. ST_MakeEnvelope with west
	// greater than east silently describes the rest of the world instead.
	envelope := "ST_MakeEnvelope($2, $1, $4, $3, 4326)::geography"
	args := []any{bounds.South, bounds.West, bounds.North, bounds.East}
	if bounds.CrossesAntimeridian() {
		envelope = `ST_Union(
			ST_MakeEnvelope($2, $1, 180, $3, 4326)::geography::geometry,
			ST_MakeEnvelope(-180, $1, $4, $3, 4326)::geography::geometry)::geography`
	}

	query := fmt.Sprintf(`
		SELECT id, kind, name, address, latitude, longitude, reel_id, confidence,
		       category, subcategory, google_place_id, google_maps_url, place_types
		FROM (
		SELECT %[2]s, l.created_at
		FROM reelpin.content_locations l
		JOIN reelpin.contents c ON c.current_version_id = l.content_version_id
		JOIN reelpin.content_versions v ON v.id = l.content_version_id
		JOIN reelpin.user_saves s ON s.content_id = c.id AND s.user_id = $5
		LEFT JOIN reelpin.user_pin_preferences p
		       ON p.location_id = l.id AND p.user_id = $5
		WHERE ST_Intersects(l.point, %[1]s) AND NOT coalesce(p.hidden, false)
		  AND ($7::text = '' OR coalesce(nullif(trim(v.raw_extraction->>'category'), ''), $8::text) = $7)

		UNION ALL

		SELECT %[3]s, m.created_at
		FROM reelpin.user_manual_pins m
		WHERE m.user_id = $5 AND ST_Intersects(m.point, %[1]s)
		  AND ($7::text = '' OR coalesce(nullif(trim(m.category), ''), $8::text) = $7)
		) u(id, kind, name, address, latitude, longitude, reel_id, confidence,
		    category, subcategory, google_place_id, google_maps_url, place_types, created_at)
		ORDER BY created_at DESC, id
		LIMIT $6`, envelope, contentPinColumns, manualPinColumns)

	// One row past the cap tells a full answer from a cut one.
	args = append(args, userID, MaxPins+1, category, uncategorised)
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("reading map pins: %w", err)
	}
	pins, err := collectPins(rows)
	if err != nil {
		return nil, false, err
	}
	if len(pins) > MaxPins {
		return pins[:MaxPins], true, nil
	}
	return pins, false, nil
}

// CountPins is how many pins Pins would return for this user with no viewport:
// visible content locations plus manual pins. The library stats report it, so
// the number on the profile is the number on the map.
func (s *Service) CountPins(ctx context.Context, userID string) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*)
		   FROM reelpin.content_locations l
		   JOIN reelpin.contents c ON c.current_version_id = l.content_version_id
		   JOIN reelpin.user_saves s ON s.content_id = c.id AND s.user_id = $1
		   LEFT JOIN reelpin.user_pin_preferences p
		          ON p.location_id = l.id AND p.user_id = $1
		   WHERE NOT coalesce(p.hidden, false))
		  + (SELECT count(*) FROM reelpin.user_manual_pins m WHERE m.user_id = $1)`,
		userID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("counting map pins: %w", err)
	}
	return count, nil
}

// PinCounted answers library stats from a reader, replacing its pinned
// location count with the map's own.
type PinCounted struct {
	reels.ReelReader
	pins *Service
}

func WithPinCount(reader reels.ReelReader, pins *Service) PinCounted {
	return PinCounted{ReelReader: reader, pins: pins}
}

func (p PinCounted) Stats(ctx context.Context, userID string) (reels.LibraryStats, error) {
	stats, err := p.ReelReader.Stats(ctx, userID)
	if err != nil {
		return reels.LibraryStats{}, err
	}
	stats.TotalPinnedLocations, err = p.pins.CountPins(ctx, userID)
	if err != nil {
		return reels.LibraryStats{}, err
	}
	return stats, nil
}

// Nearby orders this user's pins by distance from a point. The ordering is done
// by PostGIS on the geography type, so it is metres on a sphere rather than
// degrees on a plane.
func (s *Service) Nearby(ctx context.Context, userID string, centre geo.Point, radiusMetres float64, limit int) ([]Pin, error) {
	if limit <= 0 || limit > MaxPins {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+contentPinColumns+`
		FROM reelpin.content_locations l
		JOIN reelpin.contents c ON c.current_version_id = l.content_version_id
		JOIN reelpin.content_versions v ON v.id = l.content_version_id
		JOIN reelpin.user_saves s ON s.content_id = c.id AND s.user_id = $1
		LEFT JOIN reelpin.user_pin_preferences p
		       ON p.location_id = l.id AND p.user_id = $1
		WHERE ST_DWithin(l.point, ST_MakePoint($3, $2)::geography, $4)
		  AND NOT coalesce(p.hidden, false)
		ORDER BY l.point <-> ST_MakePoint($3, $2)::geography
		LIMIT $5`,
		userID, centre.Latitude, centre.Longitude, radiusMetres, limit)
	if err != nil {
		return nil, fmt.Errorf("reading nearby pins: %w", err)
	}
	return collectPins(rows)
}

// CreateManualPin drops a pin of the user's own.
func (s *Service) CreateManualPin(ctx context.Context, userID, name string, address *string, point geo.Point) (Pin, error) {
	if strings.TrimSpace(name) == "" {
		return Pin{}, errors.New("a pin needs a name")
	}
	var pin Pin
	err := s.pool.QueryRow(ctx, `
		INSERT INTO reelpin.user_manual_pins AS m (user_id, name, address, point)
		VALUES ($1, $2, $3, ST_MakePoint($5, $4)::geography)
		RETURNING `+manualPinColumns,
		userID, name, address, point.Latitude, point.Longitude,
	).Scan(pinTargets(&pin)...)
	if err != nil {
		return Pin{}, fmt.Errorf("creating a manual pin: %w", err)
	}
	return pin.withLists(), nil
}

// DeleteManualPin removes one of this user's own pins. Another user's pin is
// not found rather than forbidden.
func (s *Service) DeleteManualPin(ctx context.Context, userID, pinID string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM reelpin.user_manual_pins WHERE id = $1 AND user_id = $2`, pinID, userID)
	if err != nil {
		return fmt.Errorf("deleting a manual pin: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// HidePin records that this user does not want to see a content pin. It is a
// preference, not a deletion: the location stays, and every other user's map is
// untouched.
func (s *Service) HidePin(ctx context.Context, userID, locationID string, hidden bool) error {
	// The location must be reachable through one of this user's saves, or a
	// stranger could enumerate location ids by hiding them.
	var reachable bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM reelpin.content_locations l
			JOIN reelpin.contents c ON c.current_version_id = l.content_version_id
			JOIN reelpin.user_saves s ON s.content_id = c.id
			WHERE l.id = $1 AND s.user_id = $2)`,
		locationID, userID).Scan(&reachable)
	if err != nil {
		return fmt.Errorf("checking the pin: %w", err)
	}
	if !reachable {
		return ErrNotFound
	}

	if _, err := s.pool.Exec(ctx, `
		INSERT INTO reelpin.user_pin_preferences (user_id, location_id, hidden)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, location_id) DO UPDATE SET hidden = EXCLUDED.hidden`,
		userID, locationID, hidden); err != nil {
		return fmt.Errorf("recording the pin preference: %w", err)
	}
	return nil
}

// DeleteItem removes one map item. A manual item is deleted; a location, named
// by its own id or by reel and index, is hidden for this user only, the same
// preference HidePin records. Ids that do not parse, and ids this user cannot
// reach, are both ErrNotFound.
func (s *Service) DeleteItem(ctx context.Context, userID, itemID string) error {
	item, err := ParseItemID(itemID)
	if err != nil {
		return err
	}
	if item.Manual {
		return s.DeleteManualPin(ctx, userID, item.ID)
	}
	if item.Location {
		return s.HidePin(ctx, userID, item.ID, true)
	}

	var locationID string
	err = s.pool.QueryRow(ctx, `
		SELECT l.id::text
		FROM reelpin.user_saves s
		JOIN reelpin.contents c ON c.id = s.content_id
		JOIN reelpin.content_locations l
		       ON l.content_version_id = c.current_version_id AND l.ordinal = $3
		WHERE s.id = $1 AND s.user_id = $2`,
		item.ID, userID, item.Ordinal).Scan(&locationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("finding the map item: %w", err)
	}
	return s.HidePin(ctx, userID, locationID, true)
}

// Item is a parsed map item id: a bare {location_id}, the id the map list
// returns for a content pin; manual:{pin_id}; or reel:{reel_id}:{ordinal}.
type Item struct {
	Manual   bool
	Location bool
	ID       string
	Ordinal  int
}

// ParseItemID accepts only the three documented forms with UUID ids. Anything
// else is ErrNotFound so a malformed id cannot be told from a missing one.
func ParseItemID(raw string) (Item, error) {
	parts := strings.Split(strings.TrimSpace(raw), ":")
	switch {
	case len(parts) == 1:
		if _, err := uuid.Parse(parts[0]); err != nil {
			return Item{}, ErrNotFound
		}
		return Item{Location: true, ID: parts[0]}, nil
	case len(parts) == 2 && parts[0] == "manual":
		if _, err := uuid.Parse(parts[1]); err != nil {
			return Item{}, ErrNotFound
		}
		return Item{Manual: true, ID: parts[1]}, nil
	case len(parts) == 3 && parts[0] == "reel":
		if _, err := uuid.Parse(parts[1]); err != nil {
			return Item{}, ErrNotFound
		}
		ordinal, err := strconv.Atoi(parts[2])
		if err != nil || ordinal < 0 {
			return Item{}, ErrNotFound
		}
		return Item{ID: parts[1], Ordinal: ordinal}, nil
	}
	return Item{}, ErrNotFound
}
