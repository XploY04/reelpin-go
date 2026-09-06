// Package backfill copies what the Python service left behind into the
// canonical schema. The legacy tables are a read-only source: nothing here
// writes to public.reels, public.processing_jobs or public.processing_cache, so
// the Python service keeps serving unchanged while this runs.
//
// Existing identifiers are preserved rather than reissued. A legacy reel id
// becomes the id of its reelpin.user_saves row, which is the public reel id,
// and a legacy job id becomes the id of its reelpin.processing_jobs row. Every
// id the app already holds keeps working.
package backfill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/ai"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Version names this backfill in the progress and audit tables. Bump it only
// for a genuinely different pass; resuming depends on it staying stable.
const Version = "legacy-content-v1"

// advisoryLockKey keeps two copies of the backfill from running at once.
// Migrations hold 774_120_001.
const advisoryLockKey int64 = 774_120_002

// jobWindow bounds how far back legacy jobs are copied. Anything older is
// terminal history nobody polls.
const jobWindow = 60 * 24 * time.Hour

// legacyVersion labels everything produced before the Go pipeline existed. It
// is deliberately not enqueue.ProcessorVersion: a run recorded under the
// current processor would tell the pipeline this content is already extracted
// to today's standard, and it is not.
const legacyVersion = "python-legacy"

const (
	sourceReels = "reels"
	sourceJobs  = "processing_jobs"
)

// blocklistViolation is the SQLSTATE the blocklist trigger raises. A purged
// source appearing in the legacy tables is expected, not a reason to stop.
const blocklistViolation = "23514"

type Options struct {
	// Execute switches from a dry run to real writes. Off by default.
	Execute bool
	// BatchSize bounds one keyset page.
	BatchSize int
	// MaxRows stops the reel pass early, for a rehearsal on a big table.
	MaxRows int
}

// Report counts what happened. It carries no content and no user data.
type Report struct {
	ReelsScanned      int `json:"reels_scanned"`
	UniqueContent     int `json:"unique_content"`
	ContentVersions   int `json:"content_versions"`
	CacheHits         int `json:"cache_hits"`
	SavesCreated      int `json:"saves_created"`
	SavesAlreadyThere int `json:"saves_already_there"`
	InvalidURLs       int `json:"invalid_urls"`
	Unreadable        int `json:"unreadable"`
	Blocklisted       int `json:"blocklisted"`
	Conflicts         int `json:"conflicts"`
	Failures          int `json:"failures"`

	JobsScanned      int `json:"jobs_scanned"`
	JobsCreated      int `json:"jobs_created"`
	JobsAlreadyThere int `json:"jobs_already_there"`
	JobsUncertain    int `json:"jobs_uncertain"`
	RunsCreated      int `json:"runs_created"`
}

type Backfiller struct {
	pool     *pgxpool.Pool
	logger   *slog.Logger
	resolver *sourceidentity.Resolver
	// seen dedupes identities during a dry run, which writes nothing and so
	// cannot ask the database what it already created. It counts what this
	// pass would build from an empty canonical schema.
	seen map[string]bool
}

func New(pool *pgxpool.Pool, logger *slog.Logger) *Backfiller {
	// Identity resolution stays offline: a backfill must never depend on a
	// provider being reachable, and an unresolvable row is reported instead.
	return &Backfiller{pool: pool, logger: logger, resolver: &sourceidentity.Resolver{}}
}

// Run copies reels and then jobs, resuming from the recorded cursor. Jobs run
// second because a job is only copied once its content exists.
func (b *Backfiller) Run(ctx context.Context, options Options) (Report, error) {
	if options.BatchSize <= 0 {
		options.BatchSize = 500
	}
	b.seen = map[string]bool{}

	release, err := b.lock(ctx)
	if err != nil {
		return Report{}, err
	}
	defer release()

	report := Report{}
	if err := b.backfillReels(ctx, options, &report); err != nil {
		return report, err
	}
	// A rehearsal that stopped part-way through the reels has no business
	// judging jobs: every job past the cursor would be recorded as having no
	// content, which is a decision about the rehearsal, not about the job.
	if options.MaxRows > 0 && report.ReelsScanned >= options.MaxRows {
		return report, nil
	}
	if err := b.copyJobs(ctx, options, &report); err != nil {
		return report, err
	}
	return report, nil
}

