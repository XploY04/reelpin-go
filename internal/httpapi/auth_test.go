package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serve runs one authenticated-looking request against a server built from deps.
func serve(deps Deps, method, target, authorization string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	New(deps).Routes().ServeHTTP(rec, req)
	return rec
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) errorResponse {
	t.Helper()
	var body errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q is not JSON: %v", rec.Body.String(), err)
	}
	return body
}

func TestAuthorizationHeader(t *testing.T) {
	tests := []struct {
		name          string
		authorization string
		authErr       error
		wantStatus    int
		wantCode      string
	}{
		{name: "missing header", wantStatus: http.StatusUnauthorized, wantCode: "authentication_required"},
		{name: "not a bearer token", authorization: "Basic abc", wantStatus: http.StatusUnauthorized, wantCode: "authentication_required"},
		{name: "bearer with no token", authorization: "Bearer ", wantStatus: http.StatusUnauthorized, wantCode: "authentication_required"},
		{name: "rejected token", authorization: "Bearer bad.token", authErr: errFake, wantStatus: http.StatusUnauthorized, wantCode: "invalid_auth_token"},
		{name: "accepted token", authorization: "Bearer good.token", wantStatus: http.StatusOK},
		{name: "lowercase scheme", authorization: "bearer good.token", wantStatus: http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := testDeps(&fakePinger{})
			deps.Auth = fakeAuth{userID: testUserID, err: tt.authErr}

			rec := serve(deps, "GET", "/api/v1/reels", tt.authorization)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantCode == "" {
				return
			}

			body := decodeError(t, rec)
			if body.ErrorCode != tt.wantCode {
				t.Errorf("error_code = %q, want %q", body.ErrorCode, tt.wantCode)
			}
			if body.Success {
				t.Error("success = true, want false")
			}
		})
	}
}

func TestRejectedTokenNeverLeaksTheReason(t *testing.T) {
	deps := testDeps(&fakePinger{})
	deps.Auth = fakeAuth{err: errFake}

	rec := serve(deps, "GET", "/api/v1/reels", "Bearer bad.token")
	if got := rec.Body.String(); strings.Contains(got, errFake.Error()) {
		t.Fatalf("response leaks the verification error: %s", got)
	}
}

func TestUserIDComesFromTheTokenOnly(t *testing.T) {
	reader := &fakeReels{}
	deps := testDeps(&fakePinger{})
	deps.Reels = reader

	rec := serve(deps, "GET", "/api/v1/reels?user_id=someone-else", "Bearer good.token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if reader.lastUserID != testUserID {
		t.Fatalf("queried user %q, want %q", reader.lastUserID, testUserID)
	}
}

func TestBareRoutesAreNotRegistered(t *testing.T) {
	for _, path := range []string{
		"/reels",
		"/reels/filters",
		"/reels/category-filters",
		"/reels/" + testReelID,
		"/processing-jobs",
		"/processing-jobs/" + testJobID,
		"/account/library-stats",
	} {
		t.Run(path, func(t *testing.T) {
			rec := serve(testDeps(&fakePinger{}), "GET", path, "Bearer good.token")
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
		})
	}
}

func TestEntitlementsRouteIsNotRegistered(t *testing.T) {
	for _, path := range []string{"/api/v1/account/entitlements", "/account/entitlements"} {
		rec := serve(testDeps(&fakePinger{}), "GET", path, "Bearer good.token")
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404", path, rec.Code)
		}
	}
}
