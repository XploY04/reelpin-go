// Package postgres reads the existing Supabase tables with plain pgx. It owns
// no schema and writes nothing.
package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const queryTimeout = 5 * time.Second

// Querier is the slice of pgxpool.Pool the readers use.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

var _ Querier = (*pgxpool.Pool)(nil)

func withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, queryTimeout)
}

func decodeJSON(raw []byte, target any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return json.Unmarshal(raw, target)
}

func text(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
