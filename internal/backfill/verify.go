package backfill

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/sourceidentity"
	"github.com/jackc/pgx/v5/pgxpool"
)

// defaultSample is the 100-record comparison the rehearsal is signed off on.
const defaultSample = 100

// VerifyOptions bounds one verification pass.
type VerifyOptions struct {
	// Sample is how many carried-over rows per legacy table are compared field
	// by field. 0 takes the default. It also caps every list of ids and users
	// the report carries.
	Sample int
	// BatchSize bounds how many sampled rows are read per query.
	BatchSize int
	// LegacySchema is where the legacy tables are read from. Empty means
	// DefaultLegacySchema.
	LegacySchema string
}

// TableVerification is one legacy table's account of itself. Every row in scope
// belongs in exactly one of Carried, Skipped, CarriedButMissing or Unexplained.
type TableVerification struct {
	SourceTable string `json:"source_table"`
	// Absent is a legacy table this source does not have. Nothing is counted
	// for it, and nothing about it can fail.
	Absent  bool `json:"absent"`
	InScope int  `json:"in_scope"`
	Carried int  `json:"carried"`
	Skipped int  `json:"skipped"`
	// SkippedByAction breaks the skips down by the reason the backfill
	// recorded: skipped_invalid_url, skipped_unreadable, skipped_blocklisted,
	// skipped_active, skipped_ambiguous, skipped_conflict, skipped_invalid,
	// skipped_unknown_reference, skipped_no_location, skipped_vanished.
	SkippedByAction map[string]int `json:"skipped_by_action"`
	// Unexplained counts legacy rows with no audit entry at all. A row that was
	// neither carried over nor deliberately skipped is the migration mismatch
	// the rehearsal gate counts, because nothing decided anything about it.
	Unexplained    int      `json:"unexplained"`
	UnexplainedIDs []string `json:"unexplained_ids"`
	// CarriedButMissing counts rows the audit calls carried that have no
	// canonical row under their preserved id. The app still holds those ids.
	CarriedButMissing int      `json:"carried_but_missing"`
	MissingIDs        []string `json:"missing_ids"`

	// Expected is the legacy rows in scope that were not deliberately
	// skipped, and Present is how many of them have a canonical row owned by
	// the same user. The two sides of the migration, counted the same way.
	Expected int `json:"expected"`
	Present  int `json:"present"`
	// CanonicalTotal is every row in the canonical table, including the ones
	// the new app wrote itself. It is context, not a check.
	CanonicalTotal int `json:"canonical_total"`
	// Migrated is the legacy rows owned by users Go has served. They are left
	// out of every other count: those users change their rows in Go now, and
	// the deltas no longer carry their legacy rows.
	Migrated int `json:"migrated"`
	// UsersDiffering counts users whose Expected and Present differ, and
	// UserDiffs names the first of them.
	UsersDiffering int        `json:"users_differing"`
	UserDiffs      []UserDiff `json:"user_diffs"`
}

// UserDiff is one user's two counts for one table.
type UserDiff struct {
	UserID    string `json:"user_id"`
	Legacy    int    `json:"legacy"`
	Canonical int    `json:"canonical"`
}

// Mismatch is one field that does not agree, named by the legacy row it came
// from. It carries no values: this report is logged, and the audit table it
// reads from stores identifiers only, no content and no user text. The two rows
// are one query away by source id.
type Mismatch struct {
	SourceTable string `json:"source_table"`
	SourceID    string `json:"source_id"`
	Field       string `json:"field"`
}

// SampleReport is the field-level half of the verification.
type SampleReport struct {
	Compared int `json:"compared"`
	// TextNotComparable counts sampled reels whose content is saved by more
	// than one user. See compareReel.
	TextNotComparable int        `json:"text_not_comparable"`
	Mismatches        int        `json:"mismatches"`
	Examples          []Mismatch `json:"examples"`
}

func (s *SampleReport) differ(table, sourceID, field string) {
	s.Mismatches++
	s.Examples = append(s.Examples, Mismatch{SourceTable: table, SourceID: sourceID, Field: field})
}

