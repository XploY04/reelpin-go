package mapview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/postgres"
	"github.com/XploY04/reelpin-go/internal/reels"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is a map item that is not this user's to change.
var ErrNotFound = errors.New("map item not found")

// libraryLimit matches the Python display query.
const libraryLimit = 5000

type Service struct {
	pool   *pgxpool.Pool
	places Places
	now    func() time.Time
}

func NewService(pool *pgxpool.Pool, places Places, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{pool: pool, places: places, now: now}
}

// Map is the whole map for one user.
func (s *Service) Map(ctx context.Context, userID, category string, platforms []string) (MapResponse, error) {
	records, err := s.library(ctx, userID)
	if err != nil {
		return MapResponse{}, err
	}
	manualPins, err := s.manualPins(ctx, userID)
	if err != nil {
		return MapResponse{}, err
	}
	hidden, fingerprints, err := s.hiddenPins(ctx, userID)
	if err != nil {
		return MapResponse{}, err
	}

	return BuildFromSources(records, manualPins, hidden, fingerprints, category, platforms, nil), nil
}

func (s *Service) library(ctx context.Context, userID string) ([]reels.ReelRecord, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT "+postgres.ReelListColumns+" FROM public.reels r WHERE r.user_id = $1 ORDER BY r.created_at DESC LIMIT $2",
		userID, libraryLimit)
	if err != nil {
		return nil, fmt.Errorf("reading the library: %w", err)
	}
	defer rows.Close()

	records := []reels.ReelRecord{}
	for rows.Next() {
		record, err := postgres.ScanReelList(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (s *Service) manualPins(ctx context.Context, userID string) ([]ManualPin, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, google_place_id, name, address, latitude, longitude, google_maps_url,
		       category, subcategory, secondary_categories, place_types
		FROM public.manual_map_pins
		WHERE user_id = $1
		ORDER BY created_at DESC
		LIMIT $2`, userID, libraryLimit)
	if err != nil {
		return nil, fmt.Errorf("reading manual pins: %w", err)
	}
	defer rows.Close()

	pins := []ManualPin{}
	for rows.Next() {
		var (
			pin                   ManualPin
			category, subcategory *string
			secondary, types      []byte
		)
		if err := rows.Scan(&pin.ID, &pin.GooglePlaceID, &pin.Name, &pin.Address,
			&pin.Latitude, &pin.Longitude, &pin.GoogleMapsURL,
			&category, &subcategory, &secondary, &types); err != nil {
			return nil, fmt.Errorf("reading manual pins: %w", err)
		}
		if category != nil {
			pin.Category = *category
		}
		if subcategory != nil {
			pin.Subcategory = *subcategory
		}
		_ = json.Unmarshal(secondary, &pin.SecondaryCategories)
		_ = json.Unmarshal(types, &pin.PlaceTypes)
		pins = append(pins, pin)
	}
	return pins, rows.Err()
}

func (s *Service) hiddenPins(ctx context.Context, userID string) (map[HiddenKey]bool, map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT reel_id::text, location_index, location_fingerprint
		FROM public.hidden_reel_map_pins WHERE user_id = $1`, userID)
	if err != nil {
		return nil, nil, fmt.Errorf("reading hidden pins: %w", err)
	}
	defer rows.Close()

	keys := map[HiddenKey]bool{}
	fingerprints := map[string]bool{}
	for rows.Next() {
		var reelID string
		var index int
		var fingerprint *string
		if err := rows.Scan(&reelID, &index, &fingerprint); err != nil {
			return nil, nil, fmt.Errorf("reading hidden pins: %w", err)
		}
		keys[HiddenKey{ReelID: reelID, Index: index}] = true
		// The fingerprint is what survives a reel being reprocessed and its
		// places coming back in a different order.
		if fingerprint != nil && *fingerprint != "" {
			fingerprints[*fingerprint] = true
		}
	}
	return keys, fingerprints, rows.Err()
}

// HideOrRemove deletes a manual pin, or hides one place on a reel. Both are
// idempotent: doing it twice is not an error.
func (s *Service) HideOrRemove(ctx context.Context, userID, mapItemID string) error {
	kind, id, index, err := ParseItemID(mapItemID)
	if err != nil {
		return ErrNotFound
	}

	if kind == "manual" {
		tag, err := s.pool.Exec(ctx,
			`DELETE FROM public.manual_map_pins WHERE id = $1 AND user_id = $2`, id, userID)
		if err != nil {
			return fmt.Errorf("removing a manual pin: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	}

	// Hiding a reel place needs the reel, both to prove ownership and to
	// compute the fingerprint that outlives reprocessing.
	record, err := s.reel(ctx, userID, id)
	if err != nil {
		return err
	}
	locations := reels.BuildMappableLocations(record)
	if index < 0 || index >= len(locations) {
		return ErrNotFound
	}
	location := locations[index]

	if _, err := s.pool.Exec(ctx, `
		INSERT INTO public.hidden_reel_map_pins (user_id, reel_id, location_index, location_fingerprint)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT DO NOTHING`,
		userID, record.ID, index,
		Fingerprint(location.LocationName, location.LocationDisplayLabel, location.Latitude, location.Longitude),
	); err != nil {
		return fmt.Errorf("hiding a reel place: %w", err)
	}
	return nil
}

func (s *Service) reel(ctx context.Context, userID, reelID string) (reels.ReelRecord, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT "+postgres.ReelListColumns+" FROM public.reels r WHERE r.id = $1 AND r.user_id = $2",
		reelID, userID)
	if err != nil {
		return reels.ReelRecord{}, fmt.Errorf("reading the reel: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return reels.ReelRecord{}, ErrNotFound
	}
	return postgres.ScanReelList(rows)
}

// CreatePin saves a place the user chose from search. It is idempotent by
// Google place id, so tapping twice does not produce two pins.
func (s *Service) CreatePin(ctx context.Context, userID, googlePlaceID, sessionToken string) (MapItem, error) {
	if strings.TrimSpace(googlePlaceID) == "" {
		return MapItem{}, fmt.Errorf("a place id is required")
	}

	var existing ManualPin
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, google_place_id, name, address, latitude, longitude, google_maps_url
		FROM public.manual_map_pins
		WHERE user_id = $1 AND google_place_id = $2`, userID, googlePlaceID,
	).Scan(&existing.ID, &existing.GooglePlaceID, &existing.Name, &existing.Address,
		&existing.Latitude, &existing.Longitude, &existing.GoogleMapsURL)
	if err == nil {
		return manualPinItem(existing), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return MapItem{}, fmt.Errorf("reading an existing pin: %w", err)
	}

	details, err := s.places.Details(ctx, googlePlaceID, sessionToken)
	if err != nil {
		return MapItem{}, err
	}

	types, err := json.Marshal(details.Types)
	if err != nil {
		types = []byte("[]")
	}

	var pin ManualPin
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO public.manual_map_pins
			(user_id, google_place_id, name, address, latitude, longitude, google_maps_url, place_types)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		-- The index is partial, so the conflict target must repeat its predicate.
		ON CONFLICT (user_id, google_place_id) WHERE google_place_id IS NOT NULL
		DO UPDATE SET updated_at = now()
		RETURNING id::text, google_place_id, name, address, latitude, longitude, google_maps_url`,
		userID, googlePlaceID, details.Name, nullable(details.Address),
		details.Latitude, details.Longitude, nullable(details.GoogleMapsURL), types,
	).Scan(&pin.ID, &pin.GooglePlaceID, &pin.Name, &pin.Address,
		&pin.Latitude, &pin.Longitude, &pin.GoogleMapsURL); err != nil {
		return MapItem{}, fmt.Errorf("saving the pin: %w", err)
	}
	pin.PlaceTypes = details.Types
	return manualPinItem(pin), nil
}

func nullable(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}
