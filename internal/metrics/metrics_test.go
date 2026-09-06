package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testAdminKey = "test-admin-key"

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("X-Admin-Key", testAdminKey)
	rec := httptest.NewRecorder()
	m.GuardedHandler(testAdminKey).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape status = %d", rec.Code)
	}
	return rec.Body.String()
}

func TestAnUnroutedRequestGetsAConstantLabel(t *testing.T) {
	m := New()
	m.ObserveRequest("", "GET", 404, time.Millisecond)

	if body := scrape(t, m); !strings.Contains(body, `route="unmatched"`) {
		t.Fatalf("missing the unmatched label in:\n%s", body)
	}
}

func TestStagesAreLabelledByFailureClass(t *testing.T) {
	m := New()
	m.ObserveStage("transcribe", "instagram", "provider_exhausted", 2*time.Second)

	body := scrape(t, m)
	if !strings.Contains(body, `stage="transcribe"`) || !strings.Contains(body, `outcome="provider_exhausted"`) {
		t.Fatalf("stage labels missing from:\n%s", body)
	}
}

func TestTheScrapeNeedsTheAdminKey(t *testing.T) {
	rec := httptest.NewRecorder()
	New().GuardedHandler(testAdminKey).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: queue depths are operational detail", rec.Code)
	}
}

func TestANilRegistryIsSafe(t *testing.T) {
	var m *Metrics
	// Metrics are optional everywhere, so every entry point takes a nil.
	m.ObserveRequest("/api/v2/reels", "GET", 200, time.Millisecond)
	m.ObserveStage("persist", "instagram", "ok", time.Millisecond)
}
