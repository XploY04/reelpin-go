//go:build integration

package postgres

import (
	"context"
	"testing"

	"github.com/XploY04/reelpin-go/internal/spend"
)

func TestTheLedgerSumsOnlyTheCurrentMonth(t *testing.T) {
	pool := rawPool(t)
	ledger := NewSpend(pool)
	ctx := context.Background()

	for _, entry := range []spend.Entry{
		{
			Usage: spend.Usage{
				Provider: "gemini", Model: "gemini-2.0-flash-lite", Operation: "extract",
				Calls: 1, InputTokens: 1_000_000, OutputTokens: 2_000, Measured: true,
			},
			CostMicros: 75_000, Priced: true,
		},
		{
			Usage:      spend.Usage{Provider: "apify", Model: "instagram", Operation: "actor_run", Calls: 1},
			CostMicros: 4_000, Priced: true,
		},
	} {
		if err := ledger.Insert(ctx, entry); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	// A row from a previous month must not count toward this month's limit.
	if _, err := pool.Exec(ctx, `
		INSERT INTO reelpin.provider_usage
			(occurred_at, provider, model, operation, calls, measured, cost_micros, priced)
		VALUES (now() - interval '2 months', 'gemini', 'old', 'extract', 1, true, 999_000_000, true)`,
	); err != nil {
		t.Fatalf("seeding an older month: %v", err)
	}

	total, err := ledger.MonthToDateMicros(ctx)
	if err != nil {
		t.Fatalf("MonthToDateMicros: %v", err)
	}
	if total != 79_000 {
		t.Errorf("month to date = %d micros, want 79000", total)
	}
}

func TestAnUnpricedCallIsStoredRatherThanDropped(t *testing.T) {
	pool := rawPool(t)
	ledger := NewSpend(pool)
	ctx := context.Background()

	if err := ledger.Insert(ctx, spend.Entry{
		Usage: spend.Usage{Provider: "brand-new", Model: "x", Operation: "y", Calls: 3},
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	var calls int
	var priced bool
	if err := pool.QueryRow(ctx, `
		SELECT calls, priced FROM reelpin.provider_usage WHERE provider = 'brand-new'`,
	).Scan(&calls, &priced); err != nil {
		t.Fatalf("reading it back: %v", err)
	}
	if calls != 3 || priced {
		t.Errorf("calls = %d, priced = %v; unpriced spending must still be on record", calls, priced)
	}
}
