package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer(p DatabasePinger) *Server {
	return New(testDeps(p))
}

func do(t *testing.T, s *Server, req *http.Request) (*httptest.ResponseRecorder, HealthResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)

	var body HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding body %q: %v", rec.Body.String(), err)
	}
	return rec, body
}

func TestHealthEndpoints(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		pingErr    error
		wantStatus int
		wantState  string
		wantReady  bool
		wantChecks []string
		wantCalls  int
	}{
		{
			name:       "liveness never touches the database",
			path:       "/api/v1/health/live",
			pingErr:    errors.New("db down"),
			wantStatus: http.StatusOK,
			wantState:  "ok",
			wantReady:  true,
			wantChecks: []string{"api"},
			wantCalls:  0,
		},
		{
			name:       "readiness success",
			path:       "/api/v1/health/ready",
			wantStatus: http.StatusOK,
			wantState:  "ok",
			wantReady:  true,
			wantChecks: []string{"api", "supabase"},
			wantCalls:  1,
		},
		{
			name:       "readiness failure",
			path:       "/api/v1/health/ready",
			pingErr:    errors.New("connection refused"),
			wantStatus: http.StatusServiceUnavailable,
			wantState:  "degraded",
			wantReady:  false,
			wantChecks: []string{"api", "supabase"},
			wantCalls:  1,
		},
		{
			name:       "compatibility health stays 200 when degraded",
			path:       "/api/v1/health",
			pingErr:    errors.New("connection refused"),
			wantStatus: http.StatusOK,
			wantState:  "degraded",
			wantReady:  false,
			wantChecks: []string{"api", "supabase"},
			wantCalls:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pinger := &fakePinger{err: tt.pingErr}
			rec, body := do(t, newTestServer(pinger), httptest.NewRequest("GET", tt.path, nil))

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			if body.Status != tt.wantState {
				t.Errorf("status = %q, want %q", body.Status, tt.wantState)
			}
			if body.Ready != tt.wantReady {
				t.Errorf("ready = %v, want %v", body.Ready, tt.wantReady)
			}
			if body.Service != "ReelMind API" {
				t.Errorf("service = %q, want ReelMind API", body.Service)
			}
			if body.Version != "test" {
				t.Errorf("version = %q, want test", body.Version)
			}
			if len(body.Checks) != len(tt.wantChecks) {
				t.Errorf("checks = %v, want keys %v", body.Checks, tt.wantChecks)
			}
			for _, key := range tt.wantChecks {
				if _, ok := body.Checks[key]; !ok {
					t.Errorf("missing check %q", key)
				}
			}
			if pinger.calls != tt.wantCalls {
				t.Errorf("ping calls = %d, want %d", pinger.calls, tt.wantCalls)
			}

			assertUTC(t, body.CheckedAt)
			for _, check := range body.Checks {
				if check.CheckedAt != "" {
					assertUTC(t, check.CheckedAt)
				}
			}
		})
	}
}

func assertUTC(t *testing.T, value string) {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("timestamp %q is not RFC3339: %v", value, err)
	}
	if _, offset := parsed.Zone(); offset != 0 {
		t.Errorf("timestamp %q is not UTC", value)
	}
}

func TestReadinessFailureHidesDatabaseDetails(t *testing.T) {
	pinger := &fakePinger{err: errors.New("failed to connect to postgres://reelpin:secret@localhost:5432/reelpin")}
	rec, _ := do(t, newTestServer(pinger), httptest.NewRequest("GET", "/api/v1/health/ready", nil))

	for _, leak := range []string{"secret", "localhost", "postgres://"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("response leaks %q: %s", leak, rec.Body.String())
		}
	}
}

func TestRequestID(t *testing.T) {
	tests := []struct {
		name     string
		incoming string
		want     string
	}{
		{name: "valid id is preserved", incoming: "abc-123_x.y", want: "abc-123_x.y"},
		{name: "invalid characters are replaced", incoming: "bad id!"},
		{name: "too long is replaced", incoming: strings.Repeat("a", 129)},
		{name: "missing id is generated"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/v1/health/live", nil)
			if tt.incoming != "" {
				req.Header.Set("X-Request-ID", tt.incoming)
			}
			rec := httptest.NewRecorder()
			newTestServer(&fakePinger{}).Routes().ServeHTTP(rec, req)

			got := rec.Header().Get("X-Request-ID")
			if tt.want != "" {
				if got != tt.want {
					t.Fatalf("X-Request-ID = %q, want %q", got, tt.want)
				}
				return
			}
			if got == "" || got == tt.incoming {
				t.Fatalf("X-Request-ID = %q, want a generated id", got)
			}
			if !validRequestID(got) {
				t.Fatalf("generated id %q is not valid", got)
			}
		})
	}
}

func TestRecoverPanic(t *testing.T) {
	s := newTestServer(&fakePinger{})
	handler := s.recoverPanic(jsonContentType(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/health/live", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding body: %v", err)
	}
	want := map[string]any{
		"success":    false,
		"error_code": "internal_error",
		"message":    "The server could not finish this request.",
		"detail":     "Unhandled server error",
		"retryable":  true,
	}
	for key, value := range want {
		if body[key] != value {
			t.Errorf("%s = %v, want %v", key, body[key], value)
		}
	}
}

func TestFrameworkErrorsAreJSON(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantCode   string
	}{
		{name: "unknown route", method: "GET", path: "/missing", wantStatus: http.StatusNotFound, wantCode: "not_found"},
		{name: "unknown api route", method: "GET", path: "/api/v1/collections", wantStatus: http.StatusNotFound, wantCode: "not_found"},
		{name: "wrong method", method: "POST", path: "/api/v1/health/live", wantStatus: http.StatusMethodNotAllowed, wantCode: "method_not_allowed"},
		{name: "wrong method on a reel route", method: "DELETE", path: "/api/v1/reels/abc", wantStatus: http.StatusMethodNotAllowed, wantCode: "method_not_allowed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			newTestServer(&fakePinger{}).Routes().ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}

			var body errorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body %q is not JSON: %v", rec.Body.String(), err)
			}
			if body.ErrorCode != tt.wantCode {
				t.Errorf("error_code = %q, want %q", body.ErrorCode, tt.wantCode)
			}
			if body.Success {
				t.Error("success = true, want false")
			}
		})
	}
}

func TestWriteJSONEncodingFailure(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, map[string]any{"bad": make(chan int)})

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}

	var body errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q is not JSON: %v", rec.Body.String(), err)
	}
	if body.ErrorCode != "internal_error" {
		t.Errorf("error_code = %q, want internal_error", body.ErrorCode)
	}
}

func TestHealthUsesOneTimestamp(t *testing.T) {
	_, body := do(t, newTestServer(&fakePinger{}), httptest.NewRequest("GET", "/api/v1/health/ready", nil))

	for key, check := range body.Checks {
		if check.CheckedAt != body.CheckedAt {
			t.Errorf("checks[%q].checked_at = %q, want %q", key, check.CheckedAt, body.CheckedAt)
		}
	}
}

func TestDatabaseCheckStatusOnFailure(t *testing.T) {
	_, body := do(t, newTestServer(&fakePinger{err: errors.New("down")}), httptest.NewRequest("GET", "/api/v1/health/ready", nil))

	if got := body.Checks["supabase"].Status; got != "degraded" {
		t.Errorf("supabase status = %q, want degraded", got)
	}
}
