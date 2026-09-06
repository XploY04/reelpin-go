package spend

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"net/http/httptest"
	"strings"

	"github.com/XploY04/reelpin-go/internal/metrics"
)

func testPrices(t *testing.T) Prices {
	t.Helper()
	prices, err := ParsePrices(
		"gemini:gemini-2.0-flash-lite:input_mtok=0.075," +
			"gemini:gemini-2.0-flash-lite:output_mtok=0.30," +
			"gemini:gemini-embedding-2:call=0.0001," +
			"apify:*:call=0.004")
	if err != nil {
		t.Fatalf("ParsePrices: %v", err)
	}
	return prices
}

func TestParseUSDIsExactToTheMicro(t *testing.T) {
	// 0.075 has no exact binary form. A limit that lands a micro either side of
	// what was approved is a limit nobody approved.
	tests := []struct {
		raw  string
		want Micros
	}{
		{"0", 0},
		{"20", 20_000_000},
		{"$12.50", 12_500_000},
		{"0.075", 75_000},
		{"0.000001", 1},
		{".5", 500_000},
	}
	for _, tt := range tests {
		got, err := ParseUSD(tt.raw)
		if err != nil {
			t.Errorf("ParseUSD(%q): %v", tt.raw, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseUSD(%q) = %d micros, want %d", tt.raw, got, tt.want)
		}
	}

	for _, raw := range []string{"", "-1", "0.0000001", "twenty", "1.2.3"} {
		if _, err := ParseUSD(raw); err == nil {
			t.Errorf("ParseUSD(%q) was accepted", raw)
		}
	}
}

func TestCostUsesMeasuredTokensWhenTheProviderReportedThem(t *testing.T) {
	prices := testPrices(t)

	cost, priced := prices.Cost(Usage{
		Provider: "gemini", Model: "gemini-2.0-flash-lite", Operation: "extract",
		Calls: 1, InputTokens: 1_000_000, OutputTokens: 100_000, Measured: true,
	})
	if !priced {
		t.Fatal("a configured model came back unpriced")
	}
	// 1M input at $0.075 plus 100k output at $0.30 per million.
	if want := Micros(75_000 + 30_000); cost != want {
		t.Errorf("cost = %d micros, want %d", cost, want)
	}
}

func TestCostFallsBackToACallPriceAndReportsWhatItCannotPrice(t *testing.T) {
	prices := testPrices(t)

	// The provider reported nothing, so only the call itself can be priced.
	cost, priced := prices.Cost(Usage{
		Provider: "apify", Model: "instagram", Operation: "actor_run", Calls: 2,
	})
	if !priced || cost != 8_000 {
		t.Errorf("cost = %d micros, priced = %v, want 8000 from the provider wildcard", cost, priced)
	}

	// A measured call with no token price still falls back to the call price.
	cost, priced = prices.Cost(Usage{
		Provider: "gemini", Model: "gemini-embedding-2", Operation: "embed",
		Calls: 1, InputTokens: 500, Measured: true,
	})
	if !priced || cost != 100 {
		t.Errorf("cost = %d micros, priced = %v, want the per-call price", cost, priced)
	}

	if cost, priced := prices.Cost(Usage{Provider: "someone-else", Model: "m", Calls: 1}); priced || cost != 0 {
		t.Errorf("an unconfigured provider was priced at %d micros", cost)
	}
}

// recordingStore captures what the ledger stored and can fail on demand.
type recordingStore struct {
	entries []Entry
	total   Micros
	err     error
}

func (s *recordingStore) Insert(_ context.Context, entry Entry) error {
	if s.err != nil {
		return s.err
	}
	s.entries = append(s.entries, entry)
	s.total += entry.CostMicros
	return nil
}

func (s *recordingStore) MonthToDateMicros(context.Context) (Micros, error) {
	return s.total, nil
}

func quietLedger(store Store, prices Prices, meters *metrics.Metrics) *Ledger {
	return NewLedger(store, prices, meters, slog.New(slog.NewJSONHandler(io.Discard, nil)))
}

func TestLedgerStoresThePriceItComputed(t *testing.T) {
	store := &recordingStore{}
	quietLedger(store, testPrices(t), metrics.New()).Record(context.Background(), Usage{
		Provider: "gemini", Model: "gemini-2.0-flash-lite", Operation: "categorize",
		Calls: 1, InputTokens: 2_000_000, Measured: true,
	})

	if len(store.entries) != 1 {
		t.Fatalf("stored %d entries", len(store.entries))
	}
	entry := store.entries[0]
	if !entry.Priced || entry.CostMicros != 150_000 {
		t.Errorf("entry = %+v, want $0.15 priced", entry)
	}
}

func TestLedgerRecordsAnUnpricedCallRatherThanDroppingIt(t *testing.T) {
	store := &recordingStore{}
	quietLedger(store, testPrices(t), nil).Record(context.Background(), Usage{
		Provider: "brand-new", Model: "x", Operation: "y", Calls: 1,
	})

	if len(store.entries) != 1 {
		t.Fatalf("stored %d entries; unpriced spending must still be on record", len(store.entries))
	}
	if store.entries[0].Priced {
		t.Error("an unconfigured provider was marked priced")
	}
}

func TestLedgerSurvivesACancelledCaller(t *testing.T) {
	// The caller is usually a pipeline stage that has just been cancelled. The
	// money is already spent; the row still has to land.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	store := &recordingStore{}
	quietLedger(store, testPrices(t), nil).Record(ctx, Usage{
		Provider: "apify", Model: "instagram", Operation: "actor_run", Calls: 1,
	})
	if len(store.entries) != 1 {
		t.Fatalf("stored %d entries after the caller was cancelled", len(store.entries))
	}
}

func TestAFailedInsertIsCountedAsUndercounting(t *testing.T) {
	meters := metrics.New()
	store := &recordingStore{err: errors.New("no connection")}
	quietLedger(store, testPrices(t), meters).Record(context.Background(), Usage{
		Provider: "gemini", Model: "gemini-2.0-flash-lite", Operation: "extract",
		Calls: 1, InputTokens: 100, Measured: true,
	})

	if !strings.Contains(exposition(t, meters), `accounting="unrecorded"`) {
		t.Error("a lost ledger row is invisible; the gate would undercount in silence")
	}
}

// exposition scrapes the registry the way Prometheus does, so the assertion is
// on what an alert would actually see.
func exposition(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	request := httptest.NewRequest("GET", "/metrics", nil)
	request.Header.Set("X-Admin-Key", "test-key")
	recorder := httptest.NewRecorder()
	m.GuardedHandler("test-key").ServeHTTP(recorder, request)
	return recorder.Body.String()
}
