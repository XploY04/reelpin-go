package httpapi

import (
	"encoding/json"
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

func TestDeletingAReelAnswersNoContent(t *testing.T) {
	fake := &fakeLifecycle{}
	rec := serve(lifecycleDeps(fake), "DELETE", "/api/v2/reels/"+testReelID, "Bearer good.token")

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (%s)", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want nothing: the client already knows which reel", rec.Body.String())
	}
	if fake.lastUserID != testUserID {
		t.Errorf("deleted as %q, want the token subject", fake.lastUserID)
	}
	if fake.lastReelID != testReelID {
		t.Errorf("deleted %q, want the path id", fake.lastReelID)
	}
}

func TestDeletingSomebodyElsesReelIsNotFound(t *testing.T) {
	// The service cannot tell "not yours" from "does not exist", and neither
	// can the response: a different answer would confirm the id is real.
	fake := &fakeLifecycle{deleteErr: lifecycle.ErrNotFound}
	rec := serve(lifecycleDeps(fake), "DELETE", "/api/v2/reels/"+testReelID, "Bearer good.token")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if code := decodeError(t, rec).Error.Code; code != "reel_not_found" {
		t.Errorf("code = %q", code)
	}
}

func TestDeletingWhileAnAccountIsGoingAwayIsAConflict(t *testing.T) {
	fake := &fakeLifecycle{deleteErr: lifecycle.ErrDeletionPending}
	rec := serve(lifecycleDeps(fake), "DELETE", "/api/v2/reels/"+testReelID, "Bearer good.token")

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if code := decodeError(t, rec).Error.Code; code != "account_deletion_pending" {
		t.Errorf("code = %q", code)
	}
}

func TestDeletingAReelNeedsASession(t *testing.T) {
	fake := &fakeLifecycle{}
	rec := serve(lifecycleDeps(fake), "DELETE", "/api/v2/reels/"+testReelID, "")

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if fake.calls != 0 {
		t.Error("the service was called without a verified user")
	}
}

func TestAReelDeleteFailureNeverLeaksTheReason(t *testing.T) {
	fake := &fakeLifecycle{deleteErr: errFake}
	rec := serve(lifecycleDeps(fake), "DELETE", "/api/v2/reels/"+testReelID, "Bearer good.token")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, errFake.Error()) {
		t.Errorf("the internal error leaked: %s", body)
	}
}

func TestAccountDeletionReportsBothHalvesTruthfully(t *testing.T) {
	tests := []struct {
		name         string
		report       lifecycle.Report
		wantIdentity bool
		wantPending  bool
	}{
		{
			name:         "both halves done",
			report:       lifecycle.Report{DatabaseCleaned: true, IdentityDeleted: true, Saves: 3},
			wantIdentity: true,
		},
		{
			// The case the old branch got wrong: data gone, sign-in still
			// working, and the response saying so rather than "deleted".
			name:        "data gone, identity still there",
			report:      lifecycle.Report{DatabaseCleaned: true, Pending: true, Saves: 3},
			wantPending: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeLifecycle{report: tt.report}
			rec := serve(lifecycleDeps(fake), "DELETE", "/api/v2/account", "Bearer good.token")

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
			}
			var body struct {
				DataDeleted     bool `json:"data_deleted"`
				IdentityDeleted bool `json:"identity_deleted"`
				Pending         bool `json:"pending"`
				Removed         struct {
					Saves int `json:"saves"`
				} `json:"removed"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not the report: %v", err)
			}
			if !body.DataDeleted {
				t.Error("data_deleted = false after a successful cleanup")
			}
			if body.IdentityDeleted != tt.wantIdentity {
				t.Errorf("identity_deleted = %v, want %v", body.IdentityDeleted, tt.wantIdentity)
			}
			if body.Pending != tt.wantPending {
				t.Errorf("pending = %v, want %v", body.Pending, tt.wantPending)
			}
			if body.Removed.Saves != tt.report.Saves {
				t.Errorf("removed.saves = %d, want %d", body.Removed.Saves, tt.report.Saves)
			}
			if fake.lastUserID != testUserID {
				t.Errorf("deleted account %q, want the token subject", fake.lastUserID)
			}
		})
	}
}

func TestDeletingAnAccountNeedsASession(t *testing.T) {
	fake := &fakeLifecycle{}
	rec := serve(lifecycleDeps(fake), "DELETE", "/api/v2/account", "")

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if fake.calls != 0 {
		t.Error("an account was deleted without a verified user")
	}
}
