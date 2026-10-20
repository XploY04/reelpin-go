package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func profileDeps(fake *fakeProfiles) Deps {
	deps := testDeps(&fakePinger{})
	deps.Profiles = fake
	return deps
}

func put(deps Deps, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("PUT", target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	routes(deps).ServeHTTP(rec, req)
	return rec
}

func putProfile(deps Deps, body string) *httptest.ResponseRecorder {
	return put(deps, "/api/v2/profile", body, map[string]string{"Authorization": "Bearer good.token"})
}

func TestPutProfileCreatesThenUpdatesOnlyWhatIsSent(t *testing.T) {
	fake := &fakeProfiles{}
	deps := profileDeps(fake)

	rec := putProfile(deps, `{"email":" me@example.com ","full_name":"Asha Rao"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if fake.lastUserID != testUserID {
		t.Errorf("saved as %q, want the token subject", fake.lastUserID)
	}

	rec = putProfile(deps, `{"avatar_url":"https://example.com/a.png","full_name":"  "}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["id"] != testUserID {
		t.Errorf("id = %v", body["id"])
	}
	if body["email"] != "me@example.com" || body["full_name"] != "Asha Rao" {
		t.Errorf("a blank or missing field overwrote the stored value: %v", body)
	}
	if body["avatar_url"] != "https://example.com/a.png" {
		t.Errorf("avatar_url = %v", body["avatar_url"])
	}
}

func TestAnotherUsersIDInTheBodyIsRejected(t *testing.T) {
	fake := &fakeProfiles{}
	rec := putProfile(profileDeps(fake), `{"id":"`+otherUserID+`","full_name":"Mallory"}`)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: the id is never accepted from a body", rec.Code)
	}
	if len(fake.stored) != 0 {
		t.Error("something was stored")
	}
}

func TestProfileValidation(t *testing.T) {
	tests := []struct{ name, body, field string }{
		{"email without a domain", `{"email":"me@"}`, "email"},
		{"email with a display name", `{"email":"Me <me@example.com>"}`, "email"},
		{"full name too long", `{"full_name":"` + strings.Repeat("a", 201) + `"}`, "full_name"},
		{"avatar is not a URL", `{"avatar_url":"not a url"}`, "avatar_url"},
		{"avatar is not http", `{"avatar_url":"javascript:alert(1)"}`, "avatar_url"},
		{"not JSON", `nope`, "body"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := putProfile(profileDeps(&fakeProfiles{}), tt.body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (%s)", rec.Code, rec.Body.String())
			}
			if field := decodeError(t, rec).Error.Details["field"]; field != tt.field {
				t.Errorf("field = %v, want %s", field, tt.field)
			}
		})
	}
}

func TestGetProfileReturnsOnlyTheCallersOwn(t *testing.T) {
	fake := &fakeProfiles{}
	deps := profileDeps(fake)
	putProfile(deps, `{"full_name":"Asha Rao"}`)

	rec := serve(deps, "GET", "/api/v2/profile", "Bearer good.token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if fake.lastUserID != testUserID {
		t.Errorf("read %q, want the token subject", fake.lastUserID)
	}
}

func TestGetProfileWithNoneSavedIs404(t *testing.T) {
	rec := serve(profileDeps(&fakeProfiles{}), "GET", "/api/v2/profile", "Bearer good.token")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if code := decodeError(t, rec).Error.Code; code != "profile_not_found" {
		t.Errorf("code = %q", code)
	}
}

func TestProfileNeedsASession(t *testing.T) {
	deps := profileDeps(&fakeProfiles{})
	if rec := serve(deps, "GET", "/api/v2/profile", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET status = %d, want 401", rec.Code)
	}
	if rec := put(deps, "/api/v2/profile", `{}`, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("PUT status = %d, want 401", rec.Code)
	}
}

func TestADriverErrorOnTheProfileIsNotLeaked(t *testing.T) {
	deps := profileDeps(&fakeProfiles{err: errFake})
	for _, rec := range []*httptest.ResponseRecorder{
		serve(deps, "GET", "/api/v2/profile", "Bearer good.token"),
		putProfile(deps, `{"full_name":"Asha"}`),
	} {
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
		if strings.Contains(rec.Body.String(), errFake.Error()) {
			t.Error("the driver error reached the body")
		}
	}
}