func (b *Backfiller) lock(ctx context.Context) (func(), error) {
	conn, err := b.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquiring a connection for the lock: %w", err)
	}

	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, advisoryLockKey).Scan(&acquired); err != nil {
		conn.Release()
		return nil, fmt.Errorf("taking the backfill lock: %w", err)
	}
	if !acquired {
		conn.Release()
		return nil, errors.New("another backfill is already running")
	}

	return func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, advisoryLockKey)
		conn.Release()
	}, nil
}

type reelRow struct {
	ID                  string
	UserID              string
	URL                 string
	NormalizedURL       *string
	ThumbnailURL        *string
	Title               string
	Summary             string
	Transcript          string
	Category            *string
	Subcategory         *string
	SecondaryCategories json.RawMessage
	KeyFacts            json.RawMessage
	Locations           json.RawMessage
	PeopleMentioned     json.RawMessage
	ActionableItems     json.RawMessage
	Events              json.RawMessage
	ParseStatus         *string
	CreatedAt           *time.Time
}

func (b *Backfiller) backfillReels(ctx context.Context, options Options, report *Report) error {
	cursor, err := b.cursor(ctx, sourceReels)
	if err != nil {
		return err
	}

	batch := int64(0)
	for {
		rows, err := b.readReels(ctx, cursor, options.BatchSize)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return b.finishCursor(ctx, options, sourceReels)
		}
		batch++

		for _, row := range rows {
			cursor = row.ID
			report.ReelsScanned++
			if err := b.backfillOneReel(ctx, options, batch, row, report); err != nil {
				return err
			}
			if options.MaxRows > 0 && report.ReelsScanned >= options.MaxRows {
				return b.saveCursor(ctx, options, sourceReels, cursor, report.ReelsScanned)
			}
		}

		if err := b.saveCursor(ctx, options, sourceReels, cursor, report.ReelsScanned); err != nil {
			return err
		}
	}
}

func (b *Backfiller) backfillOneReel(ctx context.Context, options Options, batch int64, row reelRow, report *Report) error {
	// The save carries the legacy reel id, so its presence is what "already
	// done" means. Nothing on the legacy row records that.
	done, err := b.exists(ctx, "reelpin.user_saves", row.ID)
	if err != nil {
		return err
	}
	if done {
		report.SavesAlreadyThere++
		return nil
	}

	identity, scopeHash, err := b.identify(ctx, row.URL, row.NormalizedURL, row.UserID)
	if err != nil {
		report.InvalidURLs++
		return b.audit(ctx, options, batch, sourceReels, row.ID, "skipped_invalid_url", nil, nil, nil,
			"identity could not be derived")
	}

	extraction, err := row.extraction()
	if err != nil {
		report.Unreadable++
		return b.audit(ctx, options, batch, sourceReels, row.ID, "skipped_unreadable", nil, nil, nil,
			"legacy json could not be read: "+err.Error())
	}

	if !options.Execute {
		return b.reportOneReel(ctx, identity, scopeHash, report)
	}

	transaction, err := b.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("starting the reel transaction: %w", err)
	}
	defer transaction.Rollback(ctx)

	contentID, versionID, created, err := findOrCreateContent(ctx, transaction, identity, scopeHash)
	if err != nil {
		if isBlocklisted(err) {
			// The failed insert aborted the transaction, so it has to go
			// before the audit write, which runs on its own connection.
			_ = transaction.Rollback(ctx)
			report.Blocklisted++
			return b.audit(ctx, options, batch, sourceReels, row.ID, "skipped_blocklisted", nil, nil, nil,
				"the source has been purged and blocklisted")
		}
		report.Failures++
		return err
	}
	if created {
		report.UniqueContent++
	}

	// A second user's save of the same content reuses the version the first
	// one built. Only the first save of a content pays for reading the cache.
	if versionID == "" {
		cached, err := readCache(ctx, transaction, identity)
		if err != nil {
			return err
		}
		if cached != nil {
			report.CacheHits++
		}
		versionID, err = insertVersion(ctx, transaction, contentID, row, extraction, cached)
		if err != nil {
			report.Failures++
			return err
		}
		report.ContentVersions++
	}

	tag, err := transaction.Exec(ctx, `
		INSERT INTO reelpin.user_saves (id, user_id, content_id, saved_at)
		VALUES ($1, $2, $3, coalesce($4, now()))
		ON CONFLICT DO NOTHING`,
		row.ID, row.UserID, contentID, row.CreatedAt)
	if err != nil {
		report.Failures++
		return fmt.Errorf("creating the save: %w", err)
	}
	if tag.RowsAffected() == 1 {
		report.SavesCreated++
	} else {
		// The same user saved this content twice under two legacy rows. One
		// save survives; the other is reported rather than forced in.
		report.Conflicts++
	}

	if err := auditTx(ctx, transaction, batch, sourceReels, row.ID, "saved", &contentID, &versionID, nil, ""); err != nil {
		return err
	}
	return transaction.Commit(ctx)
}

