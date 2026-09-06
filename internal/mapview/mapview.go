// Package mapview answers "what is on my map". Every query starts from the
// authenticated user's saves: a location is only visible because the user
// saved the content it came from, so ownership is a join, not a filter added
// afterwards.
package mapview

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/geo"
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
func (s *Service) Pins(ctx context.Context, userID string, bounds geo.Bounds) ([]Pin, error) {
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
		SELECT l.id::text, 'content', l.name, l.address,
		       ST_Y(l.point::geometry), ST_X(l.point::geometry),
		       s.id::text, l.confidence
		FROM reelpin.content_locations l
		JOIN reelpin.contents c ON c.current_version_id = l.content_version_id
		JOIN reelpin.user_saves s ON s.content_id = c.id AND s.user_id = $5
		LEFT JOIN reelpin.user_pin_preferences p
		       ON p.location_id = l.id AND p.user_id = $5
		WHERE ST_Intersects(l.point, %[1]s) AND NOT coalesce(p.hidden, false)

		UNION ALL

		SELECT m.id::text, 'manual', m.name, m.address,
		       ST_Y(m.point::geometry), ST_X(m.point::geometry),
		       NULL, NULL
		FROM reelpin.user_manual_pins m
		WHERE m.user_id = $5 AND ST_Intersects(m.point, %[1]s)

		LIMIT $6`, envelope)

	args = append(args, userID, MaxPins)
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("reading map pins: %w", err)
	}
	defer rows.Close()

	pins := []Pin{}
	for rows.Next() {
		var pin Pin
		if err := rows.Scan(&pin.ID, &pin.Kind, &pin.Name, &pin.Address,
			&pin.Latitude, &pin.Longitude, &pin.ReelID, &pin.Confidence); err != nil {
			return nil, fmt.Errorf("reading a pin: %w", err)
		}
		pins = append(pins, pin)
	}
	return pins, rows.Err()
}

// Nearby orders this user's pins by distance from a point. The ordering is done
// by PostGIS on the geography type, so it is metres on a sphere rather than
// degrees on a plane.
func (s *Service) Nearby(ctx context.Context, userID string, centre geo.Point, radiusMetres float64, limit int) ([]Pin, error) {
	if limit <= 0 || limit > MaxPins {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT l.id::text, 'content', l.name, l.address,
		       ST_Y(l.point::geometry), ST_X(l.point::geometry),
		       s.id::text, l.confidence
		FROM reelpin.content_locations l
		JOIN reelpin.contents c ON c.current_version_id = l.content_version_id
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
	defer rows.Close()

	pins := []Pin{}
	for rows.Next() {
		var pin Pin
		if err := rows.Scan(&pin.ID, &pin.Kind, &pin.Name, &pin.Address,
			&pin.Latitude, &pin.Longitude, &pin.ReelID, &pin.Confidence); err != nil {
			return nil, fmt.Errorf("reading a pin: %w", err)
		}
		pins = append(pins, pin)
	}
	return pins, rows.Err()
}

// CreateManualPin drops a pin of the user's own.
func (s *Service) CreateManualPin(ctx context.Context, userID, name string, address *string, point geo.Point) (Pin, error) {
	if strings.TrimSpace(name) == "" {
		return Pin{}, errors.New("a pin needs a name")
	}
	var pin Pin
	err := s.pool.QueryRow(ctx, `
		INSERT INTO reelpin.user_manual_pins (user_id, name, address, point)
		VALUES ($1, $2, $3, ST_MakePoint($5, $4)::geography)
		RETURNING id::text, 'manual', name, address,
		          ST_Y(point::geometry), ST_X(point::geometry)`,
		userID, name, address, point.Latitude, point.Longitude,
	).Scan(&pin.ID, &pin.Kind, &pin.Name, &pin.Address, &pin.Latitude, &pin.Longitude)
	if err != nil {
		return Pin{}, fmt.Errorf("creating a manual pin: %w", err)
	}
	return pin, nil
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

var _ = pgx.ErrNoRows
