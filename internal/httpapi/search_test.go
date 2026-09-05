package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/search"
)

func searchDeps(fake *fakeSearch) Deps {
	deps := testDeps(&fakePinger{})
	deps.Search = fake
	return deps
}

func TestSearchRunsAsTheTokenSubject(t *testing.T) {
	for _, target := range []string{"/api/v1/search", "/search"} {
		fake := &fakeSearch{}
		rec := request(searchDeps(fake), "POST", target, `{"query":"artjuna cafe","limit":5}`, "Bearer good.token")

		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200 (%s)", target, rec.Code, rec.Body.String())
		}
		if fake.lastUserID != testUserID {
			t.Errorf("searched as %q, want the token subject", fake.lastUserID)
		}
		if fake.lastQuery != "artjuna cafe" || fake.lastLimit != 5 {
			t.Errorf("query = %q limit = %d", fake.lastQuery, fake.lastLimit)
		}
	}
}

func TestSearchIgnoresAUserIDInTheBody(t *testing.T) {
	fake := &fakeSearch{}
	rec := request(searchDeps(fake), "POST", "/api/v1/search",
		`{"query":"cafe","user_id":"`+otherUserID+`"}`, "Bearer good.token")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if fake.lastUserID != testUserID {
		t.Fatalf("searched as %q: a body field must never choose the user", fake.lastUserID)
	}
}

func TestSearchPassesFiltersThrough(t *testing.T) {
	fake := &fakeSearch{}
	rec := request(searchDeps(fake), "POST", "/api/v1/search",
		`{"query":"cafe","platform":"instagram","category":"food","subcategory":"cafes","saved_date":"week"}`,
		"Bearer good.token")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	filters := fake.lastFilters
	if len(filters.Platforms) != 1 || filters.Platforms[0] != "instagram" {
		t.Errorf("platforms = %v", filters.Platforms)
	}
	if filters.Category != "food" || filters.Subcategory != "cafes" || filters.SavedDate != "week" {
		t.Errorf("filters = %+v", filters)
	}
}

func TestSearchRequiresAQuery(t *testing.T) {
	for _, body := range []string{`{}`, `{"query":"   "}`} {
		rec := request(searchDeps(&fakeSearch{}), "POST", "/api/v1/search", body, "Bearer good.token")
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("body %s status = %d, want 422", body, rec.Code)
		}
	}
}

func TestSearchRejectsAnOutOfRangeLimit(t *testing.T) {
	rec := request(searchDeps(&fakeSearch{}), "POST", "/api/v1/search", `{"query":"cafe","limit":50}`, "Bearer good.token")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
}

func TestSearchRejectsAnUnknownPlatform(t *testing.T) {
	rec := request(searchDeps(&fakeSearch{}), "POST", "/api/v1/search", `{"query":"cafe","platform":"myspace"}`, "Bearer good.token")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if code := decodeError(t, rec).ErrorCode; code != "invalid_platform" {
		t.Errorf("error_code = %q", code)
	}
}

func TestSearchRequiresASession(t *testing.T) {
	rec := request(searchDeps(&fakeSearch{}), "POST", "/api/v1/search", `{"query":"cafe"}`, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestSearchFailureIsA500(t *testing.T) {
	rec := request(searchDeps(&fakeSearch{err: errFake}), "POST", "/api/v1/search", `{"query":"cafe"}`, "Bearer good.token")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if code := decodeError(t, rec).ErrorCode; code != "search_failed" {
		t.Errorf("error_code = %q", code)
	}
}

func TestSearchIsMetered(t *testing.T) {
	deps := searchDeps(&fakeSearch{})
	deps.Limiter = &fakeLimiter{allow: false}

	rec := request(deps, "POST", "/api/v1/search", `{"query":"cafe"}`, "Bearer good.token")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: a query costs a provider call", rec.Code)
	}
}

func TestSearchWithNoResultsSerializesAnEmptyList(t *testing.T) {
	fake := &fakeSearch{response: search.Response{Query: "nothing", SearchMode: "empty"}}

	rec := request(searchDeps(fake), "POST", "/api/v1/search", `{"query":"nothing"}`, "Bearer good.token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"results":[]`) {
		t.Errorf("body = %s, want an empty list rather than null", body)
	}
}
