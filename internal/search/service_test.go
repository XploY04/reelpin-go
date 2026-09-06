package search

import (
	"context"
	"testing"
	"time"
)

// recordingMeters captures what Search reported without pulling the Prometheus
// client into this package's tests.
type recordingMeters struct {
	calls []struct {
		mode  string
		total int
	}
}

func (r *recordingMeters) ObserveSearch(mode string, total int, _ time.Duration) {
	r.calls = append(r.calls, struct {
		mode  string
		total int
	}{mode, total})
}

// A one-character query returns before any arm runs. That return used to sit
// above the observation, so the outcome the relevance alert watches for could
// never be counted and the alert could never fire.
func TestAQueryTooShortToRunIsStillCounted(t *testing.T) {
	meters := &recordingMeters{}
	service := &Service{now: time.Now, Metrics: meters}

	response, err := service.Search(context.Background(), "user", "a", Filters{}, 10)
	if err != nil {
		t.Fatalf("searching: %v", err)
	}
	if response.Total != 0 {
		t.Fatalf("total = %d, want 0", response.Total)
	}
	if len(meters.calls) != 1 {
		t.Fatalf("observed %d searches, want 1", len(meters.calls))
	}
	if meters.calls[0].mode != "empty" || meters.calls[0].total != 0 {
		t.Fatalf("observed %+v, want mode empty and total 0", meters.calls[0])
	}
}
