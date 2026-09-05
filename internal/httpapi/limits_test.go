package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/ratelimit"
)

type fakeLimiter struct {
	err      error
	allow    bool
	subjects []string
	policies []string
}

func (f *fakeLimiter) Allow(_ context.Context, policy ratelimit.Policy, subject string) (ratelimit.Decision, error) {
	f.policies = append(f.policies, policy.Name)
	f.subjects = append(f.subjects, subject)
	if f.err != nil {
		return ratelimit.Decision{}, f.err
	}
	if !f.allow {
		return ratelimit.Decision{Allowed: false, RetryAfter: 7 * time.Second}, nil
	}
	return ratelimit.Decision{Allowed: true, Remaining: 3}, nil
}

func TestClientIP(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}

	tests := []struct {
		name       string
		remoteAddr string
		forwarded  string
		trusted    []netip.Prefix
		want       string
	}{
		{
			name:       "no proxies configured, the socket wins",
			remoteAddr: "203.0.113.9:5555", forwarded: "1.2.3.4", want: "203.0.113.9",
		},
		{
			name:       "an untrusted client cannot claim another address",
			remoteAddr: "203.0.113.9:5555", forwarded: "1.2.3.4", trusted: trusted, want: "203.0.113.9",
		},
		{
			name:       "a trusted proxy is believed",
			remoteAddr: "10.1.2.3:5555", forwarded: "1.2.3.4", trusted: trusted, want: "1.2.3.4",
		},
		{
			name:       "the last untrusted hop wins in a chain",
			remoteAddr: "10.1.2.3:5555", forwarded: "9.9.9.9, 1.2.3.4, 10.0.0.7", trusted: trusted, want: "1.2.3.4",
		},
		{
			name:       "a trusted proxy with no header falls back to the socket",
			remoteAddr: "10.1.2.3:5555", trusted: trusted, want: "10.1.2.3",
		},
		{
			name:       "garbage in the header is ignored",
			remoteAddr: "10.1.2.3:5555", forwarded: "not-an-ip", trusted: trusted, want: "10.1.2.3",
		},
		{
			name:       "ipv6 sockets parse",
			remoteAddr: "[2606:4700::1111]:443", want: "2606:4700::1111",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/v1/reels", nil)
			req.RemoteAddr = tt.remoteAddr
			if tt.forwarded != "" {
				req.Header.Set("X-Forwarded-For", tt.forwarded)
			}
			if got := clientIP(req, tt.trusted); got != tt.want {
				t.Errorf("clientIP = %q, want %q", got, tt.want)
			}
		})
	}
}

func shareRequest(deps Deps) *httptest.ResponseRecorder {
	return postShare(deps, `{"raw_payload_text":"https://www.instagram.com/reel/C8abc123/"}`, "Bearer good.token")
}

func TestRateLimitedRequestIs429(t *testing.T) {
	limiter := &fakeLimiter{allow: false}
	deps := testDeps(&fakePinger{})
	deps.Limiter = limiter

	rec := shareRequest(deps)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (%s)", rec.Code, rec.Body.String())
	}
	if retry := rec.Header().Get("Retry-After"); retry != "7" {
		t.Errorf("Retry-After = %q, want 7", retry)
	}

	body := decodeError(t, rec)
	if body.ErrorCode != "rate_limited" {
		t.Errorf("error_code = %q, want rate_limited", body.ErrorCode)
	}
	if !body.Retryable {
		t.Error("retryable = false on a rate limit")
	}
}

func TestRateLimitChargesTheUserAndTheAddress(t *testing.T) {
	limiter := &fakeLimiter{allow: true}
	deps := testDeps(&fakePinger{})
	deps.Limiter = limiter

	if rec := shareRequest(deps); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	if len(limiter.subjects) != 2 {
		t.Fatalf("charged %v, want a user bucket and an address bucket", limiter.subjects)
	}
	if !strings.HasPrefix(limiter.subjects[0], "user:") || !strings.HasPrefix(limiter.subjects[1], "ip:") {
		t.Errorf("subjects = %v", limiter.subjects)
	}
	if !strings.Contains(limiter.subjects[0], testUserID) {
		t.Errorf("the user bucket is not keyed by the token subject: %q", limiter.subjects[0])
	}
}

func TestReadOnlyRouteFailsOpenWhenRedisIsDown(t *testing.T) {
	limiter := &fakeLimiter{err: errors.New("redis is unreachable")}
	deps := testDeps(&fakePinger{})
	deps.Limiter = limiter

	// Share resolution calls no provider and writes nothing, so an outage must
	// not stop people sharing.
	if rec := shareRequest(deps); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

func TestPaidRouteFailsClosedWhenRedisIsDown(t *testing.T) {
	limiter := &fakeLimiter{err: errors.New("redis is unreachable")}
	deps := testDeps(&fakePinger{})
	deps.Limiter = limiter

	server := New(deps)
	// The enqueue policy arrives with its endpoint in Task 9; this proves the
	// fail-closed path the endpoint will use.
	handler := server.authenticated(server.rateLimited(routeLimit{
		User: &ratelimit.Enqueue,
		IP:   &ratelimit.EnqueueIP,
	}, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("the handler ran without a rate limit decision")
	}))

	req := httptest.NewRequest("POST", "/api/v1/processing-jobs/reels", nil)
	req.Header.Set("Authorization", "Bearer good.token")
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if code := decodeError(t, rec).ErrorCode; code != "rate_limit_unavailable" {
		t.Errorf("error_code = %q, want rate_limit_unavailable", code)
	}
}

func TestHealthIsNeverRateLimited(t *testing.T) {
	limiter := &fakeLimiter{allow: false}
	deps := testDeps(&fakePinger{})
	deps.Limiter = limiter

	for _, path := range []string{"/api/v1/health", "/api/v1/health/live", "/api/v1/health/ready"} {
		rec := serve(deps, "GET", path, "")
		if rec.Code == http.StatusTooManyRequests {
			t.Errorf("%s was rate limited", path)
		}
	}
	if len(limiter.subjects) != 0 {
		t.Errorf("health spent %v", limiter.subjects)
	}
}

func TestLibraryReadsAreNotLimitedYet(t *testing.T) {
	limiter := &fakeLimiter{allow: false}
	deps := testDeps(&fakePinger{})
	deps.Limiter = limiter

	if rec := serve(deps, "GET", "/api/v1/reels", "Bearer good.token"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: read-only library calls carry no limit", rec.Code)
	}
}
