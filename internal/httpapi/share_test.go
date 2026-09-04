package httpapi

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/sourceidentity"
)

func postShare(deps Deps, body string, authorization string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/share/resolve", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	New(deps).Routes().ServeHTTP(rec, req)
	return rec
}

func TestResolveShareRequiresAuth(t *testing.T) {
	rec := postShare(testDeps(&fakePinger{}), `{"raw_payload_text":"https://x.com/a/status/1"}`, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if code := decodeError(t, rec).ErrorCode; code != "authentication_required" {
		t.Errorf("error_code = %q, want authentication_required", code)
	}
}

func TestResolveShareSuccess(t *testing.T) {
	rec := postShare(testDeps(&fakePinger{}),
		`{"raw_payload_text":"look at this https://www.instagram.com/reel/C8abc123/?igsh=x"}`,
		"Bearer good.token")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	var body sourceidentity.ShareResolveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not a share response: %v", err)
	}
	if !body.Supported {
		t.Fatalf("supported = false: %s", rec.Body.String())
	}
	if body.Provider == nil || *body.Provider != "instagram" {
		t.Errorf("provider = %v, want instagram", body.Provider)
	}
	if body.NormalizedURL == nil || *body.NormalizedURL != "https://www.instagram.com/reel/C8abc123/" {
		t.Errorf("normalized url = %v", body.NormalizedURL)
	}
}

func TestResolveShareUnsupportedPayloadIs200(t *testing.T) {
	// The app renders the message; an unsupported link is not an API error.
	rec := postShare(testDeps(&fakePinger{}), `{"raw_payload_text":"no link at all"}`, "Bearer good.token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body sourceidentity.ShareResolveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not a share response: %v", err)
	}
	if body.Supported || body.ErrorMessage == nil {
		t.Errorf("body = %+v, want an unsupported response with a message", body)
	}
}

func TestResolveShareRejectsBadBodies(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "not json", body: "{"},
		{name: "empty", body: ""},
		{name: "unknown field", body: `{"raw_payload_text":"x","user_id":"someone-else"}`},
		{name: "too large", body: `{"raw_payload_text":"` + strings.Repeat("a", maxShareBodyBytes) + `"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := postShare(testDeps(&fakePinger{}), tt.body, "Bearer good.token")
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (%s)", rec.Code, rec.Body.String())
			}
			if code := decodeError(t, rec).ErrorCode; code != "validation_error" {
				t.Errorf("error_code = %q, want validation_error", code)
			}
		})
	}
}

func TestResolveShareWrongMethod(t *testing.T) {
	rec := serve(testDeps(&fakePinger{}), "GET", "/api/v1/share/resolve", "Bearer good.token")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
		t.Errorf("Allow = %q, want POST", allow)
	}
}

func TestResolveShareDoesNotLogTheSharedContent(t *testing.T) {
	var logs bytes.Buffer
	deps := testDeps(&fakePinger{})
	deps.Logger = slog.New(slog.NewJSONHandler(&logs, nil))

	payload := "private note https://www.instagram.com/reel/SECRETCODE/"
	rec := postShare(deps, `{"raw_payload_text":"`+payload+`"}`, "Bearer good.token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	written := logs.String()
	for _, leak := range []string{"SECRETCODE", "private note", "instagram.com/reel"} {
		if strings.Contains(written, leak) {
			t.Errorf("the log leaks %q: %s", leak, written)
		}
	}
	if !strings.Contains(written, `"platform":"instagram"`) {
		t.Errorf("the log should still carry the platform: %s", written)
	}
	if !strings.Contains(written, `"url_hash"`) {
		t.Errorf("the log should carry a url hash: %s", written)
	}
}
