package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/notify"
)

const testCampaignID = "88888888-8888-4888-8888-888888888888"

func notifyDeps(fake *fakeNotifications) Deps {
	deps := testDeps(&fakePinger{})
	deps.Notifications = fake
	return deps
}

func adminRequest(deps Deps, method, target, body, adminKey string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if adminKey != "" {
		req.Header.Set("X-Admin-Key", adminKey)
	}
	rec := httptest.NewRecorder()
	New(deps).Routes().ServeHTTP(rec, req)
	return rec
}

func TestDeviceTokenRoutes(t *testing.T) {
	fake := &fakeNotifications{}
	deps := notifyDeps(fake)

	rec := request(deps, "POST", "/api/v1/device-push-tokens",
		`{"token":"device-token","platform":"ios"}`, "Bearer good.token")
	if rec.Code != http.StatusOK {
		t.Fatalf("register status = %d (%s)", rec.Code, rec.Body.String())
	}
	if fake.lastAction != "register" || fake.lastUserID != testUserID {
		t.Errorf("register reached %q as %q", fake.lastAction, fake.lastUserID)
	}

	rec = request(deps, "DELETE", "/api/v1/device-push-tokens", `{"token":"device-token"}`, "Bearer good.token")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d (%s)", rec.Code, rec.Body.String())
	}
	if fake.lastAction != "delete" {
		t.Errorf("delete reached %q", fake.lastAction)
	}

	// Both need a token in the body.
	if rec := request(deps, "POST", "/api/v1/device-push-tokens", `{}`, "Bearer good.token"); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("a body with no token gave %d, want 422", rec.Code)
	}
}

func TestADeviceTokenIsNeverLogged(t *testing.T) {
	var logs strings.Builder
	deps := notifyDeps(&fakeNotifications{})
	deps.Logger = slog.New(slog.NewJSONHandler(&logs, nil))

	request(deps, "POST", "/api/v1/device-push-tokens",
		`{"token":"super-secret-device-token","platform":"ios"}`, "Bearer good.token")

	if strings.Contains(logs.String(), "super-secret-device-token") {
		t.Fatalf("the device token was logged: %s", logs.String())
	}
}

func TestNotificationOpenIsScopedToTheUser(t *testing.T) {
	fake := &fakeNotifications{err: notify.ErrNotFound}

	rec := request(notifyDeps(fake), "POST",
		"/api/v1/notifications/11111111-1111-4111-8111-111111111111/opened", `{}`, "Bearer good.token")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if code := decodeError(t, rec).ErrorCode; code != "notification_not_found" {
		t.Errorf("error_code = %q", code)
	}
}

func TestAdminRoutesNeedTheAdminKey(t *testing.T) {
	targets := []struct {
		method string
		target string
		body   string
	}{
		{"POST", "/api/v1/proactive-recall/push", `{"user_id":"u","title":"t"}`},
		{"POST", "/api/v1/admin/notification-campaigns", `{"title":"t","body":"b"}`},
		{"GET", "/api/v1/admin/notification-campaigns", ""},
		{"GET", "/api/v1/admin/notification-campaigns/" + testCampaignID, ""},
		{"POST", "/api/v1/admin/notification-campaigns/" + testCampaignID + "/send", `{}`},
		{"POST", "/api/v1/admin/notification-campaigns/" + testCampaignID + "/cancel", `{}`},
	}

	for _, tt := range targets {
		t.Run(tt.method+" "+tt.target, func(t *testing.T) {
			deps := notifyDeps(&fakeNotifications{campaign: notify.Campaign{CampaignID: testCampaignID}})

			if rec := adminRequest(deps, tt.method, tt.target, tt.body, ""); rec.Code != http.StatusUnauthorized {
				t.Errorf("no key gave %d, want 401", rec.Code)
			}
			if rec := adminRequest(deps, tt.method, tt.target, tt.body, "wrong-key"); rec.Code != http.StatusUnauthorized {
				t.Errorf("a wrong key gave %d, want 401", rec.Code)
			}
			if rec := adminRequest(deps, tt.method, tt.target, tt.body, testAdminKey); rec.Code != http.StatusOK {
				t.Errorf("the right key gave %d, want 200 (%s)", rec.Code, rec.Body.String())
			}
			// A user session is not admin access.
			if rec := request(deps, tt.method, tt.target, tt.body, "Bearer good.token"); rec.Code != http.StatusUnauthorized {
				t.Errorf("a user session gave %d, want 401", rec.Code)
			}
		})
	}
}

func TestAdminRoutesAreUnavailableWithoutAConfiguredKey(t *testing.T) {
	deps := notifyDeps(&fakeNotifications{})
	deps.AdminKey = ""

	rec := adminRequest(deps, "GET", "/api/v1/admin/notification-campaigns", "", "anything")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if code := decodeError(t, rec).ErrorCode; code != "dashboard_not_configured" {
		t.Errorf("error_code = %q", code)
	}
}

func TestProactiveRecallWithoutADeviceIsNotAFailure(t *testing.T) {
	fake := &fakeNotifications{err: notify.ErrNoDeviceTokens}

	rec := adminRequest(notifyDeps(fake), "POST", "/api/v1/proactive-recall/push",
		`{"user_id":"someone","title":"Come back"}`, testAdminKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: a user with no device is not an error", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no registered device") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestProactiveRecallCarriesRoutingData(t *testing.T) {
	fake := &fakeNotifications{}

	if rec := adminRequest(notifyDeps(fake), "POST", "/api/v1/proactive-recall/push",
		`{"user_id":"someone","title":"Come back","body":"You saved 12 reels"}`, testAdminKey); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(fake.sent) != 1 {
		t.Fatalf("sent %d notifications", len(fake.sent))
	}
	data := fake.sent[0].Data
	for _, key := range []string{"schema_version", "type", "target"} {
		if data[key] == "" {
			t.Errorf("data is missing %q, which the tap handler routes on", key)
		}
	}
}

func TestCampaignStateErrorsAre400(t *testing.T) {
	fake := &fakeNotifications{err: errors.New("a sending campaign cannot be cancelled")}

	rec := adminRequest(notifyDeps(fake), "POST",
		"/api/v1/admin/notification-campaigns/"+testCampaignID+"/cancel", `{}`, testAdminKey)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if code := decodeError(t, rec).ErrorCode; code != "campaign_state_invalid" {
		t.Errorf("error_code = %q", code)
	}
}
