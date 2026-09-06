package search

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/embed"
	"github.com/XploY04/reelpin-go/internal/reels"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Filters narrow a search before ranking, inside SQL, so an arm never spends
// its depth on rows the user excluded.
type Filters struct {
	Platforms   []string
	Category    string
	Subcategory string
	SavedDate   string
}

type Service struct {
	pool     *pgxpool.Pool
	embedder embed.Embedder
	logger   *slog.Logger
	now      func() time.Time
	// MaxDistance is the vector arm's relevance gate. It is a field and not a
	// constant because the right cutoff depends on the embedding model, and
	// tuning it is how the arm is calibrated against a real library.
	MaxDistance float64
}

func NewService(pool *pgxpool.Pool, embedder embed.Embedder, logger *slog.Logger, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{
		pool:        pool,
		embedder:    embedder,
		logger:      logger,
		now:         now,
		MaxDistance: MaxDenseDistance,
	}
}

// Search runs the three arms, fuses them, and returns display reels.
func (s *Service) Search(ctx context.Context, userID, query string, filters Filters, limit int) (Response, error) {
	normalized := NormalizeQuery(query)
	if len([]rune(normalized)) < MinQueryRunes {
		return Response{Query: normalized, SearchMode: "empty", Results: []Result{}}, nil
	}
	if limit <= 0 || limit > MaxLimit {
		limit = DefaultLimit
	}

	arms := map[string][]Candidate{}
	used := []string{}

	// Dense first, because it is the only arm that costs a provider call and
	// the only one that can be unavailable.
	if s.embedder != nil {
		candidates, err := s.dense(ctx, userID, normalized, filters)
		if err != nil {
			return Response{}, err
		}
		if len(candidates) > 0 {
			arms[ArmDense] = candidates
			used = append(used, ArmDense)
		}
	}

	sparse, err := s.sparse(ctx, userID, normalized, filters)
	if err != nil {
		return Response{}, err
	}
	if len(sparse) > 0 {
		arms[ArmSparse] = sparse
		used = append(used, ArmSparse)
	}

	fuzzy, err := s.fuzzy(ctx, userID, normalized, filters)
	if err != nil {
		return Response{}, err
	}
	if len(fuzzy) > 0 {
		arms[ArmFuzzy] = fuzzy
		used = append(used, ArmFuzzy)
	}

	fused := Fuse(arms)
	if len(fused) > limit {
		fused = fused[:limit]
	}
	if len(fused) == 0 {
		return Response{Query: normalized, SearchMode: Mode(used), Results: []Result{}}, nil
	}

	ids := make([]string, 0, len(fused))
	for _, hit := range fused {
		ids = append(ids, hit.ReelID)
	}
	records, err := s.load(ctx, userID, ids)
	if err != nil {
		return Response{}, err
	}

	best := fused[0].Score
	results := make([]Result, 0, len(fused))
	for _, hit := range fused {
		record, ok := records[hit.ReelID]
		if !ok {
			// It was deleted between ranking and loading.
			continue
		}
		percent := RelevancePercent(hit.Score, best)
		results = append(results, Result{
			Reel:              reels.BuildDisplayReel(record, s.now().UTC()),
			RelevanceScore:    hit.Score,
			RelevancePercent:  percent,
			DisplayScoreLabel: ScoreLabel(percent),
		})
	}

	return Response{
		Query:      normalized,
		SearchMode: Mode(used),
		Total:      len(results),
		Results:    results,
	}, nil
}

// dense ranks by cosine distance against the embedding of the query. The
// stored vector's model and dimension have to match the one that embedded the
// query: two sets made differently are not comparable, and mixing them ranks by
// nothing at all.
//
// A missing query vector degrades the search rather than failing it. Words and
// spelling still find things, and a provider outage should not empty a library.
func (s *Service) dense(ctx context.Context, userID, query string, filters Filters) ([]Candidate, error) {
	vectors, err := s.embedder.Embed(ctx, []string{query})
	if err != nil || len(vectors) != 1 {
		s.logger.Warn("query embedding unavailable, serving lexical results only", "error", err)
		return nil, nil
	}

	where, args := filterClause(userID, filters)
	args = append(args, embed.Vector(vectors[0]), s.MaxDistance,
		s.embedder.Model(), s.embedder.Dimension(), CandidatesPerArm)
	vector, distance := len(args)-4, len(args)-3
	model, dimension, limit := len(args)-2, len(args)-1, len(args)

	statement := fmt.Sprintf(`
		%s
		SELECT s.save_id::text
		FROM scoped s
		JOIN reelpin.content_embeddings e ON e.content_version_id = s.version_id
		WHERE e.embedding IS NOT NULL
		  AND e.model = $%d AND e.dimension = $%d
		  AND e.embedding <=> $%d::vector <= $%d
		ORDER BY e.embedding <=> $%d::vector
		LIMIT $%d`,
		where, model, dimension, vector, distance, vector, limit)

	return s.candidates(ctx, statement, args)
}

// sparse is full-text over the weighted document the schema keeps on every
// content version.
func (s *Service) sparse(ctx context.Context, userID, query string, filters Filters) ([]Candidate, error) {
	where, args := filterClause(userID, filters)
	args = append(args, query, CandidatesPerArm)
	text, limit := len(args)-1, len(args)

	statement := fmt.Sprintf(`
		%s
		SELECT s.save_id::text
		FROM scoped s
		WHERE s.search_document @@ websearch_to_tsquery('english', $%d)
		ORDER BY ts_rank(s.search_document, websearch_to_tsquery('english', $%d)) DESC,
		         s.saved_at DESC
		LIMIT $%d`, where, text, text, limit)

	return s.candidates(ctx, statement, args)
}

