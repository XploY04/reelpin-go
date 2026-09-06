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
	deps.Limiter = allowAllLimiter{}
	return deps
}

func TestSearchRunsAsTheTokenSubject(t *testing.T) {
	fake := &fakeSearch{}
	rec := request(searchDeps(fake), "POST", "/api/v2/search", `{"query":"artjuna cafe","limit":5}`, "Bearer good.token")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if fake.lastUserID != testUserID {
		t.Errorf("searched as %q, want the token subject", fake.lastUserID)
	}
	if fake.lastQuery != "artjuna cafe" || fake.lastLimit != 5 {
		t.Errorf("query = %q limit = %d", fake.lastQuery, fake.lastLimit)
	}
}

// A user_id is not a field of this request. Accepting one would let a body
// choose the account, so the strict decoder rejects it outright.
func TestSearchRejectsAUserIDInTheBody(t *testing.T) {
	fake := &fakeSearch{}
	rec := request(searchDeps(fake), "POST", "/api/v2/search",
		`{"query":"cafe","user_id":"`+otherUserID+`"}`, "Bearer good.token")

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%s)", rec.Code, rec.Body.String())
	}
	if fake.lastUserID != "" {
		t.Fatalf("searched as %q: a body field must never choose the user", fake.lastUserID)
	}
}

func TestSearchPassesFiltersThrough(t *testing.T) {
	fake := &fakeSearch{}
	rec := request(searchDeps(fake), "POST", "/api/v2/search",
		`{"query":"cafe","platform":"instagram","category":"food","subcategory":"cafes","saved_date":"2026-09-02"}`,
		"Bearer good.token")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	filters := fake.lastFilters
	if len(filters.Platforms) != 1 || filters.Platforms[0] != "instagram" {
		t.Errorf("platforms = %v", filters.Platforms)
	}
	if filters.Category != "food" || filters.Subcategory != "cafes" || filters.SavedDate != "2026-09-02" {
		t.Errorf("filters = %+v", filters)
	}
}

func TestSearchRequiresAQuery(t *testing.T) {
	for _, body := range []string{`{}`, `{"query":"   "}`} {
		rec := request(searchDeps(&fakeSearch{}), "POST", "/api/v2/search", body, "Bearer good.token")
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("body %s status = %d, want 422", body, rec.Code)
		}
	}
}

func TestSearchRejectsAnOutOfRangeLimit(t *testing.T) {
	rec := request(searchDeps(&fakeSearch{}), "POST", "/api/v2/search", `{"query":"cafe","limit":50}`, "Bearer good.token")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
}

func TestSearchRejectsAMalformedSavedDate(t *testing.T) {
	rec := request(searchDeps(&fakeSearch{}), "POST", "/api/v2/search", `{"query":"cafe","saved_date":"week"}`, "Bearer good.token")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
}

func TestSearchRejectsAnUnknownPlatform(t *testing.T) {
	rec := request(searchDeps(&fakeSearch{}), "POST", "/api/v2/search", `{"query":"cafe","platform":"myspace"}`, "Bearer good.token")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if code := decodeError(t, rec).Error.Code; code != "invalid_platform" {
		t.Errorf("error_code = %q", code)
	}
}

func TestSearchRequiresASession(t *testing.T) {
	rec := request(searchDeps(&fakeSearch{}), "POST", "/api/v2/search", `{"query":"cafe"}`, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestSearchFailureIsA500(t *testing.T) {
	rec := request(searchDeps(&fakeSearch{err: errFake}), "POST", "/api/v2/search", `{"query":"cafe"}`, "Bearer good.token")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if code := decodeError(t, rec).Error.Code; code != "search_failed" {
		t.Errorf("error_code = %q", code)
	}
}

func TestSearchIsMetered(t *testing.T) {
	fake := &fakeSearch{}
	deps := searchDeps(fake)
	deps.Limiter = denyingLimiter{}

	rec := request(deps, "POST", "/api/v2/search", `{"query":"cafe"}`, "Bearer good.token")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: a query costs a provider call", rec.Code)
	}
	if fake.lastQuery != "" {
		t.Error("a refused query still reached the search service")
	}
}

// Without a working limiter the safe answer is a stable 503, exactly as it is
// for submissions: an unmetered provider call is the thing being prevented.
func TestSearchFailsClosedWithoutAWorkingLimiter(t *testing.T) {
	for _, tt := range []struct {
		name    string
		limiter RateLimiter
	}{
		{"no limiter configured", nil},
		{"redis unavailable", unavailableLimiter{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeSearch{}
			deps := searchDeps(fake)
			deps.Limiter = tt.limiter

			rec := request(deps, "POST", "/api/v2/search", `{"query":"cafe"}`, "Bearer good.token")
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", rec.Code)
			}
			if code := decodeError(t, rec).Error.Code; code != "search_unavailable" {
				t.Errorf("error_code = %q", code)
			}
			if fake.lastQuery != "" {
				t.Error("an unmetered query reached the search service")
			}
		})
	}
}

func TestSearchWithNoResultsSerializesAnEmptyList(t *testing.T) {
	fake := &fakeSearch{response: search.Response{Query: "nothing", SearchMode: "empty"}}

	rec := request(searchDeps(fake), "POST", "/api/v2/search", `{"query":"nothing"}`, "Bearer good.token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"results":[]`) {
		t.Errorf("body = %s, want an empty list rather than null", body)
	}
}
