package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/mapview"
)

func mapDeps(fake *fakeMap) Deps {
	deps := testDeps(&fakePinger{})
	deps.Map = fake
	return deps
}

func TestTheViewportReachesTheServiceUnchanged(t *testing.T) {
	fake := &fakeMap{}
	rec := serve(mapDeps(fake), "GET",
		"/api/v2/map/pins?south=15&west=73&north=16&east=74", "Bearer good.token")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if fake.lastUserID != testUserID {
		t.Errorf("asked for %q, want the token subject", fake.lastUserID)
	}
	bounds := fake.lastBounds
	if bounds.South != 15 || bounds.West != 73 || bounds.North != 16 || bounds.East != 74 {
		t.Fatalf("bounds = %+v", bounds)
	}
}

func TestAPacificViewportIsNotRejected(t *testing.T) {
	// West greater than east is a box crossing the antimeridian, which is a
	// place a user can pan to, not a mistake.
	fake := &fakeMap{}
	rec := serve(mapDeps(fake), "GET",
		"/api/v2/map/pins?south=-25&west=170&north=-5&east=-170", "Bearer good.token")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if !fake.lastBounds.CrossesAntimeridian() {
		t.Error("the service was not told the box crosses the antimeridian")
	}
}

func TestBadCoordinatesAreNamedNotGuessed(t *testing.T) {
	tests := []struct{ name, query string }{
		{"missing south", "?west=73&north=16&east=74"},
		{"south not a number", "?south=here&west=73&north=16&east=74"},
		{"latitude out of range", "?south=-91&west=73&north=16&east=74"},
		{"south above north", "?south=17&west=73&north=16&east=74"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serve(mapDeps(&fakeMap{}), "GET", "/api/v2/map/pins"+tt.query, "Bearer good.token")
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (%s)", rec.Code, rec.Body.String())
			}
			body := decodeError(t, rec)
			if body.Error.Details["field"] == nil {
				t.Error("the error does not name the offending field")
			}
		})
	}
}

func TestAManualPinIsCreatedAndDeleted(t *testing.T) {
	fake := &fakeMap{}
	req := httptest.NewRequest("POST", "/api/v2/map/manual-pins",
		strings.NewReader(`{"name":"My spot","latitude":15.58,"longitude":73.74}`))
	req.Header.Set("Authorization", "Bearer good.token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	routes(mapDeps(fake)).ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	var pin mapview.Pin
	if err := json.Unmarshal(rec.Body.Bytes(), &pin); err != nil {
		t.Fatal(err)
	}
	if pin.Kind != "manual" || pin.ReelID != nil {
		t.Fatalf("pin = %+v", pin)
	}

	del := serve(mapDeps(fake), "DELETE", "/api/v2/map/manual-pins/"+testReelID, "Bearer good.token")
	if del.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", del.Code)
	}
}

func TestAnUnreachablePinIsNotFound(t *testing.T) {
	fake := &fakeMap{err: mapview.ErrNotFound}
	req := httptest.NewRequest("POST", "/api/v2/map/locations/"+testReelID+"/hidden",
		strings.NewReader(`{"hidden":true}`))
	req.Header.Set("Authorization", "Bearer good.token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	routes(mapDeps(fake)).ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: hiding must not confirm an id exists", rec.Code)
	}
	if code := decodeError(t, rec).Error.Code; code != "pin_not_found" {
		t.Errorf("code = %q", code)
	}
}

func TestMapRoutesRequireASession(t *testing.T) {
	for _, target := range []string{
		"/api/v2/map/pins?south=15&west=73&north=16&east=74",
		"/api/v2/map/nearby?latitude=15&longitude=73",
	} {
		rec := serve(mapDeps(&fakeMap{}), "GET", target, "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s status = %d, want 401", target, rec.Code)
		}
	}
}

func deleteItem(deps Deps, id, authorization string) *httptest.ResponseRecorder {
	return serve(deps, "DELETE", "/api/v2/map/items/"+id, authorization)
}

func TestDeletingAMapItemReachesTheServiceForTheTokenUser(t *testing.T) {
	for _, id := range []string{
		"33333333-3333-4333-8333-333333333333",
		"reel:22222222-2222-4222-8222-222222222222:0",
		"manual:55555555-5555-4555-8555-555555555555",
	} {
		t.Run(id, func(t *testing.T) {
			fake := &fakeMap{}
			rec := deleteItem(mapDeps(fake), id, "Bearer good.token")

			if rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204 (%s)", rec.Code, rec.Body.String())
			}
			if fake.lastUserID != testUserID || fake.lastItemID != id {
				t.Errorf("service saw user %q item %q", fake.lastUserID, fake.lastItemID)
			}
		})
	}
}

func TestAnUnknownOrForeignMapItemIs404(t *testing.T) {
	rec := deleteItem(mapDeps(&fakeMap{err: mapview.ErrNotFound}),
		"reel:22222222-2222-4222-8222-222222222222:9", "Bearer good.token")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if code := decodeError(t, rec).Error.Code; code != "map_item_not_found" {
		t.Errorf("code = %q", code)
	}
}

func TestDeletingAMapItemNeedsASession(t *testing.T) {
	rec := deleteItem(mapDeps(&fakeMap{}), "manual:55555555-5555-4555-8555-555555555555", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestADriverErrorDeletingAMapItemIsNotLeaked(t *testing.T) {
	rec := deleteItem(mapDeps(&fakeMap{err: errFake}),
		"manual:55555555-5555-4555-8555-555555555555", "Bearer good.token")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), errFake.Error()) {
		t.Error("the driver error reached the body")
	}
}
