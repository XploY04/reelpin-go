package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/ratelimit"
)

func webDeps(fake *fakeWebsite) Deps {
	deps := testDeps(&fakePinger{})
	deps.Website = fake
	deps.Limiter = allowAllLimiter{}
	return deps
}

func postFeedback(deps Deps, body string) *httptest.ResponseRecorder {
	return post(deps, "/api/v2/feedback", body, map[string]string{"Authorization": "Bearer good.token"})
}

func TestFeedbackIsStoredAgainstTheTokenUser(t *testing.T) {
	fake := &fakeWebsite{}
	rec := postFeedback(webDeps(fake),
		`{"topic":"Support","email":"me@example.com","message":"  The map is blank.  "}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if fake.lastUserID != testUserID {
		t.Errorf("stored for %q, want the token subject", fake.lastUserID)
	}
	if fake.lastFeedback.Topic != "Support" {
		t.Errorf("topic = %q", fake.lastFeedback.Topic)
	}
	if !strings.Contains(rec.Body.String(), "66666666-6666-4666-8666-666666666666") {
		t.Errorf("the response carries no id: %s", rec.Body.String())
	}
}

func TestFeedbackValidation(t *testing.T) {
	tests := []struct{ name, body, field string }{
		{"unknown topic", `{"topic":"Spam","message":"hi"}`, "topic"},
		{"missing topic", `{"message":"hi"}`, "topic"},
		{"blank message", `{"topic":"Feedback","message":"   "}`, "message"},
		{"message too long", `{"topic":"Feedback","message":"` + strings.Repeat("a", 2001) + `"}`, "message"},
		{"bad email", `{"topic":"Feedback","email":"nope","message":"hi"}`, "email"},
		{"unknown field", `{"topic":"Feedback","message":"hi","user_id":"x"}`, "body"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := postFeedback(webDeps(&fakeWebsite{}), tt.body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (%s)", rec.Code, rec.Body.String())
			}
			if field := decodeError(t, rec).Error.Details["field"]; field != tt.field {
				t.Errorf("field = %v, want %s", field, tt.field)
			}
		})
	}
}

func TestFeedbackWithoutAValidSessionIsAnonymous(t *testing.T) {
	for name, tt := range map[string]struct {
		auth    fakeAuth
		headers map[string]string
	}{
		"no token":       {fakeAuth{userID: testUserID}, nil},
		"rejected token": {fakeAuth{err: errors.New("token expired")}, map[string]string{"Authorization": "Bearer expired.token"}},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeWebsite{lastUserID: "unset"}
			deps := webDeps(fake)
			deps.Auth = tt.auth

			rec := post(deps, "/api/v2/feedback", `{"topic":"Feedback","message":"hi"}`, tt.headers)
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201 (%s)", rec.Code, rec.Body.String())
			}
			if fake.lastUserID != "" {
				t.Errorf("stored for %q, want no user", fake.lastUserID)
			}
		})
	}
}

func TestFeedbackIsRateLimitedPerIPOnly(t *testing.T) {
	limiter := &recordingLimiter{}
	deps := webDeps(&fakeWebsite{})
	deps.Limiter = limiter

	rec := postFeedback(deps, `{"topic":"Feedback","message":"hi"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := limiter.subjects[ratelimit.FeedbackIP.Name]; got != "192.0.2.1" {
		t.Errorf("the IP window counted %q, want the socket peer", got)
	}
	if len(limiter.subjects) != 1 {
		t.Errorf("windows consulted = %v, want only the IP window", limiter.subjects)
	}

	deps.Limiter = denyingLimiter{}
	rec = postFeedback(deps, `{"topic":"Feedback","message":"hi"}`)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("status = %d, Retry-After = %q", rec.Code, rec.Header().Get("Retry-After"))
	}
}

func TestFeedbackCountsTheSignedBucketAndIgnoresAForgedOne(t *testing.T) {
	const secret = "shared-with-the-web-boundary"
	const bucket = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	for name, tt := range map[string]struct{ header, want string }{
		"signed": {signedBucketHeader(secret, bucket, testNow), bucket},
		"forged": {signedBucketHeader("not-the-secret", bucket, testNow), "192.0.2.1"},
	} {
		t.Run(name, func(t *testing.T) {
			limiter := &recordingLimiter{}
			deps := webDeps(&fakeWebsite{})
			deps.Limiter = limiter
			deps.IPBucketSecret = secret

			rec := post(deps, "/api/v2/feedback", `{"topic":"Feedback","message":"hi"}`,
				map[string]string{ratelimit.IPBucketHeader: tt.header})
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
			}
			if got := limiter.subjects[ratelimit.FeedbackIP.Name]; got != tt.want {
				t.Errorf("subject = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWebWritesFailClosedWithoutALimiter(t *testing.T) {
	for _, limiter := range []RateLimiter{nil, unavailableLimiter{}} {
		deps := webDeps(&fakeWebsite{})
		deps.Limiter = limiter

		if rec := postFeedback(deps, `{"topic":"Feedback","message":"hi"}`); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("feedback status = %d, want 503", rec.Code)
		}
		if rec := postClick(deps, `{"target":"play"}`, nil); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("link click status = %d, want 503", rec.Code)
		}
	}
}

func postClick(deps Deps, body string, headers map[string]string) *httptest.ResponseRecorder {
	return post(deps, "/api/v2/link-clicks", body, headers)
}

func TestALinkClickNeedsNoSession(t *testing.T) {
	fake := &fakeWebsite{}
	rec := postClick(webDeps(fake),
		`{"target":"ios","referer":"https://reelpin.in/","user_agent":"Mozilla","country":"IN"}`, nil)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (%s)", rec.Code, rec.Body.String())
	}
	if fake.lastClick.Target != "ios" || fake.lastClick.Country != "IN" {
		t.Errorf("click = %+v", fake.lastClick)
	}
}

func TestALinkClickIsValidated(t *testing.T) {
	for name, body := range map[string]string{
		"unknown target": `{"target":"windows"}`,
		"missing target": `{}`,
		"unknown field":  `{"target":"play","user_id":"x"}`,
	} {
		t.Run(name, func(t *testing.T) {
			rec := postClick(webDeps(&fakeWebsite{}), body, nil)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// countingLimiter allows a fixed number of calls per subject, then refuses, so
// the public limit is exercised the way Redis would apply it.
type countingLimiter struct {
	allowed int
	seen    map[string]int
}

func (l *countingLimiter) Allow(_ context.Context, policy ratelimit.Policy, subject string) (ratelimit.Decision, error) {
	if l.seen == nil {
		l.seen = map[string]int{}
	}
	key := policy.Name + "/" + subject
	l.seen[key]++
	if l.seen[key] > l.allowed {
		return ratelimit.Decision{RetryAfter: 30 * time.Second}, nil
	}
	return ratelimit.Decision{Allowed: true}, nil
}

func TestTheLinkClickLimitIsPerIPAndIgnoresAForgedBucket(t *testing.T) {
	fake := &fakeWebsite{}
	deps := webDeps(fake)
	deps.Limiter = &countingLimiter{allowed: 2}
	deps.IPBucketSecret = "secret"

	send := func(remote string, headers map[string]string) int {
		req := httptest.NewRequest("POST", "/api/v2/link-clicks", strings.NewReader(`{"target":"play"}`))
		req.RemoteAddr = remote
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		rec := httptest.NewRecorder()
		routes(deps).ServeHTTP(rec, req)
		return rec.Code
	}

	forged := map[string]string{ratelimit.IPBucketHeader: "v1.1.00000000000000000000000000000000.deadbeef"}
	for i := 0; i < 2; i++ {
		if code := send("203.0.113.9:1000", forged); code != http.StatusNoContent {
			t.Fatalf("request %d: status = %d, want 204", i+1, code)
		}
	}
	if code := send("203.0.113.9:1001", forged); code != http.StatusTooManyRequests {
		t.Fatalf("third request from one IP: status = %d, want 429", code)
	}
	if code := send("198.51.100.4:1000", nil); code != http.StatusNoContent {
		t.Fatalf("another IP: status = %d, want 204", code)
	}
	if fake.clicks != 3 {
		t.Errorf("recorded %d clicks, want 3; a refused one must not be stored", fake.clicks)
	}
}
