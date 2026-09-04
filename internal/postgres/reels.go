package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/reels"
	"github.com/XploY04/reelpin-go/internal/uuid"
	"github.com/jackc/pgx/v5"
)

// statsRowLimit matches the Python display query, which reads at most 5000 rows
// per user before aggregating.
const statsRowLimit = 5000

const reelListColumns = `id, user_id, url, normalized_url, source_platform, source_content_type,
	source_content_id, processing_version, ingestion_method, transcript_source, thumbnail_url,
	title, summary, category, subcategory, secondary_categories, key_facts, locations,
	people_mentioned, actionable_items, events, parse_status, created_at`

type Reels struct {
	db Querier
}

func NewReels(db Querier) *Reels {
	return &Reels{db: db}
}

func (r *Reels) List(ctx context.Context, userID string, options reels.ListOptions) ([]reels.ReelRecord, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	args := []any{userID}
	where := []string{"user_id = $1"}

	if condition := platformCondition(options.Platforms, &args); condition != "" {
		where = append(where, condition)
	}
	if options.Category != "" {
		args = append(args, options.Category)
		where = append(where, fmt.Sprintf("category = $%d", len(args)))
	}
	if options.Subcategory != "" {
		args = append(args, options.Subcategory)
		where = append(where, fmt.Sprintf("subcategory = $%d", len(args)))
	}
	if options.SavedDate != "" {
		day, err := time.Parse("2006-01-02", options.SavedDate)
		if err != nil {
			return nil, fmt.Errorf("saved_date: %w", err)
		}
		args = append(args, day, day.AddDate(0, 0, 1))
		where = append(where, fmt.Sprintf("created_at >= $%d AND created_at < $%d", len(args)-1, len(args)))
	}

	args = append(args, options.Limit)
	limitClause := fmt.Sprintf("LIMIT $%d", len(args))
	offsetClause := ""
	if options.Offset > 0 {
		args = append(args, options.Offset)
		offsetClause = fmt.Sprintf(" OFFSET $%d", len(args))
	}

	query := fmt.Sprintf(
		"SELECT %s FROM reels WHERE %s ORDER BY %s %s%s",
		reelListColumns,
		strings.Join(where, " AND "),
		orderBy(options.Sort),
		limitClause,
		offsetClause,
	)

	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	records := []reels.ReelRecord{}
	for rows.Next() {
		record, err := scanReel(rows, false)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (r *Reels) Get(ctx context.Context, userID string, id uuid.UUID) (reels.ReelRecord, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	query := fmt.Sprintf(
		"SELECT %s, transcript FROM reels WHERE id = $1 AND user_id = $2",
		reelListColumns,
	)
	rows, err := r.db.Query(ctx, query, id.String(), userID)
	if err != nil {
		return reels.ReelRecord{}, err
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return reels.ReelRecord{}, err
		}
		return reels.ReelRecord{}, reels.ErrNotFound
	}
	record, err := scanReel(rows, true)
	if err != nil {
		return reels.ReelRecord{}, err
	}
	return record, rows.Err()
}

func (r *Reels) Facets(ctx context.Context, userID string) ([]reels.FacetRow, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	rows, err := r.db.Query(ctx,
		`SELECT source_platform, category, subcategory, count(*)::int
		 FROM reels WHERE user_id = $1
		 GROUP BY source_platform, category, subcategory`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	facets := []reels.FacetRow{}
	for rows.Next() {
		var row reels.FacetRow
		if err := rows.Scan(&row.SourcePlatform, &row.Category, &row.Subcategory, &row.Count); err != nil {
			return nil, err
		}
		facets = append(facets, row)
	}
	return facets, rows.Err()
}

func (r *Reels) Stats(ctx context.Context, userID string) (reels.LibraryStats, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	rows, err := r.db.Query(ctx,
		`SELECT id, category, subcategory, secondary_categories, locations
		 FROM reels WHERE user_id = $1 ORDER BY created_at DESC LIMIT $2`,
		userID, statsRowLimit,
	)
	if err != nil {
		return reels.LibraryStats{}, err
	}
	defer rows.Close()

	stats := reels.LibraryStats{}
	categories := map[string]bool{}
	subcategories := map[[2]string]bool{}
	tags := map[string]bool{}

	for rows.Next() {
		var (
			id                    string
			category, subcategory *string
			secondaryRaw, locsRaw []byte
			record                reels.ReelRecord
		)
		if err := rows.Scan(&id, &category, &subcategory, &secondaryRaw, &locsRaw); err != nil {
			return reels.LibraryStats{}, err
		}
		if err := decodeJSON(secondaryRaw, &record.SecondaryCategories); err != nil {
			return reels.LibraryStats{}, err
		}
		if err := decodeJSON(locsRaw, &record.Locations); err != nil {
			return reels.LibraryStats{}, err
		}
		record.ID = id

		stats.TotalReels++
		cleanCategory := strings.TrimSpace(text(category))
		cleanSubcategory := strings.TrimSpace(text(subcategory))
		if cleanCategory != "" {
			categories[cleanCategory] = true
		}
		if cleanSubcategory != "" {
			subcategories[[2]string{cleanCategory, cleanSubcategory}] = true
		}
		for _, tag := range record.SecondaryCategories {
			if cleaned := strings.TrimSpace(tag); cleaned != "" {
				tags[cleaned] = true
			}
		}
		stats.TotalPinnedLocations += len(reels.BuildMappableLocations(record))
	}
	if err := rows.Err(); err != nil {
		return reels.LibraryStats{}, err
	}

	stats.TotalCategories = len(categories)
	stats.TotalSubcategories = len(subcategories)
	stats.TotalTags = len(tags)
	return stats, nil
}

func orderBy(sort string) string {
	switch sort {
	case "oldest":
		return "created_at ASC"
	case "title":
		return "title ASC"
	default:
		return "created_at DESC"
	}
}

// platformCondition restricts a query to canonical platforms. `other` covers
// rows whose stored platform is NULL, blank, or unknown, so saved links from
// generic hosts stay reachable.
func platformCondition(platforms []string, args *[]any) string {
	if len(platforms) == 0 {
		return ""
	}

	var parts []string
	var stored []string
	includesOther := false
	for _, platform := range platforms {
		if platform == reels.OtherPlatform {
			includesOther = true
			continue
		}
		stored = append(stored, reels.StoredPlatformValues(platform)...)
	}

	if len(stored) > 0 {
		*args = append(*args, stored)
		parts = append(parts, fmt.Sprintf("lower(trim(source_platform)) = ANY($%d)", len(*args)))
	}
	if includesOther {
		*args = append(*args, reels.KnownStoredPlatformValues())
		parts = append(parts, fmt.Sprintf(
			"(source_platform IS NULL OR lower(trim(source_platform)) <> ALL($%d))",
			len(*args),
		))
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}

func scanReel(rows pgx.Rows, includeTranscript bool) (reels.ReelRecord, error) {
	var (
		record       reels.ReelRecord
		title        *string
		summary      *string
		transcript   *string
		category     *string
		subcategory  *string
		parseStatus  *string
		secondaryRaw []byte
		keyFactsRaw  []byte
		locationsRaw []byte
		peopleRaw    []byte
		actionsRaw   []byte
		eventsRaw    []byte
	)

	targets := []any{
		&record.ID, &record.UserID, &record.URL, &record.NormalizedURL, &record.SourcePlatform,
		&record.SourceContentType, &record.SourceContentID, &record.ProcessingVersion,
		&record.IngestionMethod, &record.TranscriptSource, &record.ThumbnailURL,
		&title, &summary, &category, &subcategory, &secondaryRaw, &keyFactsRaw, &locationsRaw,
		&peopleRaw, &actionsRaw, &eventsRaw, &parseStatus, &record.CreatedAt,
	}
	if includeTranscript {
		targets = append(targets, &transcript)
	}
	if err := rows.Scan(targets...); err != nil {
		return reels.ReelRecord{}, err
	}

	record.Title = text(title)
	record.Summary = text(summary)
	record.Transcript = text(transcript)
	record.Category = text(category)
	record.Subcategory = text(subcategory)
	record.ParseStatus = parseStatus

	for _, decode := range []struct {
		raw    []byte
		target any
	}{
		{secondaryRaw, &record.SecondaryCategories},
		{keyFactsRaw, &record.KeyFacts},
		{locationsRaw, &record.Locations},
		{peopleRaw, &record.PeopleMentioned},
		{actionsRaw, &record.ActionableItems},
		{eventsRaw, &record.Events},
	} {
		if err := decodeJSON(decode.raw, decode.target); err != nil {
			return reels.ReelRecord{}, err
		}
	}
	return record, nil
}
