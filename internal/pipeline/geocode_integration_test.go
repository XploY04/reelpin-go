//go:build integration

package pipeline

import (
	"context"
	"errors"
	"testing"

	"github.com/XploY04/reelpin-go/internal/ai"
	"github.com/XploY04/reelpin-go/internal/geo"
	"github.com/XploY04/reelpin-go/internal/googleplaces"
	"github.com/XploY04/reelpin-go/internal/mapview"
	"github.com/XploY04/reelpin-go/internal/postgres"
)

type fakeGeocoder struct {
	queries []string
	point   geo.Point
	err     error
}

func (f *fakeGeocoder) Geocode(_ context.Context, address string) (geo.Point, error) {
	f.queries = append(f.queries, address)
	return f.point, f.err
}

func TestNewExtractedPlacesReachReelAndMap(t *testing.T) {
	h := newBareHarness(t)
	ctx := context.Background()
	geocoder := &fakeGeocoder{point: geo.Point{Latitude: 12.912, Longitude: 77.638}}
	h.pipeline.deps.Geocoder = geocoder
	h.extractor.extraction.Locations = []ai.Location{{Name: "Gali Six", City: "Bengaluru", Country: "India"}}
	jobID := submitAndRun(t, h, userA)
	id := saveID(t, h.pool, jobID)
	record, err := postgres.NewReels(h.pool).Get(ctx, userA, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(geocoder.queries) != 1 || geocoder.queries[0] != "Gali Six, Bengaluru, India" {
		t.Fatalf("geocoding queries = %v", geocoder.queries)
	}
	if len(record.Locations) != 1 || record.Locations[0].Latitude == nil ||
		*record.Locations[0].Latitude != geocoder.point.Latitude {
		t.Fatalf("reel locations = %+v", record.Locations)
	}
	bounds, err := geo.NewBounds(12, 77, 14, 79)
	if err != nil {
		t.Fatal(err)
	}
	pins, _, err := mapview.New(h.pool, nil).Pins(ctx, userA, bounds, "")
	if err != nil || len(pins) != 1 || pins[0].Name != "Gali Six" {
		t.Fatalf("map pins = %+v, error = %v", pins, err)
	}
	if _, err := h.handle(t); err != nil {
		t.Fatal(err)
	}
	pins, _, err = mapview.New(h.pool, nil).Pins(ctx, userA, bounds, "")
	if err != nil || len(pins) != 1 || len(geocoder.queries) != 1 {
		t.Fatalf("redelivery: pins = %+v, calls = %d, error = %v", pins, len(geocoder.queries), err)
	}

	state := &run{Extraction: ai.Extraction{Locations: []ai.Location{{Name: "Gali Six", City: "Bengaluru", Country: "India"}}}}
	if err := h.pipeline.geocode(ctx, state); err != nil {
		t.Fatal(err)
	}
	if len(geocoder.queries) != 1 || state.Extraction.Locations[0].Latitude == nil {
		t.Fatalf("cached geocoding: calls = %d, locations = %+v", len(geocoder.queries), state.Extraction.Locations)
	}
}

func TestGeocodingMissesCacheButFailuresDoNot(t *testing.T) {
	for _, test := range []struct {
		name      string
		err       error
		wantCalls int
	}{
		{name: "not found", err: googleplaces.ErrNotFound, wantCalls: 1},
		{name: "provider failure", err: errors.New("provider unavailable"), wantCalls: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newBareHarness(t)
			geocoder := &fakeGeocoder{err: test.err}
			h.pipeline.deps.Geocoder = geocoder
			for range 2 {
				state := &run{Extraction: ai.Extraction{Locations: []ai.Location{{Name: "An unknown cafe"}}}}
				err := h.pipeline.geocode(context.Background(), state)
				if test.wantCalls == 2 && (err == nil || Classify(err).Class != Transient) {
					t.Fatalf("provider failure was not retryable: %v", err)
				}
				if test.wantCalls == 1 && err != nil {
					t.Fatal(err)
				}
				if state.Extraction.Locations[0].Latitude != nil {
					t.Fatal("a failed lookup invented coordinates")
				}
			}
			if len(geocoder.queries) != test.wantCalls {
				t.Fatalf("provider calls = %d, want %d", len(geocoder.queries), test.wantCalls)
			}
		})
	}
}