// EmbeddingCoverage counts carried reels that had an embedding in the legacy
// table, and how many of those have one on the canonical side.
type EmbeddingCoverage struct {
	// Measured is false when the legacy reels have no embedding column.
	Measured  bool `json:"measured"`
	Legacy    int  `json:"legacy"`
	Canonical int  `json:"canonical"`
	// Models splits Canonical by the model each vector was made with. Search
	// only compares a query with vectors from the configured EMBEDDING_MODEL,
	// so a carried vector under any other model is invisible to it.
	Models map[string]int `json:"models"`
}

// MapPoints counts what the map draws on each side: the located places on
// carried reels, and the pins users dropped themselves.
type MapPoints struct {
	LegacyReelPoints    int `json:"legacy_reel_points"`
	CanonicalReelPoints int `json:"canonical_reel_points"`
	LegacyManualPins    int `json:"legacy_manual_pins"`
	CanonicalManualPins int `json:"canonical_manual_pins"`
}

// VerifyReport is what a runbook reads after a rehearsal backfill.
type VerifyReport struct {
	Tables     []TableVerification `json:"tables"`
	Sample     SampleReport        `json:"sample"`
	Embeddings EmbeddingCoverage   `json:"embeddings"`
	MapPoints  MapPoints           `json:"map_points"`
}

// OK is the single condition to key off: every legacy row in scope was either
// carried over or deliberately skipped, every carried row is really there and
// belongs to the right user, nothing in the sample differs, and search and the
// map have everything the legacy side had.
func (r VerifyReport) OK() bool {
	for _, table := range r.Tables {
		if table.Unexplained > 0 || table.CarriedButMissing > 0 ||
			table.Expected != table.Present || table.UsersDiffering > 0 {
			return false
		}
	}
	if r.Embeddings.Canonical < r.Embeddings.Legacy {
		return false
	}
	if r.MapPoints.CanonicalReelPoints < r.MapPoints.LegacyReelPoints ||
		r.MapPoints.CanonicalManualPins != r.MapPoints.LegacyManualPins {
		return false
	}
	return r.Sample.Mismatches == 0
}

// legacySource is the only thing the legacy tables differ by, so the counting
// SQL is written once. Every field here is a compile-time constant, never
// anything a caller supplies.
type legacySource struct {
	name string
	// key is the legacy primary key column.
	key       string
	canonical string
	carried   string
	// canonicalKey is the canonical column a carried row's id lands in, or ""
	// when owned has to find it through the legacy row.
	canonicalKey string
	// owner is the legacy column naming the user, or "" for an anonymous table.
	owner string
	owned string
	// windowed marks a table the backfill only reads a recent slice of.
	windowed bool
	// sampled marks the tables compared field by field.
	sampled bool
}

func verifySources() []legacySource {
	sources := []legacySource{
		{
			name: sourceReels, key: "id", canonical: "reelpin.user_saves", carried: "saved",
			canonicalKey: "id", owner: "user_id", sampled: true,
		},
		{
			name: sourceJobs, key: "id", canonical: "reelpin.processing_jobs", carried: "copied",
			canonicalKey: "id", owner: "user_id", windowed: true, sampled: true,
		},
	}
	for _, mapping := range wave1Mappings {
		sources = append(sources, legacySource{
			name:         mapping.name,
			key:          mapping.key,
			canonical:    mapping.canonical,
			carried:      "copied",
			canonicalKey: mapping.canonicalKey,
			owner:        mapping.owner,
			owned:        mapping.owned,
		})
	}
	return sources
}

// Verifier reads the backfill's bookkeeping back against both schemas and says
// whether the carry-over can be signed off. Every statement it runs is a
// SELECT: the rehearsal it judges has to survive being judged.
type Verifier struct {
	pool     *pgxpool.Pool
	resolver *sourceidentity.Resolver
	source   source
}

func NewVerifier(pool *pgxpool.Pool) *Verifier {
	// Offline resolution, for the same reason the backfill uses it: the check
	// must not depend on a provider being reachable.
	return &Verifier{pool: pool, resolver: &sourceidentity.Resolver{}}
}

