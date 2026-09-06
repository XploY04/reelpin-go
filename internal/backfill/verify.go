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
	// by field. 0 takes the default.
	Sample int
	// BatchSize bounds how many sampled rows are read per query.
	BatchSize int
}

// TableVerification is one legacy table's account of itself. Every row in scope
// belongs in exactly one of Carried, Skipped, CarriedButMissing or Unexplained.
type TableVerification struct {
	SourceTable string `json:"source_table"`
	InScope     int    `json:"in_scope"`
	Carried     int    `json:"carried"`
	Skipped     int    `json:"skipped"`
	// SkippedByAction breaks the skips down by the reason the backfill
	// recorded: skipped_invalid_url, skipped_unreadable, skipped_blocklisted,
	// skipped_active, skipped_ambiguous.
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

// VerifyReport is what a runbook reads after a rehearsal backfill.
type VerifyReport struct {
	Tables []TableVerification `json:"tables"`
	Sample SampleReport        `json:"sample"`
}

// OK is the single condition to key off: every legacy row in scope was either
// carried over or deliberately skipped, every carried row is really there, and
// nothing in the sample differs.
func (r VerifyReport) OK() bool {
	for _, table := range r.Tables {
		if table.Unexplained > 0 || table.CarriedButMissing > 0 {
			return false
		}
	}
	return r.Sample.Mismatches == 0
}

// legacySource is the only thing the two legacy tables differ by, so the
// counting SQL is written once. Every field here is a compile-time constant,
// never anything a caller supplies.
type legacySource struct {
	name      string
	legacy    string
	canonical string
	carried   string
	// windowed marks a table the backfill only reads a recent slice of.
	windowed bool
}

var verifySources = []legacySource{
	{name: sourceReels, legacy: "public.reels", canonical: "reelpin.user_saves", carried: "saved"},
	{
		name:      sourceJobs,
		legacy:    "public.processing_jobs",
		canonical: "reelpin.processing_jobs",
		carried:   "copied",
		windowed:  true,
	},
}

// Verifier reads the backfill's bookkeeping back against both schemas and says
// whether the carry-over can be signed off. Every statement it runs is a
// SELECT: the rehearsal it judges has to survive being judged.
type Verifier struct {
	pool     *pgxpool.Pool
	resolver *sourceidentity.Resolver
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

	report := VerifyReport{}
	for _, source := range verifySources {
		counted, err := v.countTable(ctx, source, options)
		if err != nil {
			return VerifyReport{}, err
		}
		report.Tables = append(report.Tables, counted)

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

func (v *Verifier) countTable(ctx context.Context, source legacySource, options VerifyOptions) (TableVerification, error) {
	counted := TableVerification{SourceTable: source.name, SkippedByAction: map[string]int{}}
	cutoff := v.scope(source)

	unaudited := `NOT EXISTS (SELECT 1 FROM reelpin.backfill_audit a
			WHERE a.backfill_version = $1 AND a.source_table = '` + source.name + `'
			  AND a.source_id = l.id)`
	missing := `a.backfill_version = $1 AND a.source_table = '` + source.name + `'
		  AND a.action = '` + source.carried + `'
		  AND NOT EXISTS (SELECT 1 FROM ` + source.canonical + ` c WHERE c.id = a.source_id)`

	if err := v.pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM `+source.legacy+` l WHERE`+scopeClause+`),
			(SELECT count(*) FROM `+source.legacy+` l WHERE`+scopeClause+` AND `+unaudited+`),
			(SELECT count(*) FROM reelpin.backfill_audit a WHERE `+missing+`)`,
		Version, cutoff,
	).Scan(&counted.InScope, &counted.Unexplained, &counted.CarriedButMissing); err != nil {
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
		SELECT l.id::text FROM `+source.legacy+` l
		WHERE`+scopeClause+` AND `+unaudited+`
		ORDER BY md5(l.id::text) LIMIT $3`, Version, cutoff, options.Sample)
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
	return counted, nil
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
// worth more spread across the table.
func (v *Verifier) sampleIDs(ctx context.Context, source legacySource, limit int) ([]string, error) {
	return v.ids(ctx, `
		SELECT a.source_id::text FROM reelpin.backfill_audit a
		JOIN `+source.canonical+` c ON c.id = a.source_id
		WHERE a.backfill_version = $1 AND a.source_table = '`+source.name+`'
		  AND a.action = '`+source.carried+`'
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
}

func (v *Verifier) compareReels(ctx context.Context, ids []string, report *SampleReport) error {
	found := map[string]canonicalSave{}
	rows, err := v.pool.Query(ctx, `
		SELECT s.id::text, s.user_id::text, s.saved_at, c.source_platform, c.source_content_id,
		       c.access_scope_hash, v.id::text,
		       coalesce(v.title, ''), coalesce(v.summary, ''), coalesce(v.transcript, ''),
		       coalesce(v.extraction_status, ''), v.raw_extraction->>'category',
		       (SELECT count(*) FROM reelpin.user_saves o WHERE o.content_id = s.content_id)
		FROM reelpin.user_saves s
		JOIN reelpin.contents c ON c.id = s.content_id
		LEFT JOIN reelpin.content_versions v ON v.id = c.current_version_id
		WHERE s.id = ANY($1::uuid[])`, ids)
	if err != nil {
		return fmt.Errorf("reading the canonical saves: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var save canonicalSave
		if err := rows.Scan(&id, &save.UserID, &save.SavedAt, &save.Platform, &save.SourceContentID,
			&save.ScopeHash, &save.VersionID, &save.Title, &save.Summary, &save.Transcript,
			&save.Status, &save.Category, &save.Saves); err != nil {
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
	if legacy.CreatedAt != nil && !save.SavedAt.Equal(*legacy.CreatedAt) {
		report.differ(sourceReels, legacy.ID, "user_saves.saved_at")
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
	cached, err := readCache(ctx, v.pool, identity)
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
		FROM public.reels l
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
		FROM public.processing_jobs l
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
