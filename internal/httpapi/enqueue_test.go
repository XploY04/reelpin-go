package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/enqueue"
	"github.com/XploY04/reelpin-go/internal/jobs"
)

func post(deps Deps, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	New(deps).Routes().ServeHTTP(rec, req)
	return rec
}

func TestEnqueueAcceptsASession(t *testing.T) {
	enqueuer := &fakeEnqueuer{}
	deps := testDeps(&fakePinger{})
	deps.Enqueue = enqueuer

	rec := post(deps, "/api/v1/processing-jobs/reels",
		`{"url":"https://www.instagram.com/reel/C8abc123/","collection_ids":["fedcba98-0000-4000-8000-000000000001"]}`,
		map[string]string{"Authorization": "Bearer good.token"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if enqueuer.last.UserID != testUserID {
		t.Errorf("enqueued for %q, want the token subject", enqueuer.last.UserID)
	}
	if len(enqueuer.last.CollectionIDs) != 1 {
		t.Errorf("collection ids = %v", enqueuer.last.CollectionIDs)
	}

	var body jobs.Response
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not a job: %v", err)
	}
	if body.ID != testJobID {
		t.Errorf("id = %q, want the private job id", body.ID)
	}
	if body.CollectionIDs == nil {
		t.Error("collection_ids must be an array, not null")
	}
}

func TestEnqueueAcceptsAShareToken(t *testing.T) {
	enqueuer := &fakeEnqueuer{}
	deps := testDeps(&fakePinger{})
	deps.Enqueue = enqueuer
	deps.ShareTokens = &fakeShareTokens{userID: otherUserID}
	// A share token must work without any Authorization header at all.
	deps.Auth = fakeAuth{err: errFake}

	rec := post(deps, "/api/v1/processing-jobs/reels",
		`{"raw_payload_text":"look https://www.instagram.com/reel/C8abc123/"}`,
		map[string]string{"X-Share-Token": "device-token"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if enqueuer.last.UserID != otherUserID {
		t.Errorf("enqueued for %q, want the share token's owner", enqueuer.last.UserID)
	}
}

func TestUnknownShareTokenIsRejected(t *testing.T) {
	deps := testDeps(&fakePinger{})
	deps.ShareTokens = &fakeShareTokens{}

	rec := post(deps, "/api/v1/processing-jobs/reels", `{"url":"https://x.com/a/status/1"}`,
		map[string]string{"X-Share-Token": "revoked"})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if code := decodeError(t, rec).ErrorCode; code != "invalid_share_token" {
		t.Errorf("error_code = %q, want invalid_share_token", code)
	}
}

func TestShareTokenIsOnlyAcceptedOnEnqueue(t *testing.T) {
	deps := testDeps(&fakePinger{})
	deps.ShareTokens = &fakeShareTokens{userID: otherUserID}
	deps.Auth = fakeAuth{err: errFake}

	// A device token must not unlock the library.
	for _, target := range []string{"/api/v1/reels", "/api/v1/account/entitlements"} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("X-Share-Token", "device-token")
		rec := httptest.NewRecorder()
		New(deps).Routes().ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s status = %d, want 401", target, rec.Code)
		}
	}
}

func TestEnqueueIgnoresUserIDInTheBody(t *testing.T) {
	enqueuer := &fakeEnqueuer{}
	deps := testDeps(&fakePinger{})
	deps.Enqueue = enqueuer

	rec := post(deps, "/api/v1/processing-jobs/reels",
		`{"url":"https://x.com/a/status/1234567890","user_id":"`+otherUserID+`"}`,
		map[string]string{"Authorization": "Bearer good.token"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if enqueuer.last.UserID != testUserID {
		t.Fatalf("enqueued for %q: the body must never choose the user", enqueuer.last.UserID)
	}
}

func TestEnqueueErrors(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name: "no link in the share", err: enqueue.ErrNoURL,
			wantStatus: http.StatusBadRequest, wantCode: "invalid_request",
		},
		{
			name: "too many active jobs",
			err: &enqueue.LimitError{
				Code: "too_many_active_jobs", Message: "You already have too many reels processing.",
				Detail: "User already has 4 active jobs, which meets the active job limit of 4.",
			},
			wantStatus: http.StatusTooManyRequests, wantCode: "too_many_active_jobs",
		},
		{
			name: "hourly submission limit",
			err: &enqueue.LimitError{
				Code: "submission_rate_limited", Message: "You have reached the current submission limit.",
				Detail: "User already submitted 20 jobs in the last hour, which meets the hourly submission limit of 20.",
			},
			wantStatus: http.StatusTooManyRequests, wantCode: "submission_rate_limited",
		},
		{
			name: "anything else", err: errors.New("the database is down"),
			wantStatus: http.StatusInternalServerError, wantCode: "processing_job_enqueue_failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := testDeps(&fakePinger{})
			deps.Enqueue = &fakeEnqueuer{err: tt.err}

			rec := post(deps, "/api/v1/processing-jobs/reels", `{"url":"https://x.com/a/status/1"}`,
				map[string]string{"Authorization": "Bearer good.token"})

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			body := decodeError(t, rec)
			if body.ErrorCode != tt.wantCode {
				t.Errorf("error_code = %q, want %q", body.ErrorCode, tt.wantCode)
			}
			if tt.wantStatus == http.StatusTooManyRequests && rec.Header().Get("Retry-After") == "" {
				t.Error("a submission limit carries no Retry-After")
			}
			if body.Detail == "the database is down" {
				t.Error("the internal error leaked into the response")
			}
		})
	}
}

