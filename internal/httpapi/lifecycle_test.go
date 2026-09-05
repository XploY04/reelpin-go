package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/lifecycle"
)

func lifecycleDeps(fake *fakeLifecycle) Deps {
	deps := testDeps(&fakePinger{})
	deps.Lifecycle = fake
	return deps
}

func TestDeleteReel(t *testing.T) {
	fake := &fakeLifecycle{}

	rec := request(lifecycleDeps(fake), "DELETE", "/api/v1/reels/"+testReelID, "", "Bearer good.token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if fake.lastAction != "delete_reel" || fake.lastUserID != testUserID {
		t.Errorf("service saw %q as %q", fake.lastAction, fake.lastUserID)
	}

	// The bare alias the shipped app still calls.
	if rec := request(lifecycleDeps(fake), "DELETE", "/reels/"+testReelID, "", "Bearer good.token"); rec.Code != http.StatusOK {
		t.Errorf("bare alias status = %d", rec.Code)
	}
}

func TestDeletingSomeoneElsesReelIs404(t *testing.T) {
	fake := &fakeLifecycle{err: lifecycle.ErrNotFound}

	rec := request(lifecycleDeps(fake), "DELETE", "/api/v1/reels/"+testReelID, "", "Bearer good.token")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if code := decodeError(t, rec).ErrorCode; code != "reel_not_found" {
		t.Errorf("error_code = %q", code)
	}

	// A malformed id is the same 404, so probing tells nothing apart.
	if rec := request(lifecycleDeps(fake), "DELETE", "/api/v1/reels/not-a-uuid", "", "Bearer good.token"); rec.Code != http.StatusNotFound {
		t.Errorf("malformed id status = %d, want 404", rec.Code)
	}
}

func TestDeleteAccountReportsTheAppleLimitation(t *testing.T) {
	fake := &fakeLifecycle{}

	rec := request(lifecycleDeps(fake), "DELETE", "/api/v1/account", "", "Bearer good.token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if fake.lastAction != "delete_account" || fake.lastUserID != testUserID {
		t.Errorf("service saw %q as %q", fake.lastAction, fake.lastUserID)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "Apple ID settings") {
		t.Errorf("the response hides the Apple limitation: %s", body)
	}
}

// A partial delete is reported as a failure, so the client retries. The
// operation is safe to repeat.
func TestAPartialAccountDeleteIsAFailure(t *testing.T) {
	fake := &fakeLifecycle{err: errors.New("the auth service is down")}

	rec := request(lifecycleDeps(fake), "DELETE", "/api/v1/account", "", "Bearer good.token")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := decodeError(t, rec)
	if body.ErrorCode != "account_delete_failed" {
		t.Errorf("error_code = %q", body.ErrorCode)
	}
	if strings.Contains(body.Detail, "auth service is down") {
		t.Error("the internal error leaked into the response")
	}
}

func TestDeletionNeedsASession(t *testing.T) {
	for _, target := range []string{"/api/v1/reels/" + testReelID, "/api/v1/account"} {
		rec := request(lifecycleDeps(&fakeLifecycle{}), "DELETE", target, "", "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s status = %d, want 401", target, rec.Code)
		}
	}
}
