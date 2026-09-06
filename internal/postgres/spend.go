package postgres

import (
	"context"
	"fmt"

	"github.com/XploY04/reelpin-go/internal/spend"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Spend is the provider-usage ledger: the worker appends to it and the API sums
// it to decide whether to accept more work.
type Spend struct {
	pool *pgxpool.Pool
}

func NewSpend(pool *pgxpool.Pool) *Spend { return &Spend{pool: pool} }

func (s *Spend) Insert(ctx context.Context, entry spend.Entry) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO reelpin.provider_usage
			(provider, model, operation, calls, input_tokens, output_tokens,
			 measured, cost_micros, priced)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		entry.Provider, entry.Model, entry.Operation, entry.Calls,
		entry.InputTokens, entry.OutputTokens, entry.Measured,
		int64(entry.CostMicros), entry.Priced)
	if err != nil {
		return fmt.Errorf("recording provider usage: %w", err)
	}
	return nil
}

// MonthToDateMicros sums the current calendar month in UTC. The boundary is
// computed in UTC and compared as an instant, so a database session in another
// timezone cannot move the month.
func (s *Spend) MonthToDateMicros(ctx context.Context) (spend.Micros, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	var total int64
	if err := s.pool.QueryRow(ctx, `
		SELECT coalesce(sum(cost_micros), 0)::bigint
		FROM reelpin.provider_usage
		WHERE occurred_at >= date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'`,
	).Scan(&total); err != nil {
		return 0, fmt.Errorf("summing this month's provider spend: %w", err)
	}
	return spend.Micros(total), nil
}
