package taxonomy

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Proposal is one concept the model asked for, aggregated across every job
// that asked. ContentCount is the number of *distinct runs* that wanted it,
// which is the evidence the auto-approval threshold is measured against: one
// run proposing the same name five times is one opinion, not five.
type Proposal struct {
	NormalizedName string   `json:"normalized_name"`
	Names          []string `json:"names"`
	Descriptions   []string `json:"descriptions"`
	ContentCount   int      `json:"content_count"`
	IDs            []string `json:"-"`
}

// PendingProposals aggregates everything waiting for a decision. The curator
// reads this, the model judges it, and only then does anything change.
func PendingProposals(ctx context.Context, pool *pgxpool.Pool) ([]Proposal, error) {
	rows, err := pool.Query(ctx, `
		SELECT normalized_name,
		       array_agg(DISTINCT proposed_name),
		       array_agg(DISTINCT description) FILTER (WHERE description <> ''),
		       count(DISTINCT COALESCE(source_run_id::text, id::text)),
		       array_agg(id::text)
		FROM reelpin.category_proposals
		WHERE status = 'pending'
		GROUP BY normalized_name
		ORDER BY count(DISTINCT COALESCE(source_run_id::text, id::text)) DESC, normalized_name`)
	if err != nil {
		return nil, fmt.Errorf("reading pending proposals: %w", err)
	}
	defer rows.Close()

	proposals := []Proposal{}
	for rows.Next() {
		var proposal Proposal
		if err := rows.Scan(&proposal.NormalizedName, &proposal.Names,
			&proposal.Descriptions, &proposal.ContentCount, &proposal.IDs); err != nil {
			return nil, fmt.Errorf("reading a proposal: %w", err)
		}
		proposals = append(proposals, proposal)
	}
	return proposals, rows.Err()
}

// existingNames maps every normalized name already spoken for — active
// categories and their aliases — to the category it resolves to. A proposal
// matching one of these is a duplicate, whatever the model thinks.
func existingNames(ctx context.Context, pool *pgxpool.Pool) (map[string]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT normalized_name, id::text FROM reelpin.categories WHERE active
		UNION ALL
		SELECT a.normalized_alias, a.category_id::text
		FROM reelpin.category_aliases a
		JOIN reelpin.categories c ON c.id = a.category_id AND c.active`)
	if err != nil {
		return nil, fmt.Errorf("reading existing names: %w", err)
	}
	defer rows.Close()

	names := map[string]string{}
	for rows.Next() {
		var name, id string
		if err := rows.Scan(&name, &id); err != nil {
			return nil, fmt.Errorf("reading an existing name: %w", err)
		}
		names[name] = id
	}
	return names, rows.Err()
}