// reportOneReel is the dry run: it reads what a real run would read and counts
// what it would write, without a transaction and without a cursor.
func (b *Backfiller) reportOneReel(ctx context.Context, identity sourceidentity.SourceIdentity, scopeHash string, report *Report) error {
	report.SavesCreated++

	key := identityKey(identity, scopeHash)
	if b.seen[key] {
		return nil
	}
	b.seen[key] = true
	report.UniqueContent++
	report.ContentVersions++

	cached, err := readCache(ctx, b.pool, identity)
	if err != nil {
		return err
	}
	if cached != nil {
		report.CacheHits++
	}
	return nil
}

// identify derives the global identity and the access scope one legacy row
// belongs in. A generic link stays fenced to the user who saved it, exactly as
// a fresh submission of the same link would be.
func (b *Backfiller) identify(ctx context.Context, rawURL string, normalized *string, userID string) (sourceidentity.SourceIdentity, string, error) {
	sourceURL := strings.TrimSpace(rawURL)
	if sourceURL == "" && normalized != nil {
		sourceURL = strings.TrimSpace(*normalized)
	}
	identity, err := b.resolver.Resolve(ctx, sourceURL)
	if err != nil {
		return sourceidentity.SourceIdentity{}, "", err
	}
	scopeHash, err := identity.Scope.ForUser(userID).Hash()
	if err != nil {
		return sourceidentity.SourceIdentity{}, "", err
	}
	return identity, scopeHash, nil
}

func (b *Backfiller) readReels(ctx context.Context, cursor string, limit int) ([]reelRow, error) {
	rows, err := b.pool.Query(ctx, `
		SELECT id::text, user_id::text, url, normalized_url, thumbnail_url,
		       title, summary, transcript, category, subcategory,
		       secondary_categories, key_facts, locations, people_mentioned,
		       actionable_items, events, parse_status, created_at
		FROM public.reels
		WHERE ($1::uuid IS NULL OR id > $1::uuid)
		ORDER BY id
		LIMIT $2`, nullableUUID(cursor), limit)
	if err != nil {
		return nil, fmt.Errorf("reading legacy reels: %w", err)
	}
	defer rows.Close()

	collected := []reelRow{}
	for rows.Next() {
		var row reelRow
		var title, summary, transcript *string
		if err := rows.Scan(
			&row.ID, &row.UserID, &row.URL, &row.NormalizedURL, &row.ThumbnailURL,
			&title, &summary, &transcript, &row.Category, &row.Subcategory,
			&row.SecondaryCategories, &row.KeyFacts, &row.Locations, &row.PeopleMentioned,
			&row.ActionableItems, &row.Events, &row.ParseStatus, &row.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("reading legacy reels: %w", err)
		}
		row.Title, row.Summary, row.Transcript = text(title), text(summary), text(transcript)
		collected = append(collected, row)
	}
	return collected, rows.Err()
}