func (v *Verifier) Verify(ctx context.Context, options VerifyOptions) (VerifyReport, error) {
	if options.Sample <= 0 {
		options.Sample = defaultSample
	}
	if options.BatchSize <= 0 {
		options.BatchSize = 500
	}

	var err error
	if v.source, err = newSource(options.LegacySchema); err != nil {
		return VerifyReport{}, err
	}
	if err := v.source.describe(ctx, v.pool); err != nil {
		return VerifyReport{}, err
	}
	if err := v.source.require(sourceUsers, sourceReels, sourceJobs, sourceCache); err != nil {
		return VerifyReport{}, err
	}

	users, err := v.countUsers(ctx, options)
	if err != nil {
		return VerifyReport{}, err
	}
	report := VerifyReport{Tables: []TableVerification{users}}
	for _, source := range verifySources() {
		if !v.source.shape(source.name).exists {
			report.Tables = append(report.Tables, TableVerification{SourceTable: source.name, Absent: true})
			continue
		}
		counted, err := v.countTable(ctx, source, options)
		if err != nil {
			return VerifyReport{}, err
		}
		report.Tables = append(report.Tables, counted)
		if source.name == "manual_map_pins" {
			report.MapPoints.LegacyManualPins = counted.Expected
			report.MapPoints.CanonicalManualPins = counted.Present
		}

		if !source.sampled {
			continue
		}
		ids, err := v.sampleIDs(ctx, source, options.Sample)
		if err != nil {
			return VerifyReport{}, err
		}
		for start := 0; start < len(ids); start += options.BatchSize {
			end := min(start+options.BatchSize, len(ids))
			if err := v.compare(ctx, source, ids[start:end], &report.Sample); err != nil {
				return VerifyReport{}, err
			}
		}
	}

	if err := v.countSearchAndMap(ctx, &report); err != nil {
		return VerifyReport{}, err
	}

	if len(report.Sample.Examples) > options.Sample {
		report.Sample.Examples = report.Sample.Examples[:options.Sample]
	}
	return report, nil
}

// scopeClause bounds the legacy rows the backfill was ever going to look at.
// The job window moves with the clock, so a verification run days after the
// backfill judges a slightly smaller slice; anything that fell out of it is
// terminal history nobody polls, which is why the backfill skipped it too.
const scopeClause = ` ($2::timestamptz IS NULL OR l.created_at >= $2)`

func (v *Verifier) scope(source legacySource) any {
	if !source.windowed {
		return nil
	}
	return time.Now().Add(-jobWindow)
}

// predicates builds the SQL fragments one table is counted with. Each is a
// predicate over the legacy row l or the audit row a.
type predicates struct {
	legacy string
	scope  string
	// migrated is whether a user Go has served owns the legacy row, and
	// migratedScope is the rows scope leaves out for that reason.
	migrated      string
	migratedScope string
	key           string
	unaudited     string
	skipped       string
	missing       string
	owned         string
}

func (v *Verifier) predicates(source legacySource) predicates {
	shape := v.source.shape(source.name)
	built := predicates{
		legacy: v.source.table(source.name),
		key:    keyExpression(source.name, source.key, shape),
	}
	// Only the windowed table has to have a created_at. Every other scope
	// still names $2, so the statements take the same arguments.
	built.scope = ` $2::timestamptz IS NULL`
	if source.windowed {
		built.scope = scopeClause
	}
	built.scope += shape.environmentFilter()
	built.migratedScope = ` false`
	if source.owner != "" {
		built.migrated = migrated(source.owner)
		built.migratedScope = built.scope + ` AND ` + built.migrated
		built.scope += ` AND NOT ` + built.migrated
	}
	audited := `SELECT 1 FROM reelpin.backfill_audit a
			WHERE a.backfill_version = $1 AND a.source_table = '` + source.name + `'
			  AND a.source_id = ` + built.key
	built.unaudited = `NOT EXISTS (` + audited + `)`
	built.skipped = `EXISTS (` + audited + ` AND a.action LIKE 'skipped%')`

	if source.canonicalKey != "" {
		built.missing = `NOT EXISTS (SELECT 1 FROM ` + source.canonical + ` c
			WHERE c.` + source.canonicalKey + ` = a.source_id)`
		if built.migrated != "" {
			built.missing += ` AND NOT EXISTS (SELECT 1 FROM ` + built.legacy + ` l
				WHERE ` + built.key + ` = a.source_id AND ` + built.migrated + `)`
		}
	}

	switch {
	case source.owned != "":
		built.owned = source.owned
		built.missing = `NOT EXISTS (SELECT 1 FROM ` + built.legacy + ` l
			WHERE ` + built.key + ` = a.source_id AND (` + source.owned + ` OR ` + built.migrated + `))
			AND NOT ` + noteOwnerMigrated
	case source.owner != "":
		built.owned = `EXISTS (SELECT 1 FROM ` + source.canonical + ` c
			WHERE c.` + source.canonicalKey + ` = ` + built.key + `
			  AND c.user_id::text = lower(l.` + source.owner + `::text))`
	default:
		built.owned = `EXISTS (SELECT 1 FROM ` + source.canonical + ` c
			WHERE c.` + source.canonicalKey + ` = ` + built.key + `)`
	}
	return built
}

