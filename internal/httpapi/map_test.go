package httpapi

import (
	"net/http"
	"testing"

	"github.com/XploY04/reelpin-go/internal/mapview"
)

func mapDeps(fake *fakeMap) Deps {
	deps := testDeps(&fakePinger{})
	deps.Map = fake
	return deps
}

func TestMapRoutesReachTheServiceAsTheTokenSubject(t *testing.T) {
	tests := []struct {
		name   string
		method string
		target string
		body   string
		action string
	}{
		{name: "map", method: "GET", target: "/api/v1/map", action: "map"},
		{name: "bare map", method: "GET", target: "/map", action: "map"},
		{name: "search", method: "GET", target: "/api/v1/map/search?query=cafe", action: "search"},
		{name: "pin", method: "POST", target: "/api/v1/map/pins", body: `{"google_place_id":"place-1"}`, action: "pin"},
		{name: "remove", method: "DELETE", target: "/api/v1/map/items/manual:pin-1", action: "remove"},
		{name: "discover", method: "GET", target: "/api/v1/discover", action: "discover"},
		{name: "bare discover", method: "GET", target: "/discover", action: "discover"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeMap{}
			rec := request(mapDeps(fake), tt.method, tt.target, tt.body, "Bearer good.token")

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
			}
			if fake.lastAction != tt.action {
				t.Errorf("action = %q, want %q", fake.lastAction, tt.action)
			}
			if fake.lastUserID != testUserID {
				t.Errorf("acted as %q, want the token subject", fake.lastUserID)
			}
		})
	}
}

func TestMapRoutesRequireASession(t *testing.T) {
	for _, target := range []string{"/api/v1/map", "/api/v1/discover", "/api/v1/map/search?query=cafe"} {
		rec := request(mapDeps(&fakeMap{}), "GET", target, "", "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s status = %d, want 401", target, rec.Code)
		}
	}
}

func TestAMissingMapItemIs404(t *testing.T) {
	fake := &fakeMap{err: mapview.ErrNotFound}

	rec := request(mapDeps(fake), "DELETE", "/api/v1/map/items/manual:nope", "", "Bearer good.token")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if code := decodeError(t, rec).ErrorCode; code != "map_item_not_found" {
		t.Errorf("error_code = %q", code)
	}
}

func TestPinningRequiresAPlaceID(t *testing.T) {
	rec := request(mapDeps(&fakeMap{}), "POST", "/api/v1/map/pins", `{}`, "Bearer good.token")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%s)", rec.Code, rec.Body.String())
	}
}

func TestMapRejectsAnUnknownPlatformFilter(t *testing.T) {
	rec := request(mapDeps(&fakeMap{}), "GET", "/api/v1/map?platform=myspace", "", "Bearer good.token")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if code := decodeError(t, rec).ErrorCode; code != "invalid_platform" {
		t.Errorf("error_code = %q", code)
	}
}

func TestDiscoverRejectsAMalformedDate(t *testing.T) {
	rec := request(mapDeps(&fakeMap{}), "GET", "/api/v1/discover?selected_date=05-09-2026", "", "Bearer good.token")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%s)", rec.Code, rec.Body.String())
	}
}

// Place search costs a provider call, so it is metered and fails closed.
func TestPlaceSearchIsRateLimited(t *testing.T) {
	deps := mapDeps(&fakeMap{})
	deps.Limiter = &fakeLimiter{allow: false}

	rec := request(deps, "GET", "/api/v1/map/search?query=cafe", "", "Bearer good.token")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
}
