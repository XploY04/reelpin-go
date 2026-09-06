package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/enqueue"
	"github.com/XploY04/reelpin-go/internal/ratelimit"
	"github.com/XploY04/reelpin-go/internal/reels"
)

const validKey = "9e0d4f3a-1111-4222-8333-444455556666"

func submitDeps(submitter *fakeSubmitter) Deps {
	deps := testDeps(&fakePinger{})
	deps.Enqueue = submitter
	deps.Limiter = allowAllLimiter{}
	return deps
}

// post sends a submission with the headers the contract requires.
func post(deps Deps, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	New(deps).Routes().ServeHTTP(rec, req)
	return rec
}

func TestSubmissionAcceptsAndReturnsTheJob(t *testing.T) {
	submitter := &fakeSubmitter{}
	rec := post(submitDeps(submitter), "/api/v2/processing-jobs/reels",
		`{"url":"https://www.instagram.com/reel/C8abc123/"}`,
		map[string]string{"Authorization": "Bearer good.token", "Idempotency-Key": validKey})

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", rec.Code, rec.Body.String())
	}
	if submitter.lastRequest.UserID != testUserID {
		t.Errorf("submitted as %q, want the token subject", submitter.lastRequest.UserID)
	}
	if submitter.lastRequest.IdempotencyKey != validKey {
		t.Errorf("idempotency key = %q", submitter.lastRequest.IdempotencyKey)
	}
	if submitter.lastRequest.Endpoint != "processing-jobs/reels" {
		t.Errorf("endpoint = %q: the key must be scoped per endpoint", submitter.lastRequest.Endpoint)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["id"] == nil || body["status"] != "queued" {
		t.Fatalf("body = %v", body)
	}
	if body["collection_ids"] == nil {
		t.Error("collection_ids is null; the app decodes a non-nullable list")
	}
}

func TestAlreadySavedAnswersTwoHundredWithTheReel(t *testing.T) {
	record := sampleReel(testReelID, testUserID)
	submitter := &fakeSubmitter{result: enqueue.Result{
		Kind: enqueue.AlreadySaved, Reel: &record,
	}}

	rec := post(submitDeps(submitter), "/api/v2/processing-jobs/reels",
		`{"url":"https://www.instagram.com/reel/C8abc123/"}`,
		map[string]string{"Authorization": "Bearer good.token", "Idempotency-Key": validKey})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for content the user already has", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["id"] != testReelID {
		t.Fatalf("body = %v, want the reel", body)
	}
}

func TestSubmissionRequiresAnIdempotencyKey(t *testing.T) {
	// Retrying without one cannot be made safe, so it is refused rather than
	// defaulted.
	rec := post(submitDeps(&fakeSubmitter{}), "/api/v2/processing-jobs/reels",
		`{"url":"https://www.instagram.com/reel/C8abc123/"}`,
		map[string]string{"Authorization": "Bearer good.token"})

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if code := decodeError(t, rec).Error.Code; code != "validation_error" {
		t.Errorf("code = %q", code)
	}
}