func (v *Verifier) countTable(ctx context.Context, source legacySource, options VerifyOptions) (TableVerification, error) {
	counted := TableVerification{SourceTable: source.name, SkippedByAction: map[string]int{}}
	cutoff := v.scope(source)
	sql := v.predicates(source)

	missing := `a.backfill_version = $1 AND a.source_table = '` + source.name + `'
		  AND a.action = '` + source.carried + `' AND ` + sql.missing

	if err := v.pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM `+sql.legacy+` l WHERE`+sql.scope+`),
			(SELECT count(*) FROM `+sql.legacy+` l WHERE`+sql.scope+` AND `+sql.unaudited+`),
			(SELECT count(*) FROM reelpin.backfill_audit a WHERE `+missing+`),
			(SELECT count(*) FROM `+sql.legacy+` l WHERE`+sql.scope+` AND NOT `+sql.skipped+`),
			(SELECT count(*) FROM `+sql.legacy+` l WHERE`+sql.scope+` AND NOT `+sql.skipped+` AND `+sql.owned+`),
			(SELECT count(*) FROM `+source.canonical+`),
			(SELECT count(*) FROM `+sql.legacy+` l WHERE`+sql.migratedScope+`)`,
		Version, cutoff,
	).Scan(&counted.InScope, &counted.Unexplained, &counted.CarriedButMissing,
		&counted.Expected, &counted.Present, &counted.CanonicalTotal, &counted.Migrated); err != nil {
		return TableVerification{}, fmt.Errorf("counting %s: %w", source.name, err)
	}

	rows, err := v.pool.Query(ctx, `
		SELECT action, count(*) FROM reelpin.backfill_audit
		WHERE backfill_version = $1 AND source_table = $2
		GROUP BY action`, Version, source.name)
	if err != nil {
		return TableVerification{}, fmt.Errorf("counting %s decisions: %w", source.name, err)
	}
	defer rows.Close()
	for rows.Next() {
		var action string
		var total int
		if err := rows.Scan(&action, &total); err != nil {
			return TableVerification{}, fmt.Errorf("counting %s decisions: %w", source.name, err)
		}
		switch {
		case action == source.carried:
			counted.Carried = total - counted.CarriedButMissing
		case strings.HasPrefix(action, "skipped"):
			counted.Skipped += total
			counted.SkippedByAction[action] = total
		}
	}
	if rows.Err() != nil {
		return TableVerification{}, fmt.Errorf("counting %s decisions: %w", source.name, rows.Err())
	}

	counted.UnexplainedIDs, err = v.ids(ctx, `
		SELECT (`+sql.key+`)::text FROM `+sql.legacy+` l
		WHERE`+sql.scope+` AND `+sql.unaudited+`
		ORDER BY md5((`+sql.key+`)::text) LIMIT $3`, Version, cutoff, options.Sample)
	if err != nil {
		return TableVerification{}, err
	}
	counted.MissingIDs, err = v.ids(ctx, `
		SELECT a.source_id::text FROM reelpin.backfill_audit a
		WHERE `+missing+`
		ORDER BY md5(a.source_id::text) LIMIT $2`, Version, options.Sample)
	if err != nil {
		return TableVerification{}, err
	}

	if source.owner == "" {
		return counted, nil
	}
	counted.UserDiffs, counted.UsersDiffering, err = v.userDiffs(ctx, source, sql, cutoff, options.Sample)
	if err != nil {
		return TableVerification{}, err
	}
	return counted, nil
}

// countUsers accounts for the users step, which keeps no audit: it upserts
// every legacy user on every run except those Go deleted, so any other legacy
// user with no auth.users row is unexplained. Users Go has served stay in
// scope, because the step does not leave them out either.
func (v *Verifier) countUsers(ctx context.Context, options VerifyOptions) (TableVerification, error) {
	counted := TableVerification{SourceTable: sourceUsers, SkippedByAction: map[string]int{}}
	legacy := v.source.table(sourceUsers)
	missing := `NOT EXISTS (SELECT 1 FROM auth.users u WHERE u.id = l.id)`
	if err := v.pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM `+legacy+` l WHERE `+notDeleted+`),
			(SELECT count(*) FROM `+legacy+` l WHERE NOT `+missing+` AND `+notDeleted+`),
			(SELECT count(*) FROM auth.users)`,
	).Scan(&counted.InScope, &counted.Present, &counted.CanonicalTotal); err != nil {
		return TableVerification{}, fmt.Errorf("counting %s: %w", sourceUsers, err)
	}
	counted.Expected = counted.InScope
	counted.Carried = counted.Present
	counted.Unexplained = counted.InScope - counted.Present

	var err error
	counted.UnexplainedIDs, err = v.ids(ctx, `
		SELECT l.id::text FROM `+legacy+` l
		WHERE `+missing+` AND `+notDeleted+`
		ORDER BY md5(l.id::text) LIMIT $1`, options.Sample)
	if err != nil {
		return TableVerification{}, err
	}
	return counted, nil
}

