package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/XploY04/reelpin-go/internal/reels"
)

func TestLibraryStats(t *testing.T) {
	stats := reels.LibraryStats{
		TotalReels: 7, TotalPinnedLocations: 3, TotalTags: 5,
		TotalCategories: 2, TotalSubcategories: 4,
	}
	deps := testDeps(&fakePinger{})
	deps.Reels = &fakeReels{stats: stats}

	rec := serve(deps, "GET", "/api/v1/account/library-stats", "Bearer good.token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body reels.LibraryStats
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not library stats: %v", err)
	}
	if body != stats {
		t.Errorf("stats = %+v, want %+v", body, stats)
	}
}

func TestLibraryStatsFailure(t *testing.T) {
	deps := testDeps(&fakePinger{})
	deps.Reels = &fakeReels{err: errFake}

	rec := serve(deps, "GET", "/api/v1/account/library-stats", "Bearer good.token")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if code := decodeError(t, rec).ErrorCode; code != "library_stats_failed" {
		t.Errorf("error_code = %q, want library_stats_failed", code)
	}
}

func TestEmptyLibraryStats(t *testing.T) {
	deps := testDeps(&fakePinger{})
	deps.Reels = &fakeReels{}

	rec := serve(deps, "GET", "/api/v1/account/library-stats", "Bearer good.token")
	var body reels.LibraryStats
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not library stats: %v", err)
	}
	if body != (reels.LibraryStats{}) {
		t.Errorf("stats = %+v, want zeros", body)
	}
}

func TestEntitlements(t *testing.T) {
	deps := testDeps(&fakePinger{})
	deps.Reels = &fakeReels{stats: reels.LibraryStats{TotalReels: 4, TotalPinnedLocations: 2, TotalTags: 3}}

	rec := serve(deps, "GET", "/api/v1/account/entitlements", "Bearer good.token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body EntitlementsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not entitlements: %v", err)
	}

	if body.UserID != testUserID || body.Plan != "free" || body.IsPremium {
		t.Errorf("plan = %+v", body.CurrentEntitlement)
	}
	if body.CurrentEntitlement.Restricted {
		t.Error("restricted = true on a healthy lookup")
	}
	if body.Usage.SavedReels != 4 || body.Usage.PinnedLocations != 2 || body.Usage.Tags != 3 {
		t.Errorf("usage = %+v", body.Usage)
	}
	if body.Limits.SavedReels == nil || *body.Limits.SavedReels != 25 {
		t.Errorf("saved_reels limit = %v, want 25", body.Limits.SavedReels)
	}
	if body.Limits.PinnedLocations == nil || *body.Limits.PinnedLocations != 10 {
		t.Errorf("pinned_locations limit = %v, want 10", body.Limits.PinnedLocations)
	}
	if body.Limits.SearchesPerMonth == nil || *body.Limits.SearchesPerMonth != 50 {
		t.Errorf("searches_per_month limit = %v, want 50", body.Limits.SearchesPerMonth)
	}
	if body.Pricing["pro_monthly"] != "$9.99" || body.Pricing["pro_yearly"] != "$79.99" || body.Pricing["free_monthly"] != "$0" {
		t.Errorf("pricing = %+v", body.Pricing)
	}
	if len(body.PlanCards) != 2 || body.PlanCards[0].CTALabel != "Current plan" || body.PlanCards[1].CTALabel != "Upgrade" {
		t.Errorf("plan cards = %+v", body.PlanCards)
	}
	if body.PlanCards[1].Limits.SavedReels != nil {
		t.Error("pro limits must stay unlimited")
	}
	if !body.Features["save_reels"] || body.Features["unlimited_saves"] {
		t.Errorf("features = %+v", body.Features)
	}
}

func TestRestrictedEntitlements(t *testing.T) {
	deps := testDeps(&fakePinger{})
	deps.Reels = &fakeReels{err: errFake}

	rec := serve(deps, "GET", "/api/v1/account/entitlements", "Bearer good.token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even when stats fail", rec.Code)
	}

	var body EntitlementsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not entitlements: %v", err)
	}
	if body.Plan != "restricted" || !body.CurrentEntitlement.Restricted {
		t.Errorf("entitlement = %+v", body.CurrentEntitlement)
	}
	if body.CurrentEntitlement.SubscriptionStatus != "unavailable" {
		t.Errorf("subscription_status = %q, want unavailable", body.CurrentEntitlement.SubscriptionStatus)
	}
	if body.CurrentEntitlement.ErrorMessage == nil ||
		*body.CurrentEntitlement.ErrorMessage != "Plan access could not be verified right now." {
		t.Errorf("error_message = %v", body.CurrentEntitlement.ErrorMessage)
	}
	if len(body.PlanCards) != 0 {
		t.Errorf("plan_cards = %+v, want empty", body.PlanCards)
	}
	for feature, enabled := range body.Features {
		if enabled {
			t.Errorf("feature %q is on in a restricted response", feature)
		}
	}
	if body.Usage != (Usage{}) {
		t.Errorf("usage = %+v, want zeros", body.Usage)
	}
}