func TestSubmissionRejectsAnEmptyOrUnknownBody(t *testing.T) {
	tests := []struct{ name, body string }{
		{"no url", `{}`},
		{"unknown field", `{"url":"https://x.com/i/status/1","user_id":"someone-else"}`},
		{"not an object", `"just a string"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := post(submitDeps(&fakeSubmitter{}), "/api/v2/processing-jobs/reels", tt.body,
				map[string]string{"Authorization": "Bearer good.token", "Idempotency-Key": validKey})
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestSubmissionMapsUseCaseFailuresToTheContract(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"active job cap", enqueue.ErrActiveJobLimit, http.StatusTooManyRequests, "active_job_limit"},
		{"unsupported link", enqueue.ErrUnsupported, http.StatusUnprocessableEntity, "validation_error"},
		{"unreachable collection", enqueue.ErrCollectionUnreachable, http.StatusUnprocessableEntity, "validation_error"},
		{"anything else", errors.New("the database is away"), http.StatusInternalServerError, "submission_failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := post(submitDeps(&fakeSubmitter{err: tt.err}), "/api/v2/processing-jobs/reels",
				`{"url":"https://www.instagram.com/reel/C8abc123/"}`,
				map[string]string{"Authorization": "Bearer good.token", "Idempotency-Key": validKey})

			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tt.status, rec.Body.String())
			}
			if code := decodeError(t, rec).Error.Code; code != tt.code {
				t.Errorf("code = %q, want %q", code, tt.code)
			}
			if strings.Contains(rec.Body.String(), "database is away") {
				t.Error("the internal error leaked into the response")
			}
		})
	}
}

func TestACollectionTheUserCannotFileIntoNamesTheField(t *testing.T) {
	// The app has to know which field to send the user back to, and a 403
	// would tell a stranger the collection exists.
	rec := post(submitDeps(&fakeSubmitter{err: enqueue.ErrCollectionUnreachable}),
		"/api/v2/processing-jobs/reels",
		`{"url":"https://www.instagram.com/reel/C8abc123/","collection_ids":["`+testCollectionID+`"]}`,
		map[string]string{"Authorization": "Bearer good.token", "Idempotency-Key": validKey})

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%s)", rec.Code, rec.Body.String())
	}
	if field := decodeError(t, rec).Error.Details["field"]; field != "collection_ids" {
		t.Errorf("error.details.field = %v, want collection_ids", field)
	}
}

func TestAReusedKeyWithADifferentBodyIsAConflict(t *testing.T) {
	rec := post(submitDeps(&fakeSubmitter{}), "/api/v2/processing-jobs/reels",
		`{"url":"https://www.instagram.com/reel/C8abc123/"}`,
		map[string]string{"Authorization": "Bearer good.token", "Idempotency-Key": "conflict"})

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if code := decodeError(t, rec).Error.Code; code != "idempotency_conflict" {
		t.Errorf("code = %q", code)
	}
}

func TestSubmissionFailsClosedWithoutAWorkingLimiter(t *testing.T) {
	tests := []struct {
		name    string
		limiter RateLimiter
	}{
		{"no limiter configured", nil},
		{"redis unavailable", unavailableLimiter{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := testDeps(&fakePinger{})
			deps.Enqueue = &fakeSubmitter{}
			deps.Limiter = tt.limiter

			rec := post(deps, "/api/v2/processing-jobs/reels",
				`{"url":"https://www.instagram.com/reel/C8abc123/"}`,
				map[string]string{"Authorization": "Bearer good.token", "Idempotency-Key": validKey})

			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503: a provider call must never be unmetered", rec.Code)
			}
			if code := decodeError(t, rec).Error.Code; code != "processing_unavailable" {
				t.Errorf("code = %q", code)
			}
		})
	}
}

func TestARefusedSubmissionCarriesRetryAfter(t *testing.T) {
	deps := testDeps(&fakePinger{})
	deps.Enqueue = &fakeSubmitter{}
	deps.Limiter = denyingLimiter{}

	rec := post(deps, "/api/v2/processing-jobs/reels",
		`{"url":"https://www.instagram.com/reel/C8abc123/"}`,
		map[string]string{"Authorization": "Bearer good.token", "Idempotency-Key": validKey})

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After for a client to obey")
	}
	if code := decodeError(t, rec).Error.Code; code != "rate_limited" {
		t.Errorf("code = %q", code)
	}
}

func TestTheMeterIsNotConsultedForReads(t *testing.T) {
	deps := testDeps(&fakePinger{})
	deps.Reels = &fakeReels{}
	deps.Limiter = unavailableLimiter{}

	rec := serve(deps, "GET", "/api/v2/reels", "Bearer good.token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: reads stay available when the meter is down", rec.Code)
	}
}

func TestNativeShareAuthenticatesByTokenAndEnqueues(t *testing.T) {
	submitter := &fakeSubmitter{}
	deps := submitDeps(submitter)

	rec := post(deps, "/api/v2/native-shares/reels",
		`{"raw_payload_text":"look at this https://www.instagram.com/reel/C8abc123/"}`,
		map[string]string{"X-Share-Token": "device.token", "Idempotency-Key": validKey})

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", rec.Code, rec.Body.String())
	}
	if submitter.lastRequest.UserID != testUserID {
		t.Errorf("enqueued as %q, want the token's user", submitter.lastRequest.UserID)
	}
	if submitter.lastRequest.RawPayloadText == "" {
		t.Error("the shared text did not reach the use case")
	}
	if submitter.lastRequest.Endpoint != "native-shares/reels" {
		t.Errorf("endpoint = %q", submitter.lastRequest.Endpoint)
	}
}

func TestNativeShareRefusesEverySortOfBadToken(t *testing.T) {
	tests := []struct {
		name   string
		header map[string]string
		code   string
	}{
		{"no token", map[string]string{"Idempotency-Key": validKey}, "share_token_required"},
		{"unknown token", map[string]string{"X-Share-Token": "nope", "Idempotency-Key": validKey}, "invalid_share_token"},
		{"a session is not a share token", map[string]string{"Authorization": "Bearer good.token", "Idempotency-Key": validKey}, "share_token_required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := post(submitDeps(&fakeSubmitter{}), "/api/v2/native-shares/reels",
				`{"raw_payload_text":"https://www.instagram.com/reel/C8abc123/"}`, tt.header)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (%s)", rec.Code, rec.Body.String())
			}
			if code := decodeError(t, rec).Error.Code; code != tt.code {
				t.Errorf("code = %q, want %q", code, tt.code)
			}
		})
	}
}

func TestShareTokensAreMintedAndRevoked(t *testing.T) {
	tokens := &fakeShareTokens{}
	deps := testDeps(&fakePinger{})
	deps.ShareTokens = tokens

	rec := post(deps, "/api/v2/share-tokens", `{}`,
		map[string]string{"Authorization": "Bearer good.token"})
	if rec.Code != http.StatusOK {
		t.Fatalf("mint status = %d (%s)", rec.Code, rec.Body.String())
	}
	var minted map[string]any
	json.Unmarshal(rec.Body.Bytes(), &minted)
	if minted["token"] == "" || minted["expires_at"] == nil {
		t.Fatalf("mint body = %v", minted)
	}
	if tokens.userID != testUserID {
		t.Errorf("minted for %q, want the token subject", tokens.userID)
	}

	req := httptest.NewRequest("DELETE", "/api/v2/share-tokens", nil)
	req.Header.Set("Authorization", "Bearer good.token")
	revokeRec := httptest.NewRecorder()
	New(deps).Routes().ServeHTTP(revokeRec, req)
	if revokeRec.Code != http.StatusOK {
		t.Fatalf("revoke status = %d", revokeRec.Code)
	}
	if tokens.revoked != 1 {
		t.Errorf("revoked %d times", tokens.revoked)
	}
}

func TestShareResolveAnswersUnsupportedWithoutFailing(t *testing.T) {
	deps := testDeps(&fakePinger{})

	rec := post(deps, "/api/v2/share/resolve", `{"raw_payload_text":"nothing here at all"}`,
		map[string]string{"Authorization": "Bearer good.token"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: unsupported is an answer, not an error", rec.Code)
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["supported"] != false {
		t.Fatalf("body = %v", body)
	}
}

// unavailableLimiter stands in for Redis being down.
type unavailableLimiter struct{}

func (unavailableLimiter) Allow(context.Context, ratelimit.Policy, string) (ratelimit.Decision, error) {
	return ratelimit.Decision{}, ratelimit.ErrUnavailable
}

// denyingLimiter stands in for a caller over their window.
type denyingLimiter struct{}

func (denyingLimiter) Allow(context.Context, ratelimit.Policy, string) (ratelimit.Decision, error) {
	return ratelimit.Decision{Allowed: false, RetryAfter: 42 * 1e9}, nil
}

var _ = reels.ErrNotFound

// recordingLimiter keeps the subject each policy was counted against, which is
// the only way to see which bucket a request landed in.
type recordingLimiter struct {
	subjects map[string]string
}

func (l *recordingLimiter) Allow(_ context.Context, policy ratelimit.Policy, subject string) (ratelimit.Decision, error) {
	if l.subjects == nil {
		l.subjects = map[string]string{}
	}
	l.subjects[policy.Name] = subject
	return ratelimit.Decision{Allowed: true, Remaining: 1}, nil
}

// submitFrom posts one submission from a given socket, optionally claiming a
// bucket, and reports the subject the per-IP policy counted against.
func submitFrom(deps Deps, limiter *recordingLimiter, remoteAddr, bucketHeader string) string {
	req := httptest.NewRequest("POST", "/api/v2/processing-jobs/reels",
		strings.NewReader(`{"url":"https://www.instagram.com/reel/C8abc123/"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer good.token")
	req.Header.Set("Idempotency-Key", validKey)
	if bucketHeader != "" {
		req.Header.Set(ratelimit.IPBucketHeader, bucketHeader)
	}
	req.RemoteAddr = remoteAddr

	New(deps).Routes().ServeHTTP(httptest.NewRecorder(), req)
	return limiter.subjects[ratelimit.SubmissionIP.Name]
}

func bucketDeps(secret string, limiter *recordingLimiter) Deps {
	deps := submitDeps(&fakeSubmitter{})
	deps.Limiter = limiter
	deps.IPBucketSecret = secret
	return deps
}

// The whole point of the signed bucket: two visitors behind one web boundary
// arrive on one socket and must still be counted apart.
func TestASignedBucketSeparatesTwoVisitorsOnOneSocket(t *testing.T) {
	const secret = "shared-with-the-web-boundary"

	subjects := []string{}
	for _, bucket := range []string{
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	} {
		limiter := &recordingLimiter{}
		subjects = append(subjects, submitFrom(bucketDeps(secret, limiter), limiter,
			"10.0.0.1:44444", signedBucketHeader(secret, bucket, testNow)))
	}

	if subjects[0] == subjects[1] {
		t.Fatalf("both visitors counted against %q", subjects[0])
	}
	if subjects[0] == "10.0.0.1" || subjects[1] == "10.0.0.1" {
		t.Fatalf("fell back to the socket peer despite a valid bucket: %v", subjects)
	}
}

// A header anyone could have written must buy nothing.
func TestAForgedBucketFallsBackToTheSocketPeer(t *testing.T) {
	const secret = "shared-with-the-web-boundary"

	cases := map[string]string{
		"signed with another secret": signedBucketHeader("not-the-secret", "cccccccccccccccccccccccccccccccc", testNow),
		"invented":                   "v1.1780000000.ffffffffffffffffffffffffffffffff.00",
		"absent":                     "",
	}
	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			limiter := &recordingLimiter{}
			got := submitFrom(bucketDeps(secret, limiter), limiter, "10.0.0.1:44444", header)
			if got != "10.0.0.1" {
				t.Fatalf("subject = %q, want the socket peer", got)
			}
		})
	}
}

// Without a secret configured this service cannot check anything, so even a
// genuinely signed bucket must not be believed.
func TestAnUnconfiguredServiceBelievesNoBucket(t *testing.T) {
	limiter := &recordingLimiter{}
	got := submitFrom(bucketDeps("", limiter), limiter, "10.0.0.1:44444",
		signedBucketHeader("shared-with-the-web-boundary", "dddddddddddddddddddddddddddddddd", testNow))

	if got != "10.0.0.1" {
		t.Fatalf("subject = %q, want the socket peer", got)
	}
}
