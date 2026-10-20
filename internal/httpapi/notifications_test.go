package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/notify"
)

func notifyDeps(fake *fakeNotifications) Deps {
	deps := testDeps(&fakePinger{})
	deps.Notifications = fake
	return deps
}

func send(deps Deps, method, target, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer good.token")
	rec := httptest.NewRecorder()
	routes(deps).ServeHTTP(rec, req)
	return rec
}

func TestRegisteringADeviceRunsAsTheTokenSubject(t *testing.T) {
	fake := &fakeNotifications{}
	rec := send(notifyDeps(fake), "POST", "/api/v2/device-push-tokens",
		`{"token":"device-token","platform":"ios"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if fake.lastUserID != testUserID {
		t.Errorf("registered for %q, want the token subject", fake.lastUserID)
	}
	if fake.lastPlatform != "ios" {
		t.Errorf("platform = %q", fake.lastPlatform)
	}
}

// TestARegistrationCarriesItsBuild is what a forced-update rollout reads: the
// build fields reach the service as sent, blank labels as absent, and a
// registration without them still works for the builds that predate them.
func TestARegistrationCarriesItsBuild(t *testing.T) {
	fake := &fakeNotifications{}
	rec := send(notifyDeps(fake), "POST", "/api/v2/device-push-tokens",
		`{"token":"device-token","platform":"android","app_version":" 2.4.0 ","app_build":412,"timezone":"Asia/Kolkata","locale":"  "}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	device := fake.lastDevice
	if device.AppVersion == nil || *device.AppVersion != "2.4.0" {
		t.Errorf("app_version = %v, want 2.4.0 trimmed", device.AppVersion)
	}
	if device.AppBuild == nil || *device.AppBuild != 412 {
		t.Errorf("app_build = %v, want 412", device.AppBuild)
	}
	if device.Timezone == nil || *device.Timezone != "Asia/Kolkata" {
		t.Errorf("timezone = %v", device.Timezone)
	}
	if device.Locale != nil {
		t.Errorf("locale = %q, want a blank one read as not reported", *device.Locale)
	}

	rec = send(notifyDeps(fake), "POST", "/api/v2/device-push-tokens", `{"token":"device-token","platform":"ios"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("an older build's registration status = %d (%s)", rec.Code, rec.Body.String())
	}
	if fake.lastDevice.AppBuild != nil || fake.lastDevice.AppVersion != nil {
		t.Errorf("device = %+v, want no build fields", fake.lastDevice)
	}
}

func TestRemovingADeviceTakesTheTokenAlone(t *testing.T) {
	rec := send(notifyDeps(&fakeNotifications{}), "DELETE", "/api/v2/device-push-tokens",
		`{"token":"t","app_build":412}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%s)", rec.Code, rec.Body.String())
	}
}

func TestADeviceTokenIsNeverEchoedBack(t *testing.T) {
	// It is a credential: a response that repeats it is a response that can
	// leak it through a log, a proxy or a crash report.
	rec := send(notifyDeps(&fakeNotifications{}), "POST", "/api/v2/device-push-tokens",
		`{"token":"super-secret-device-token","platform":"android"}`)

	if strings.Contains(rec.Body.String(), "super-secret-device-token") {
		t.Fatalf("the token came back in the response: %s", rec.Body.String())
	}
}

func TestDeviceRegistrationValidatesItsInput(t *testing.T) {
	tests := []struct{ name, body string }{
		{"no token", `{"platform":"ios"}`},
		{"unknown platform", `{"token":"t","platform":"blackberry"}`},
		{"unknown field", `{"token":"t","platform":"ios","user_id":"someone"}`},
		{"negative build", `{"token":"t","platform":"ios","app_build":-1}`},
		{"build past an integer", `{"token":"t","platform":"ios","app_build":2147483648}`},
		{"build as text", `{"token":"t","platform":"ios","app_build":"42"}`},
		{"fractional build", `{"token":"t","platform":"ios","app_build":4.2}`},
		{"long version", `{"token":"t","platform":"ios","app_version":"` + strings.Repeat("1", 65) + `"}`},
		{"long timezone", `{"token":"t","platform":"ios","timezone":"` + strings.Repeat("a", 65) + `"}`},
		{"long locale", `{"token":"t","platform":"ios","locale":"` + strings.Repeat("a", 36) + `"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := send(notifyDeps(&fakeNotifications{}), "POST", "/api/v2/device-push-tokens", tt.body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestAnotherUsersDeviceTokenIsNotFound(t *testing.T) {
	rec := send(notifyDeps(&fakeNotifications{err: notify.ErrNotFound}), "DELETE",
		"/api/v2/device-push-tokens", `{"token":"someone-elses"}`)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if code := decodeError(t, rec).Error.Code; code != "device_token_not_found" {
		t.Errorf("code = %q", code)
	}
}

func TestMarkingOpenedRequiresAnOwnedNotification(t *testing.T) {
	fake := &fakeNotifications{}
	rec := send(notifyDeps(fake), "POST",
		"/api/v2/notifications/"+testReelID+"/opened", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if fake.lastUserID != testUserID {
		t.Errorf("recorded for %q", fake.lastUserID)
	}

	// Another user's, already opened, and malformed all answer 404.
	missing := send(notifyDeps(&fakeNotifications{err: notify.ErrNotFound}), "POST",
		"/api/v2/notifications/"+testReelID+"/opened", `{}`)
	if missing.Code != http.StatusNotFound {
		t.Errorf("missing status = %d", missing.Code)
	}
	malformed := send(notifyDeps(&fakeNotifications{}), "POST",
		"/api/v2/notifications/not-a-uuid/opened", `{}`)
	if malformed.Code != http.StatusNotFound {
		t.Errorf("malformed status = %d", malformed.Code)
	}
}

func TestNotificationRoutesRequireASession(t *testing.T) {
	for _, target := range []string{
		"/api/v2/device-push-tokens",
		"/api/v2/notifications/" + testReelID + "/opened",
	} {
		req := httptest.NewRequest("POST", target, strings.NewReader(`{"token":"t","platform":"ios"}`))
		rec := httptest.NewRecorder()
		routes(notifyDeps(&fakeNotifications{})).ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s status = %d, want 401", target, rec.Code)
		}
	}
}

func TestCampaignsAreNotInTheProductAPI(t *testing.T) {
	// Campaign administration is an operator command. A route here would be a
	// route an attacker can reach with a user's token.
	for _, route := range newServer(testDeps(&fakePinger{})).RouteManifest() {
		if strings.Contains(route.Path, "campaign") {
			t.Errorf("%s exposes campaign administration on the product API", route.Path)
		}
	}
}
