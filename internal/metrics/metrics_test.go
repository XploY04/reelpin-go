package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
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

func TestSearchCountsEmptyResultsSeparately(t *testing.T) {
	m := New()
	m.ObserveSearch("dense+sparse", 3, 50*time.Millisecond)
	m.ObserveSearch("sparse", 0, 10*time.Millisecond)

	body := scrape(t, m)
	for _, want := range []string{`outcome="results"`, `outcome="empty"`, `mode="dense+sparse"`} {
		if !strings.Contains(body, want) {
			t.Errorf("%s missing from:\n%s", want, body)
		}
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

func TestANilRegistryIsSafe(t *testing.T) {
	var m *Metrics
	// Metrics are optional everywhere, so every entry point takes a nil.
	m.ObserveRequest("/api/v1/reels", "GET", 200, time.Millisecond)
	m.ObserveSearch("dense", 1, time.Millisecond)
	m.ObserveStage("save", "instagram", "ok", time.Millisecond)
}