// userDiffs is the per-user half of the count: a table can balance in total
// while one user lost rows and another gained them.
func (v *Verifier) userDiffs(
	ctx context.Context,
	source legacySource,
	sql predicates,
	cutoff any,
	limit int,
) ([]UserDiff, int, error) {
	rows, err := v.pool.Query(ctx, `
		SELECT owner, expected, present, count(*) OVER ()
		FROM (
			SELECT lower(l.`+source.owner+`::text) AS owner,
			       count(*) FILTER (WHERE NOT `+sql.skipped+`) AS expected,
			       count(*) FILTER (WHERE NOT `+sql.skipped+` AND `+sql.owned+`) AS present
			FROM `+sql.legacy+` l
			WHERE`+sql.scope+`
			GROUP BY 1
		) per_user
		WHERE expected <> present
		ORDER BY owner
		LIMIT $3`, Version, cutoff, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("counting %s per user: %w", source.name, err)
	}
	defer rows.Close()

	diffs := []UserDiff{}
	differing := 0
	for rows.Next() {
		var diff UserDiff
		if err := rows.Scan(&diff.UserID, &diff.Legacy, &diff.Canonical, &differing); err != nil {
			return nil, 0, fmt.Errorf("counting %s per user: %w", source.name, err)
		}
		diffs = append(diffs, diff)
	}
	return diffs, differing, rows.Err()
}