// extraction rebuilds the content-neutral half of a legacy row. The column
// names line up with ai.Extraction because both extractors were written to the
// same shape; anything that will not decode is reported, never dropped.
func (r reelRow) extraction() (ai.Extraction, error) {
	built := ai.Extraction{Title: r.Title, Summary: r.Summary}
	decoder := legacyDecoder{}
	decoder.into("secondary_categories", r.SecondaryCategories, &built.TopicalTags)
	decoder.into("key_facts", r.KeyFacts, &built.KeyFacts)
	decoder.into("locations", r.Locations, &built.Locations)
	decoder.into("people_mentioned", r.PeopleMentioned, &built.PeopleMentioned)
	decoder.into("actionable_items", r.ActionableItems, &built.ActionableItems)
	decoder.into("events", r.Events, &built.Events)
	return built, decoder.err
}

// legacyDecoder collects the first column that will not decode, so one bad
// blob names itself in the audit row instead of vanishing.
type legacyDecoder struct{ err error }

func (d *legacyDecoder) into(column string, raw json.RawMessage, target any) {
	if d.err != nil || len(raw) == 0 || string(raw) == "null" {
		return
	}
	if err := json.Unmarshal(raw, target); err != nil {
		d.err = fmt.Errorf("%s: %w", column, err)
	}
}

type cacheRow struct {
	Transcript    string
	Caption       string
	ThumbnailURL  *string
	ExtractedData json.RawMessage
}

func (c cacheRow) extraction() (ai.Extraction, error) {
	var built ai.Extraction
	decoder := legacyDecoder{}
	decoder.into("extracted_data", c.ExtractedData, &built)
	return built, decoder.err
}

// querier is the half of pgx.Tx and pgxpool.Pool the cache read needs, so a dry
// run can read it without a transaction.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// readCache prefers the global extraction Python already paid for over the
// per-user copy on the reel row.
func readCache(ctx context.Context, db querier, identity sourceidentity.SourceIdentity) (*cacheRow, error) {
	if identity.ContentID == "" {
		return nil, nil
	}

	var row cacheRow
	var transcript, caption *string
	err := db.QueryRow(ctx, `
		SELECT transcript, caption, thumbnail_url, extracted_data
		FROM public.processing_cache
		WHERE source_platform = $1 AND source_content_id = $2
		ORDER BY updated_at DESC NULLS LAST
		LIMIT 1`,
		identity.Platform, identity.ContentID,
	).Scan(&transcript, &caption, &row.ThumbnailURL, &row.ExtractedData)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the legacy processing cache: %w", err)
	}

	row.Transcript, row.Caption = text(transcript), text(caption)
	return &row, nil
}

