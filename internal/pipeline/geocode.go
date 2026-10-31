package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/geo"
	"github.com/XploY04/reelpin-go/internal/googleplaces"
	"github.com/jackc/pgx/v5"
)

type geocodeResult struct {
	Point *geo.Point `json:"point"`
}

func (p *Pipeline) geocode(ctx context.Context, state *run) error {
	if p.deps.Geocoder == nil {
		return nil
	}
	for index := range state.Extraction.Locations {
		location := &state.Extraction.Locations[index]
		if location.Latitude != nil && location.Longitude != nil {
			continue
		}
		query := strings.TrimSpace(location.Name)
		if address := location.Address(); address != "" {
			query += ", " + address
		}
		key := InputHash("geocode-v1", strings.ToLower(query))
		var encoded []byte
		err := p.deps.Pool.QueryRow(ctx, `
			SELECT response FROM reelpin.place_lookups
			WHERE query_hash = $1 AND expires_at > $2`, key, p.deps.Now()).Scan(&encoded)
		var result geocodeResult
		switch {
		case err == nil:
			if err := json.Unmarshal(encoded, &result); err != nil {
				return fmt.Errorf("reading cached geocoding: %w", err)
			}
		case errors.Is(err, pgx.ErrNoRows):
			point, lookupErr := p.deps.Geocoder.Geocode(ctx, query)
			if err := ctx.Err(); err != nil {
				return err
			}
			if lookupErr != nil && !errors.Is(lookupErr, googleplaces.ErrNotFound) {
				return fail(Transient, "provider_unavailable", "A place lookup could not be completed.", lookupErr)
			}
			retention := 24 * time.Hour
			if lookupErr == nil {
				if _, err := geo.NewPoint(point.Latitude, point.Longitude); err != nil {
					return fmt.Errorf("validating geocoding: %w", err)
				}
				result.Point = &point
				retention = 30 * 24 * time.Hour
			}
			encoded, err = json.Marshal(result)
			if err != nil {
				return fmt.Errorf("encoding geocoding: %w", err)
			}
			if _, err := p.deps.Pool.Exec(ctx, `
				INSERT INTO reelpin.place_lookups (query_hash, response, expires_at)
				VALUES ($1, $2::jsonb, $3)
				ON CONFLICT (query_hash) DO UPDATE
				SET response = EXCLUDED.response, expires_at = EXCLUDED.expires_at, created_at = now()`,
				key, string(encoded), p.deps.Now().Add(retention)); err != nil {
				return fmt.Errorf("caching geocoding: %w", err)
			}
		default:
			return fmt.Errorf("loading cached geocoding: %w", err)
		}
		if result.Point != nil {
			if _, err := geo.NewPoint(result.Point.Latitude, result.Point.Longitude); err != nil {
				return fmt.Errorf("validating cached geocoding: %w", err)
			}
			location.Latitude = &result.Point.Latitude
			location.Longitude = &result.Point.Longitude
		}
	}
	return nil
}