// countSearchAndMap counts what search and the map would lose. Both are
// measured over the legacy reels that were carried, because a reel that was
// deliberately skipped takes its embedding and its places with it.
func (v *Verifier) countSearchAndMap(ctx context.Context, report *VerifyReport) error {
	shape := v.source.shape(sourceReels)
	sql := v.predicates(legacySource{name: sourceReels, key: "id"})
	carried := sql.legacy + ` l
		JOIN reelpin.user_saves s ON s.id = l.id
		JOIN reelpin.contents c ON c.id = s.content_id`
	where := ` WHERE` + sql.scope + ` AND NOT ` + sql.skipped

	if shape.has("embedding") {
		report.Embeddings.Measured = true
		if err := v.pool.QueryRow(ctx, `
			SELECT
				(SELECT count(*) FROM `+sql.legacy+` l`+where+` AND l.embedding IS NOT NULL),
				(SELECT count(*) FROM `+carried+`
				 JOIN reelpin.content_embeddings e
				   ON e.content_version_id = c.current_version_id AND e.embedding IS NOT NULL
				 `+where+` AND l.embedding IS NOT NULL)`,
			Version, nil,
		).Scan(&report.Embeddings.Legacy, &report.Embeddings.Canonical); err != nil {
			return fmt.Errorf("counting embedding coverage: %w", err)
		}
		if err := v.embeddingModels(ctx, carried, where, report); err != nil {
			return err
		}
	}

	if !shape.has("locations") {
		return nil
	}
	if err := v.pool.QueryRow(ctx, `
		SELECT
			(SELECT coalesce(sum((
				SELECT count(*) FROM jsonb_array_elements(
					CASE WHEN jsonb_typeof(l.locations) = 'array' THEN l.locations ELSE '[]'::jsonb END) AS p(entry)
				WHERE `+locatedEntry+`
			)), 0) FROM `+sql.legacy+` l`+where+`),
			(SELECT count(*) FROM `+carried+`
			 JOIN reelpin.content_locations cl ON cl.content_version_id = c.current_version_id
			 `+where+`)`,
		Version, nil,
	).Scan(&report.MapPoints.LegacyReelPoints, &report.MapPoints.CanonicalReelPoints); err != nil {
		return fmt.Errorf("counting map points: %w", err)
	}
	return nil
}

// embeddingModels names the model behind every vector Canonical counted, so a
// carried model the hosts are not configured to search with shows up here
// rather than as search quietly returning nothing.
func (v *Verifier) embeddingModels(ctx context.Context, carried, where string, report *VerifyReport) error {
	rows, err := v.pool.Query(ctx, `
		SELECT e.model, count(*) FROM `+carried+`
		JOIN reelpin.content_embeddings e
		  ON e.content_version_id = c.current_version_id AND e.embedding IS NOT NULL
		`+where+` AND l.embedding IS NOT NULL
		GROUP BY e.model`, Version, nil)
	if err != nil {
		return fmt.Errorf("counting embedding models: %w", err)
	}
	defer rows.Close()

	report.Embeddings.Models = map[string]int{}
	for rows.Next() {
		var model string
		var total int
		if err := rows.Scan(&model, &total); err != nil {
			return fmt.Errorf("counting embedding models: %w", err)
		}
		report.Embeddings.Models[model] = total
	}
	return rows.Err()
}

func (v *Verifier) ids(ctx context.Context, statement string, args ...any) ([]string, error) {
	rows, err := v.pool.Query(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("listing source ids: %w", err)
	}
	defer rows.Close()

	collected := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("listing source ids: %w", err)
		}
		collected = append(collected, id)
	}
	return collected, rows.Err()
}

// sampleIDs picks the rows to compare field by field. It orders by the md5 of
// the source id rather than random(): two runs against the same database have
// to compare the same rows, or a report cannot be read against the one before
// it that found the problem. Ordering by the id itself would be deterministic
// too, but it takes one contiguous stretch of the keyspace, and the sample is
// worth more spread across the table. A user Go has served may have changed
// their row since it was carried, so theirs are not sampled.
func (v *Verifier) sampleIDs(ctx context.Context, source legacySource, limit int) ([]string, error) {
	return v.ids(ctx, `
		SELECT a.source_id::text FROM reelpin.backfill_audit a
		JOIN `+source.canonical+` c ON c.id = a.source_id
		WHERE a.backfill_version = $1 AND a.source_table = '`+source.name+`'
		  AND a.action = '`+source.carried+`'
		  AND NOT EXISTS (SELECT 1 FROM reelpin.user_migrations m WHERE m.user_id = c.user_id)
		ORDER BY md5(a.source_id::text) LIMIT $2`, Version, limit)
}

func (v *Verifier) compare(ctx context.Context, source legacySource, ids []string, report *SampleReport) error {
	if source.name == sourceJobs {
		return v.compareJobs(ctx, ids, report)
	}
	return v.compareReels(ctx, ids, report)
}