func TestProcessReelAliasIsTheSameService(t *testing.T) {
	enqueuer := &fakeEnqueuer{}
	deps := testDeps(&fakePinger{})
	deps.Enqueue = enqueuer

	for _, target := range []string{
		"/api/v1/processing-jobs/reels", "/processing-jobs/reels",
		"/api/v1/process-reel", "/process-reel",
	} {
		rec := post(deps, target, `{"url":"https://x.com/a/status/1234567890"}`,
			map[string]string{"Authorization": "Bearer good.token"})
		if rec.Code != http.StatusOK {
			t.Errorf("%s status = %d, want 200 (%s)", target, rec.Code, rec.Body.String())
		}
	}
	if enqueuer.calls != 4 {
		t.Errorf("the service ran %d times for 4 routes", enqueuer.calls)
	}
}

func TestShareTokenMintAndRevoke(t *testing.T) {
	deps := testDeps(&fakePinger{})
	deps.ShareTokens = &fakeShareTokens{token: "raw-token-value", revoked: 3}

	rec := post(deps, "/api/v1/share-tokens", `{}`, map[string]string{"Authorization": "Bearer good.token"})
	if rec.Code != http.StatusOK {
		t.Fatalf("mint status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var minted shareTokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &minted); err != nil {
		t.Fatalf("mint body: %v", err)
	}
	if minted.ShareToken != "raw-token-value" {
		t.Errorf("share_token = %q", minted.ShareToken)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/share-tokens", nil)
	req.Header.Set("Authorization", "Bearer good.token")
	revoke := httptest.NewRecorder()
	New(deps).Routes().ServeHTTP(revoke, req)

	if revoke.Code != http.StatusOK {
		t.Fatalf("revoke status = %d, want 200 (%s)", revoke.Code, revoke.Body.String())
	}
	if !strings.Contains(revoke.Body.String(), "revoked 3 share token(s)") {
		t.Errorf("revoke body = %s", revoke.Body.String())
	}
}

func TestMintedTokenIsNeverLogged(t *testing.T) {
	var logs strings.Builder
	deps := testDeps(&fakePinger{})
	deps.ShareTokens = &fakeShareTokens{token: "super-secret-token"}
	deps.Logger = slog.New(slog.NewJSONHandler(&logs, nil))

	if rec := post(deps, "/api/v1/share-tokens", `{}`,
		map[string]string{"Authorization": "Bearer good.token"}); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(logs.String(), "super-secret-token") {
		t.Fatalf("the minted token was logged: %s", logs.String())
	}
}

func TestShareTokenRoutesRequireASession(t *testing.T) {
	deps := testDeps(&fakePinger{})
	deps.ShareTokens = &fakeShareTokens{userID: otherUserID}

	// Minting with a device token would let a stolen token mint fresh ones.
	rec := post(deps, "/api/v1/share-tokens", `{}`, map[string]string{"X-Share-Token": "device-token"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
