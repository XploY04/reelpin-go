package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/XploY04/reelpin-go/internal/entitlements"
	"github.com/XploY04/reelpin-go/internal/reels"
)

func decodeEntitlements(t *testing.T, body []byte) entitlements.Entitlements {
	t.Helper()
	var ent entitlements.Entitlements
	if err := json.Unmarshal(body, &ent); err != nil {
		t.Fatalf("body is not entitlements: %v", err)
	}
	return ent
}

func TestEntitlementsFreePlan(t *testing.T) {
	deps := testDeps(&fakePinger{})
	fakeReader := &fakeReels{stats: reels.LibraryStats{
		TotalReels: 7, TotalPinnedLocations: 3, TotalTags: 5,
		TotalCategories: 2, TotalSubcategories: 4,
	}}
	fakePlan := &fakeEntitlements{}
	deps.Reels = fakeReader
	deps.Entitlements = fakePlan

	rec := serve(deps, "GET", "/api/v2/account/entitlements", "Bearer good.token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	if fakeReader.lastUserID != testUserID {
		t.Errorf("usage read as %q, want the token subject", fakeReader.lastUserID)
	}
	if fakePlan.lastUserID != testUserID {
		t.Errorf("plan checked for %q, want the token subject", fakePlan.lastUserID)
	}

	ent := decodeEntitlements(t, rec.Body.Bytes())
	if ent.Plan != "free" || ent.IsPremium {
		t.Errorf("plan = %q is_premium = %v, want free/false", ent.Plan, ent.IsPremium)
	}
	if ent.UserID != testUserID {
		t.Errorf("user_id = %q, want %q", ent.UserID, testUserID)
	}
	if ent.Usage.SavedReels != 7 || ent.Usage.PinnedLocations != 3 || ent.Usage.Tags != 5 ||
		ent.Usage.Categories != 2 || ent.Usage.Subcategories != 4 {
		t.Errorf("usage = %+v, want the library totals", ent.Usage)
	}
	if ent.Limits.SavedReels == nil || *ent.Limits.SavedReels != 25 {
		t.Errorf("free saved_reels limit = %v, want 25", ent.Limits.SavedReels)
	}
	if len(ent.PlanCards) != 2 {
		t.Errorf("free plan cards = %d, want 2", len(ent.PlanCards))
	}
}

func TestEntitlementsProPlan(t *testing.T) {
	deps := testDeps(&fakePinger{})
	deps.Reels = &fakeReels{stats: reels.LibraryStats{TotalReels: 100}}
	deps.Entitlements = &fakeEntitlements{pro: map[string]bool{testUserID: true}}

	rec := serve(deps, "GET", "/api/v2/account/entitlements", "Bearer good.token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	ent := decodeEntitlements(t, rec.Body.Bytes())
	if ent.Plan != "pro" || !ent.IsPremium {
		t.Errorf("plan = %q is_premium = %v, want pro/true", ent.Plan, ent.IsPremium)
	}
	if ent.Limits.SavedReels != nil {
		t.Errorf("pro saved_reels limit = %v, want unlimited (null)", ent.Limits.SavedReels)
	}
	if !ent.Features.UnlimitedSaves {
		t.Errorf("pro plan lacks unlimited saves: %+v", ent.Features)
	}
}

func TestEntitlementsRestrictedWhenUsageFails(t *testing.T) {
	deps := testDeps(&fakePinger{})
	deps.Reels = &fakeReels{err: errFake}
	deps.Entitlements = &fakeEntitlements{}

	rec := serve(deps, "GET", "/api/v2/account/entitlements", "Bearer good.token")
	// It degrades rather than failing: a 500 would blank the whole account tab.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (restricted body)", rec.Code)
	}

	ent := decodeEntitlements(t, rec.Body.Bytes())
	if ent.Plan != "restricted" || !ent.CurrentEntitlement.Restricted {
		t.Errorf("plan = %q restricted = %v, want restricted/true", ent.Plan, ent.CurrentEntitlement.Restricted)
	}
	if ent.CurrentEntitlement.ErrorMessage == nil {
		t.Errorf("restricted response has no error message")
	}
}

func TestEntitlementsRequiresAuth(t *testing.T) {
	rec := serve(testDeps(&fakePinger{}), "GET", "/api/v2/account/entitlements", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if code := decodeError(t, rec).Error.Code; code != "authentication_required" {
		t.Errorf("error code = %q, want authentication_required", code)
	}
}
