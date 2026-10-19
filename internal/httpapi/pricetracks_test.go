package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/pricetracks"
)

// fakePriceTracks records what it was asked and answers with whatever the
// test set up.
type fakePriceTracks struct {
	created    pricetracks.PriceTrack
	err        error
	lastUserID string
	lastParams pricetracks.CreateParams
	lastID     string
}

func (f *fakePriceTracks) List(_ context.Context, userID string) ([]pricetracks.PriceTrack, error) {
	f.lastUserID = userID
	if f.err != nil {
		return nil, f.err
	}
	return []pricetracks.PriceTrack{}, nil
}

func (f *fakePriceTracks) Create(_ context.Context, userID string, params pricetracks.CreateParams) (pricetracks.PriceTrack, error) {
	f.lastUserID, f.lastParams = userID, params
	return f.created, f.err
}

func (f *fakePriceTracks) Delete(_ context.Context, userID, trackID string) error {
	f.lastUserID, f.lastID = userID, trackID
	return f.err
}

func priceTrackDeps(fake *fakePriceTracks) Deps {
	deps := testDeps(&fakePinger{})
	deps.PriceTracks = fake
	return deps
}

func TestAnEmptyPriceTrackListIsAListNotNull(t *testing.T) {
	fake := &fakePriceTracks{}
	rec := serve(priceTrackDeps(fake), "GET", "/api/v2/price-tracks", "Bearer good.token")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"price_tracks":[]`) {
		t.Errorf("body = %s, want an empty list", rec.Body.String())
	}
	if fake.lastUserID != testUserID {
		t.Errorf("listed for %q, want the token subject", fake.lastUserID)
	}
}

func TestCreatingAPriceTrackPassesTheRequestThrough(t *testing.T) {
	fake := &fakePriceTracks{created: pricetracks.PriceTrack{
		ID: testJobID, URL: "https://shop.example/p/1", TargetPrice: 999, Active: true,
	}}
	rec := send(priceTrackDeps(fake), "POST", "/api/v2/price-tracks",
		`{"url":"https://shop.example/p/1","target_price":999,"title":"Shoes"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 as the Python API answered (%s)", rec.Code, rec.Body.String())
	}
	if fake.lastUserID != testUserID {
		t.Errorf("created for %q, want the token subject", fake.lastUserID)
	}
	if fake.lastParams.URL != "https://shop.example/p/1" || fake.lastParams.TargetPrice == nil ||
		*fake.lastParams.TargetPrice != 999 || fake.lastParams.Title != "Shoes" || fake.lastParams.ReelID != nil {
		t.Errorf("params = %+v", fake.lastParams)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"last_price", "last_checked_at", "notified_at", "reel_id"} {
		if value, present := body[field]; !present || value != nil {
			t.Errorf("%s = %v, want present and null", field, value)
		}
	}
}

func TestAPriceTrackRequestThatCannotBeStoredIsNamed(t *testing.T) {
	tests := []struct {
		name  string
		err   error
		field string
	}{
		{"service refuses a field", &pricetracks.ValidationError{Field: "target_price", Reason: "bad"}, "target_price"},
		{"someone else's save", pricetracks.ErrUnknownSave, "reel_id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := send(priceTrackDeps(&fakePriceTracks{err: tt.err}), "POST", "/api/v2/price-tracks",
				`{"url":"https://shop.example/p/1","target_price":1}`)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (%s)", rec.Code, rec.Body.String())
			}
			if field := decodeError(t, rec).Error.Details["field"]; field != tt.field {
				t.Errorf("field = %v, want %s", field, tt.field)
			}
		})
	}
}

func TestDeletingAPriceTrack(t *testing.T) {
	fake := &fakePriceTracks{}
	rec := serve(priceTrackDeps(fake), "DELETE", "/api/v2/price-tracks/"+testReelID, "Bearer good.token")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if fake.lastID != testReelID || fake.lastUserID != testUserID {
		t.Errorf("deleted %q for %q", fake.lastID, fake.lastUserID)
	}

	missing := serve(priceTrackDeps(&fakePriceTracks{err: pricetracks.ErrNotFound}), "DELETE",
		"/api/v2/price-tracks/"+testReelID, "Bearer good.token")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d, want 404", missing.Code)
	}
	if code := decodeError(t, missing).Error.Code; code != "price_track_not_found" {
		t.Errorf("code = %q", code)
	}

	malformed := serve(priceTrackDeps(&fakePriceTracks{}), "DELETE", "/api/v2/price-tracks/42", "Bearer good.token")
	if malformed.Code != http.StatusNotFound {
		t.Fatalf("malformed status = %d, want the same 404", malformed.Code)
	}
}