// fuzzy catches misspellings in the one place people type from memory: the
// title. The candidate set is already one user's library, so this scans a few
// hundred rows and needs no trigram index of its own.
func (s *Service) fuzzy(ctx context.Context, userID, query string, filters Filters) ([]Candidate, error) {
	where, args := filterClause(userID, filters)
	args = append(args, query, CandidatesPerArm)
	text, limit := len(args)-1, len(args)

	statement := fmt.Sprintf(`
		%s
		SELECT s.save_id::text
		FROM scoped s
		WHERE public.similarity(s.title, $%d) > 0.25
		ORDER BY public.similarity(s.title, $%d) DESC, s.saved_at DESC
		LIMIT $%d`, where, text, text, limit)

	return s.candidates(ctx, statement, args)
}

func (s *Service) candidates(ctx context.Context, statement string, args []any) ([]Candidate, error) {
	rows, err := s.pool.Query(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("searching: %w", err)
	}
	defer rows.Close()

	candidates := []Candidate{}
	rank := 1
	for rows.Next() {
		var reelID string
		if err := rows.Scan(&reelID); err != nil {
			return nil, fmt.Errorf("searching: %w", err)
		}
		candidates = append(candidates, Candidate{ReelID: reelID, Rank: rank})
		rank++
	}
	return candidates, rows.Err()
}

// filterClause builds the `scoped` CTE every arm starts from: this user's
// saves, already narrowed, carrying what the arms rank on. The user is always
// the first condition, so no arm can reach another account's saves.
func filterClause(userID string, filters Filters) (string, []any) {
	args := []any{userID}
	clauses := []string{"s.user_id = $1"}

	if len(filters.Platforms) > 0 {
		stored := []string{}
		includesOther := false
		for _, platform := range filters.Platforms {
			if platform == reels.OtherPlatform {
				includesOther = true
				continue
			}
			stored = append(stored, reels.StoredPlatformValues(platform)...)
		}

		parts := []string{}
		if len(stored) > 0 {
			args = append(args, stored)
			parts = append(parts, fmt.Sprintf("lower(trim(c.source_platform)) = ANY($%d)", len(args)))
		}
		if includesOther {
			args = append(args, reels.KnownStoredPlatformValues())
			parts = append(parts, fmt.Sprintf(
				"(c.source_platform IS NULL OR lower(trim(c.source_platform)) <> ALL($%d))", len(args)))
		}
		if len(parts) > 0 {
			clauses = append(clauses, "("+strings.Join(parts, " OR ")+")")
		}
	}

	if filters.Category != "" {
		args = append(args, filters.Category)
		clauses = append(clauses, fmt.Sprintf("v.raw_extraction->>'category' = $%d", len(args)))
	}
	if filters.Subcategory != "" {
		args = append(args, filters.Subcategory)
		clauses = append(clauses, fmt.Sprintf("v.raw_extraction->>'subcategory' = $%d", len(args)))
	}
	if filters.SavedDate != "" {
		args = append(args, filters.SavedDate)
		clauses = append(clauses, fmt.Sprintf(
			"s.saved_at >= $%[1]d::date AND s.saved_at < $%[1]d::date + 1", len(args)))
	}

	// A save whose content has no current version has nothing to rank, so the
	// join is inner rather than outer.
	cte := fmt.Sprintf(`
		WITH scoped AS (
			SELECT s.id AS save_id, s.saved_at, v.id AS version_id,
			       v.search_document, v.title
			FROM reelpin.user_saves s
			JOIN reelpin.contents c ON c.id = s.content_id
			JOIN reelpin.content_versions v ON v.id = c.current_version_id
			WHERE %s
		)`, strings.Join(clauses, " AND "))

	return cte, args
}

// load fetches the saves a search chose, scoped to the user again, so a bug in
// ranking can never leak another account's row into a response.
func (s *Service) load(ctx context.Context, userID string, ids []string) (map[string]reels.ReelRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT s.id::text, s.user_id::text, c.normalized_url,
		       c.source_platform, c.source_content_type,
		       COALESCE(v.title, ''), COALESCE(v.summary, ''),
		       COALESCE(v.tags, '{}'), COALESCE(v.key_facts, '{}'),
		       v.raw_extraction->>'category', v.raw_extraction->>'subcategory',
		       NULLIF(v.media->>'thumbnail_url', ''),
		       s.saved_at
		FROM reelpin.user_saves s
		JOIN reelpin.contents c ON c.id = s.content_id
		LEFT JOIN reelpin.content_versions v ON v.id = c.current_version_id
		WHERE s.user_id = $1 AND s.id = ANY($2::uuid[])`,
		userID, ids)
	if err != nil {
		return nil, fmt.Errorf("loading search results: %w", err)
	}
	defer rows.Close()

	records := map[string]reels.ReelRecord{}
	for rows.Next() {
		var (
			record                reels.ReelRecord
			tags, facts           []string
			category, subcategory *string
			savedAt               time.Time
		)
		if err := rows.Scan(&record.ID, &record.UserID, &record.URL,
			&record.SourcePlatform, &record.SourceContentType,
			&record.Title, &record.Summary, &tags, &facts,
			&category, &subcategory, &record.ThumbnailURL, &savedAt); err != nil {
			return nil, fmt.Errorf("reading a search result: %w", err)
		}
		record.SecondaryCategories = tags
		record.KeyFacts = facts
		if category != nil {
			record.Category = *category
		}
		if subcategory != nil {
			record.Subcategory = *subcategory
		}
		record.CreatedAt = &savedAt
		records[record.ID] = record
	}
	return records, rows.Err()
}