func TestGeocodingPreservesExistingCoordinatesAndUnlocatedPlaces(t *testing.T) {
	h := newBareHarness(t)
	latitude, longitude := 12.912, 77.638
	geocoder := &fakeGeocoder{err: googleplaces.ErrNotFound}
	h.pipeline.deps.Geocoder = geocoder
	state := &run{Extraction: ai.Extraction{Locations: []ai.Location{
		{Name: "Located cafe", Latitude: &latitude, Longitude: &longitude},
		{Name: "Unlocated cafe"},
	}}}
	if err := h.pipeline.geocode(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if len(geocoder.queries) != 1 || geocoder.queries[0] != "Unlocated cafe" ||
		len(state.Extraction.Locations) != 2 || *state.Extraction.Locations[0].Longitude != longitude {
		t.Fatalf("queries = %v, locations = %+v", geocoder.queries, state.Extraction.Locations)
	}
}

func TestGeocodingRetryReusesExtractionAndEventuallyStoresOnePin(t *testing.T) {
	h := newHarness(t, userA)
	ctx := context.Background()
	geocoder := &fakeGeocoder{
		point: geo.Point{Latitude: 12.912, Longitude: 77.638},
		err:   errors.New("provider unavailable"),
	}
	h.pipeline.deps.Geocoder = geocoder
	h.extractor.extraction.Locations = []ai.Location{{Name: "Gali Six", City: "Bengaluru", Country: "India"}}
	if _, err := h.handle(t); err == nil || Classify(err).Class != Transient {
		t.Fatalf("first lookup failure = %v, want a retryable failure", err)
	}
	if status := h.runStatus(t); status != "retry_scheduled" {
		t.Fatalf("run status = %q, want retry_scheduled", status)
	}
	var versions, locations, lookups int
	if err := h.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM reelpin.content_versions),
		       (SELECT count(*) FROM reelpin.content_locations),
		       (SELECT count(*) FROM reelpin.place_lookups)`,
	).Scan(&versions, &locations, &lookups); err != nil {
		t.Fatal(err)
	}
	if versions != 0 || locations != 0 || lookups != 0 {
		t.Fatalf("failed lookup stored versions=%d locations=%d lookups=%d", versions, locations, lookups)
	}
	if h.extractor.calls.Load() != 1 || h.categorizer.calls.Load() != 0 {
		t.Fatalf("first pass: extraction calls=%d categorization calls=%d", h.extractor.calls.Load(), h.categorizer.calls.Load())
	}

	geocoder.err = nil
	if _, err := h.handle(t); err != nil {
		t.Fatalf("resume after provider recovery: %v", err)
	}
	if status := h.runStatus(t); status != "completed" {
		t.Fatalf("run status after recovery = %q", status)
	}
	if h.extractor.calls.Load() != 1 || h.categorizer.calls.Load() != 1 || len(geocoder.queries) != 2 {
		t.Fatalf("recovery: extraction=%d categorization=%d place lookups=%d, want 1/1/2",
			h.extractor.calls.Load(), h.categorizer.calls.Load(), len(geocoder.queries))
	}
	var jobID string
	if err := h.pool.QueryRow(ctx, `SELECT id::text FROM reelpin.processing_jobs WHERE run_id = $1`, h.runID).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	id := saveID(t, h.pool, jobID)
	record, err := postgres.NewReels(h.pool).Get(ctx, userA, id)
	if err != nil || len(record.Locations) != 1 || record.Locations[0].Latitude == nil ||
		*record.Locations[0].Latitude != geocoder.point.Latitude {
		t.Fatalf("recovered reel locations = %+v, error = %v", record.Locations, err)
	}
	if _, err := h.handle(t); err != nil {
		t.Fatalf("completed redelivery: %v", err)
	}
	bounds, err := geo.NewBounds(12, 77, 14, 79)
	if err != nil {
		t.Fatal(err)
	}
	pins, _, err := mapview.New(h.pool, nil).Pins(ctx, userA, bounds, "")
	if err != nil || len(pins) != 1 || pins[0].Name != "Gali Six" {
		t.Fatalf("recovered map pins = %+v, error = %v", pins, err)
	}
	if h.extractor.calls.Load() != 1 || len(geocoder.queries) != 2 {
		t.Fatalf("redelivery repeated paid work: extraction=%d place lookups=%d", h.extractor.calls.Load(), len(geocoder.queries))
	}
}