// findOrCreateContent mirrors the submission path's lookup exactly. It does not
// retry a lost race: an error aborts the transaction, so the row is left for
// the next run rather than retried on a dead connection.
func findOrCreateContent(
	ctx context.Context,
	tx pgx.Tx,
	identity sourceidentity.SourceIdentity,
	scopeHash string,
) (string, string, bool, error) {
	urlHash := sourceidentity.URLHash(identity.NormalizedURL)

	var contentID string
	var versionID *string
	var err error
	if identity.ContentID != "" {
		err = tx.QueryRow(ctx, `
			SELECT id::text, current_version_id::text FROM reelpin.contents
			WHERE source_platform = $1 AND source_content_type = $2
			  AND source_content_id = $3 AND access_scope_hash = $4`,
			identity.Platform, identity.ContentType, identity.ContentID, scopeHash,
		).Scan(&contentID, &versionID)
	} else {
		err = tx.QueryRow(ctx, `
			SELECT id::text, current_version_id::text FROM reelpin.contents
			WHERE normalized_url_hash = $1 AND access_scope_hash = $2 AND source_content_id IS NULL`,
			urlHash, scopeHash,
		).Scan(&contentID, &versionID)
	}
	if err == nil {
		return contentID, text(versionID), false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, fmt.Errorf("finding the content: %w", err)
	}

	var sourceID any
	if identity.ContentID != "" {
		sourceID = identity.ContentID
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO reelpin.contents
			(source_platform, source_content_type, source_content_id,
			 normalized_url, normalized_url_hash, access_scope_hash)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id::text`,
		identity.Platform, identity.ContentType, sourceID,
		identity.NormalizedURL, urlHash, scopeHash,
	).Scan(&contentID); err != nil {
		return "", "", false, fmt.Errorf("creating the content: %w", err)
	}
	return contentID, "", true, nil
}

// insertVersion writes the one immutable version of this legacy content, in the
// same shape the Go pipeline writes: the extraction and the filed category ride
// raw_extraction, the thumbnail rides media.
func insertVersion(
	ctx context.Context,
	tx pgx.Tx,
	contentID string,
	row reelRow,
	extraction ai.Extraction,
	cached *cacheRow,
) (string, error) {
	transcript := row.Transcript
	caption := ""
	thumbnail := text(row.ThumbnailURL)

	if cached != nil {
		// The cache is the global copy Python already paid for; a reel row is
		// one user's view of it, so the cache wins wherever it has something.
		cachedExtraction, err := cached.extraction()
		if err != nil {
			return "", fmt.Errorf("reading the cached extraction: %w", err)
		}
		extraction = merge(extraction, cachedExtraction)
		if strings.TrimSpace(cached.Transcript) != "" {
			transcript = cached.Transcript
		}
		caption = cached.Caption
		if cached.ThumbnailURL != nil && strings.TrimSpace(*cached.ThumbnailURL) != "" {
			thumbnail = *cached.ThumbnailURL
		}
	}

	extraction = extraction.Normalize()
	raw, err := json.Marshal(map[string]any{
		"extraction":  extraction,
		"category":    text(row.Category),
		"subcategory": text(row.Subcategory),
	})
	if err != nil {
		return "", fmt.Errorf("encoding the extraction: %w", err)
	}
	media, err := json.Marshal(map[string]any{"thumbnail_url": thumbnail})
	if err != nil {
		return "", fmt.Errorf("encoding media metadata: %w", err)
	}

	var versionID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO reelpin.content_versions
			(content_id, processor_version, prompt_version, schema_version, model_version,
			 title, summary, caption, transcript, tags, key_facts, raw_extraction, media,
			 extraction_status, created_at)
		VALUES ($1, $2, $2, $2, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10::jsonb, $11,
		        coalesce($12, now()))
		RETURNING id::text`,
		contentID, legacyVersion, extraction.Title, extraction.Summary, caption, transcript,
		extraction.TopicalTags, extraction.KeyFacts, string(raw), string(media),
		extractionStatus(row.ParseStatus), row.CreatedAt,
	).Scan(&versionID); err != nil {
		return "", fmt.Errorf("inserting the content version: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE reelpin.contents SET current_version_id = $1, updated_at = now()
		WHERE id = $2 AND current_version_id IS NULL`,
		versionID, contentID); err != nil {
		return "", fmt.Errorf("pointing the content at its version: %w", err)
	}
	return versionID, nil
}

// merge takes every field the override actually has, keeping the base's where
// it has nothing.
func merge(base, override ai.Extraction) ai.Extraction {
	merged := base
	if strings.TrimSpace(override.Title) != "" {
		merged.Title = override.Title
	}
	if strings.TrimSpace(override.Summary) != "" {
		merged.Summary = override.Summary
	}
	if strings.TrimSpace(override.ContentDomain) != "" {
		merged.ContentDomain = override.ContentDomain
	}
	if strings.TrimSpace(override.ContentFormat) != "" {
		merged.ContentFormat = override.ContentFormat
	}
	if len(override.TopicalTags) > 0 {
		merged.TopicalTags = override.TopicalTags
	}
	if len(override.KeyFacts) > 0 {
		merged.KeyFacts = override.KeyFacts
	}
	if len(override.Locations) > 0 {
		merged.Locations = override.Locations
	}
	if len(override.PeopleMentioned) > 0 {
		merged.PeopleMentioned = override.PeopleMentioned
	}
	if len(override.ActionableItems) > 0 {
		merged.ActionableItems = override.ActionableItems
	}
	if len(override.Events) > 0 {
		merged.Events = override.Events
	}
	return merged
}

// extractionStatus maps the legacy parse status onto the two values the column
// allows. Anything Python did not call parsed is claimed as partial rather than
// presented as a complete extraction.
func extractionStatus(parseStatus *string) string {
	switch strings.ToLower(strings.TrimSpace(text(parseStatus))) {
	case "", "parsed", "complete", "completed":
		return "completed"
	default:
		return "partial"
	}
}