// canonicalSave is the canonical side of one carried-over legacy reel.
type canonicalSave struct {
	UserID          string
	SavedAt         time.Time
	Platform        string
	SourceContentID *string
	ScopeHash       string
	VersionID       *string
	Title           string
	Summary         string
	Transcript      string
	Status          string
	Category        *string
	// Saves is how many users hold this content.
	Saves int
	// MergedAt is the newest created_at of this user's legacy reels of the
	// same content that went into this save as a conflict instead of becoming
	// a save of their own. A newer one moved saved_at forward.
	MergedAt *time.Time
}

func (v *Verifier) compareReels(ctx context.Context, ids []string, report *SampleReport) error {
	found := map[string]canonicalSave{}
	rows, err := v.pool.Query(ctx, `
		SELECT s.id::text, s.user_id::text, s.saved_at, c.source_platform, c.source_content_id,
		       c.access_scope_hash, v.id::text,
		       coalesce(v.title, ''), coalesce(v.summary, ''), coalesce(v.transcript, ''),
		       coalesce(v.extraction_status, ''), v.raw_extraction->>'category',
		       (SELECT count(*) FROM reelpin.user_saves o WHERE o.content_id = s.content_id),
		       (SELECT max(m.created_at) FROM `+v.source.table(sourceReels)+` m
		        JOIN reelpin.backfill_audit a
		          ON a.backfill_version = $2 AND a.source_table = '`+sourceReels+`'
		         AND a.source_id = m.id AND a.action = 'skipped_conflict' AND a.content_id = s.content_id
		        WHERE lower(m.user_id::text) = s.user_id::text)
		FROM reelpin.user_saves s
		JOIN reelpin.contents c ON c.id = s.content_id
		LEFT JOIN reelpin.content_versions v ON v.id = c.current_version_id
		WHERE s.id = ANY($1::uuid[])`, ids, Version)
	if err != nil {
		return fmt.Errorf("reading the canonical saves: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var save canonicalSave
		if err := rows.Scan(&id, &save.UserID, &save.SavedAt, &save.Platform, &save.SourceContentID,
			&save.ScopeHash, &save.VersionID, &save.Title, &save.Summary, &save.Transcript,
			&save.Status, &save.Category, &save.Saves, &save.MergedAt); err != nil {
			return fmt.Errorf("reading the canonical saves: %w", err)
		}
		found[id] = save
	}
	if rows.Err() != nil {
		return fmt.Errorf("reading the canonical saves: %w", rows.Err())
	}

	legacy, err := v.readReelsByID(ctx, ids)
	if err != nil {
		return err
	}
	for _, row := range legacy {
		save, ok := found[row.ID]
		if !ok {
			// Counted as carried_but_missing already; nothing to compare.
			continue
		}
		if err := v.compareReel(ctx, row, save, report); err != nil {
			return err
		}
	}
	return nil
}

// compareReel checks the fields a wrong value would actually break: who owns
// the save, where it sits in the feed, the identity two users deduplicate on,
// and the text the detail view renders. Tags, key facts, locations, the caption
// and the thumbnail are left out on purpose. They feed search, the map and a
// card image, all of which reprocessing rebuilds, and none of which makes a
// save unreadable or shows it to the wrong person.
func (v *Verifier) compareReel(ctx context.Context, legacy reelRow, save canonicalSave, report *SampleReport) error {
	report.Compared++

	if save.UserID != legacy.UserID {
		report.differ(sourceReels, legacy.ID, "user_saves.user_id")
	}
	// A legacy row with no created_at was saved at the backfill's clock, which
	// is not something to compare against.
	if legacy.CreatedAt != nil {
		want := *legacy.CreatedAt
		if save.MergedAt != nil && save.MergedAt.After(want) {
			want = *save.MergedAt
		}
		if !save.SavedAt.Equal(want) {
			report.differ(sourceReels, legacy.ID, "user_saves.saved_at")
		}
	}

	identity, scopeHash, err := identify(ctx, v.resolver, legacy.URL, legacy.NormalizedURL, legacy.UserID)
	if err != nil {
		// It resolved once, or it would never have been carried over.
		report.differ(sourceReels, legacy.ID, "contents.source_identity")
		return nil
	}
	if save.Platform != identity.Platform {
		report.differ(sourceReels, legacy.ID, "contents.source_platform")
	}
	if text(save.SourceContentID) != identity.ContentID {
		report.differ(sourceReels, legacy.ID, "contents.source_content_id")
	}
	if save.ScopeHash != scopeHash {
		report.differ(sourceReels, legacy.ID, "contents.access_scope_hash")
	}

	if save.VersionID == nil {
		report.differ(sourceReels, legacy.ID, "contents.current_version_id")
		return nil
	}
	// A content several users saved carries one version, built from whichever
	// of their legacy rows the backfill reached first, and nothing records
	// which one that was. Its text is counted rather than guessed at.
	if save.Saves != 1 {
		report.TextNotComparable++
		return nil
	}

	extraction, err := legacy.extraction()
	if err != nil {
		report.differ(sourceReels, legacy.ID, "reels.json")
		return nil
	}
	cached, err := readCache(ctx, v.pool, v.source.table(sourceCache), identity)
	if err != nil {
		return err
	}
	expected, err := versionFields(legacy, extraction, cached)
	if err != nil {
		report.differ(sourceReels, legacy.ID, "processing_cache.extracted_data")
		return nil
	}

	for _, field := range []struct{ name, want, got string }{
		{"content_versions.title", expected.Extraction.Title, save.Title},
		{"content_versions.summary", expected.Extraction.Summary, save.Summary},
		{"content_versions.transcript", expected.Transcript, save.Transcript},
		{"content_versions.extraction_status", extractionStatus(legacy.ParseStatus), save.Status},
		{"content_versions.raw_extraction.category", text(legacy.Category), text(save.Category)},
	} {
		if field.want != field.got {
			report.differ(sourceReels, legacy.ID, field.name)
		}
	}
	return nil
}

func (v *Verifier) readReelsByID(ctx context.Context, ids []string) ([]reelRow, error) {
	rows, err := v.pool.Query(ctx, `
		SELECT `+reelColumns+`
		FROM `+v.source.table(sourceReels)+` l
		WHERE l.id = ANY($1::uuid[])
		ORDER BY l.id`, ids)
	if err != nil {
		return nil, fmt.Errorf("reading sampled legacy reels: %w", err)
	}
	defer rows.Close()

	collected := []reelRow{}
	for rows.Next() {
		row, err := scanReel(rows)
		if err != nil {
			return nil, fmt.Errorf("reading sampled legacy reels: %w", err)
		}
		collected = append(collected, row)
	}
	return collected, rows.Err()
}

// compareJobs checks what the app polls a job for: whether it may see the job
// at all, whether the job is finished, and what to show if it failed. The run
// linkage and the timestamps are left out; a wrong one is a wrong history, not
// a job that never resolves.
func (v *Verifier) compareJobs(ctx context.Context, ids []string, report *SampleReport) error {
	rows, err := v.pool.Query(ctx, `
		SELECT l.id::text, l.user_id::text, l.url, l.status, coalesce(l.failure_code, ''),
		       c.user_id::text, c.url, c.status, coalesce(c.failure_code, '')
		FROM `+v.source.table(sourceJobs)+` l
		JOIN reelpin.processing_jobs c ON c.id = l.id
		WHERE l.id = ANY($1::uuid[])
		ORDER BY l.id`, ids)
	if err != nil {
		return fmt.Errorf("reading the canonical jobs: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id, legacyUser, legacyURL, legacyStatus, legacyFailure string
		var user, url, status, failure string
		if err := rows.Scan(&id, &legacyUser, &legacyURL, &legacyStatus, &legacyFailure,
			&user, &url, &status, &failure); err != nil {
			return fmt.Errorf("reading the canonical jobs: %w", err)
		}
		report.Compared++

		for _, field := range []struct{ name, want, got string }{
			{"processing_jobs.user_id", legacyUser, user},
			{"processing_jobs.url", legacyURL, url},
			{"processing_jobs.status", jobStatus(legacyStatus), status},
			{"processing_jobs.failure_code", legacyFailure, failure},
		} {
			if field.want != field.got {
				report.differ(sourceJobs, id, field.name)
			}
		}
	}
	return rows.Err()
}
