// Package taxonomy owns the category tree the extraction prompt is built from,
// and the weekly curation that grows it.
//
// The split that matters: a processing job may *propose* a category, and only
// the curator may activate one. That is why proposals are their own table and
// why the application role has no write grant on categories.
package taxonomy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode"

	"github.com/XploY04/reelpin-go/internal/ai"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Service reads the active tree. It never writes: activation belongs to the
// curator, which runs under the maintenance role.
type Service struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// Tree is the active taxonomy as the model sees it, plus the version it was
// read at. Categorization is pinned to a version for the length of a run: a
// curator activating a category mid-run must not change how that run files.
type Tree struct {
	Options []ai.TaxonomyOption
	Version string
}

// ActiveTree reads the two-level tree in one query. Ordering is by normalized
// name so the version is stable for an unchanged tree.
func (s *Service) ActiveTree(ctx context.Context) (Tree, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, COALESCE(parent_id::text, ''), name, description
		FROM reelpin.categories
		WHERE active
		ORDER BY parent_id NULLS FIRST, normalized_name`)
	if err != nil {
		return Tree{}, fmt.Errorf("loading the taxonomy: %w", err)
	}
	defer rows.Close()

	type row struct{ id, parent, name, description string }
	scanned := []row{}
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.parent, &r.name, &r.description); err != nil {
			return Tree{}, fmt.Errorf("reading a category: %w", err)
		}
		scanned = append(scanned, r)
	}
	if rows.Err() != nil {
		return Tree{}, fmt.Errorf("loading the taxonomy: %w", rows.Err())
	}

	byID := map[string]*ai.TaxonomyOption{}
	roots := []string{}
	for _, r := range scanned {
		byID[r.id] = &ai.TaxonomyOption{ID: r.id, Name: r.name, Description: r.description}
		if r.parent == "" {
			roots = append(roots, r.id)
		}
	}
	for _, r := range scanned {
		if r.parent == "" {
			continue
		}
		if parent, ok := byID[r.parent]; ok {
			parent.Subcategories = append(parent.Subcategories, *byID[r.id])
		}
	}

	options := make([]ai.TaxonomyOption, 0, len(roots))
	for _, id := range roots {
		options = append(options, *byID[id])
	}
	return Tree{Options: options, Version: version(options)}, nil
}

// version fingerprints the tree by identity and name. Two reads of an
// unchanged tree produce the same version; any activation, rename or
// deactivation produces a different one.
func version(options []ai.TaxonomyOption) string {
	parts := []string{}
	for _, option := range options {
		parts = append(parts, option.ID, option.Name)
		for _, sub := range option.Subcategories {
			parts = append(parts, sub.ID, sub.Name)
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:8])
}

// Normalize is the one definition of "the same category name". The database
// uniqueness constraint and the curator's duplicate detection both depend on
// it agreeing with itself, so it lives here and nowhere else.
func Normalize(name string) string {
	var builder strings.Builder
	lastWasSeparator := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			builder.WriteRune(r)
			lastWasSeparator = false
		case !lastWasSeparator && builder.Len() > 0:
			builder.WriteRune(' ')
			lastWasSeparator = true
		}
	}
	return strings.TrimSpace(builder.String())
}
