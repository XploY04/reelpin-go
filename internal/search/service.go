package search

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/embed"
	"github.com/XploY04/reelpin-go/internal/metrics"
	"github.com/XploY04/reelpin-go/internal/postgres"
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
	// Metrics is optional. Nil means searches are not measured.
	Metrics *metrics.Metrics
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
func (s *Service) Search(ctx context.Context, userID, query string, filters Filters, limit int) (response Response, err error) {
	started := time.Now()
	// Every path that returns a response is observed, including the two that
	// return nothing. An empty result is the outcome the relevance alert
	// watches for, so skipping it would make that alert unfireable.
	defer func() {
		if err == nil && s.Metrics != nil {
			s.Metrics.ObserveSearch(response.SearchMode, response.Total, time.Since(started))
		}
	}()

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
		vectors, err := s.embedder.Embed(ctx, []string{normalized}, embed.TaskQuery)
		if err != nil || len(vectors) != 1 {
			// A missing query vector degrades the search rather than failing
			// it: words and spelling still find things.
			s.logger.Warn("query embedding unavailable, serving lexical results only", "error", err)
		} else {
			candidates, err := s.dense(ctx, userID, embed.Vector(embed.Normalize(vectors[0])), filters)
			if err != nil {
				return Response{}, err
			}
			if len(candidates) > 0 {
				arms[ArmDense] = candidates
				used = append(used, ArmDense)
			}
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

// dense ranks by cosine distance over the content document and the transcript
// chunks, taking each reel's best chunk so a long transcript cannot crowd out
// everything else.
func (s *Service) dense(ctx context.Context, userID, vector string, filters Filters) ([]Candidate, error) {
	where, args := filterClause(userID, filters)
	args = append(args, vector, s.MaxDistance, CandidatesPerArm)

	query := fmt.Sprintf(`
		WITH scoped AS (
			SELECT r.id, r.content_version_id FROM public.reels r WHERE %s
		),
		document AS (
			SELECT s.id, v.embedding <=> $%d::vector AS distance
			FROM scoped s
			JOIN reelpin.content_versions v ON v.id = s.content_version_id
			WHERE v.embedding IS NOT NULL AND v.embedding <=> $%d::vector <= $%d
		),
		chunk AS (
			SELECT s.id, min(c.embedding <=> $%d::vector) AS distance
			FROM scoped s
			JOIN reelpin.content_chunks c ON c.content_version_id = s.content_version_id
			WHERE c.embedding IS NOT NULL AND c.embedding <=> $%d::vector <= $%d
			GROUP BY s.id
		)
		SELECT id::text FROM (
			SELECT id, distance FROM document
			UNION ALL
			SELECT id, distance FROM chunk
		) hits
		GROUP BY id
		ORDER BY min(distance)
		LIMIT $%d`, where,
		len(args)-2, len(args)-2, len(args)-1,
		len(args)-2, len(args)-2, len(args)-1,
		len(args))

	return s.candidates(ctx, query, args)
}

// sparse is full-text over the fields that carry signal. The raw transcript is
// deliberately excluded: it matches everything and ranks nothing.
func (s *Service) sparse(ctx context.Context, userID, query string, filters Filters) ([]Candidate, error) {
	where, args := filterClause(userID, filters)
	args = append(args, query, CandidatesPerArm)

	statement := fmt.Sprintf(`
		SELECT r.id::text
		FROM public.reels r
		WHERE %s
		  AND to_tsvector('english',
		        coalesce(r.title,'') || ' ' || coalesce(r.summary,'') || ' ' ||
		        coalesce(r.category,'') || ' ' || coalesce(r.subcategory,'') || ' ' ||
		        coalesce(r.key_facts::text,'') || ' ' || coalesce(r.locations::text,'') || ' ' ||
		        coalesce(r.people_mentioned::text,''))
		      @@ websearch_to_tsquery('english', $%d)
		ORDER BY ts_rank(
		        to_tsvector('english', coalesce(r.title,'') || ' ' || coalesce(r.summary,'')),
		        websearch_to_tsquery('english', $%d)) DESC,
		        r.created_at DESC
		LIMIT $%d`, where, len(args)-1, len(args)-1, len(args))

	return s.candidates(ctx, statement, args)
}

// fuzzy catches misspellings in the two places people type from memory: the
// title and a place name.
func (s *Service) fuzzy(ctx context.Context, userID, query string, filters Filters) ([]Candidate, error) {
	where, args := filterClause(userID, filters)
	args = append(args, query, CandidatesPerArm)

	statement := fmt.Sprintf(`
		SELECT r.id::text
		FROM public.reels r
		WHERE %s
		  AND (similarity(r.title, $%d) > 0.25
		       OR similarity(coalesce(r.locations::text, ''), $%d) > 0.15)
		ORDER BY GREATEST(similarity(r.title, $%d),
		                  similarity(coalesce(r.locations::text, ''), $%d)) DESC
		LIMIT $%d`, where, len(args)-1, len(args)-1, len(args)-1, len(args)-1, len(args))

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

// filterClause builds the scope every arm shares. The user is always the first
// condition: no arm may ever reach another account's reels.
func filterClause(userID string, filters Filters) (string, []any) {
	args := []any{userID}
	clauses := []string{"r.user_id = $1"}

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
			parts = append(parts, fmt.Sprintf("lower(trim(r.source_platform)) = ANY($%d)", len(args)))
		}
		if includesOther {
			args = append(args, reels.KnownStoredPlatformValues())
			parts = append(parts, fmt.Sprintf(
				"(r.source_platform IS NULL OR lower(trim(r.source_platform)) <> ALL($%d))", len(args)))
		}
		if len(parts) > 0 {
			clauses = append(clauses, "("+strings.Join(parts, " OR ")+")")
		}
	}

	if filters.Category != "" {
		args = append(args, filters.Category)
		clauses = append(clauses, fmt.Sprintf("r.category = $%d", len(args)))
	}
	if filters.Subcategory != "" {
		args = append(args, filters.Subcategory)
		clauses = append(clauses, fmt.Sprintf("r.subcategory = $%d", len(args)))
	}
	if filters.SavedDate != "" {
		args = append(args, filters.SavedDate)
		clauses = append(clauses, fmt.Sprintf("r.created_at::date = $%d::date", len(args)))
	}

	return strings.Join(clauses, " AND "), args
}

// load fetches the reels a search chose, scoped to the user again, so a bug in
// ranking can never leak another account's row into a response.
func (s *Service) load(ctx context.Context, userID string, ids []string) (map[string]reels.ReelRecord, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT "+postgres.ReelListColumns+
			" FROM public.reels r WHERE r.user_id = $1 AND r.id = ANY($2::uuid[])",
		userID, ids)
	if err != nil {
		return nil, fmt.Errorf("loading search results: %w", err)
	}
	defer rows.Close()

	records := map[string]reels.ReelRecord{}
	for rows.Next() {
		record, err := postgres.ScanReelList(rows)
		if err != nil {
			return nil, err
		}
		records[record.ID] = record
	}
	return records, rows.Err()
}
