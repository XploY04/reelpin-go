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
	sourceCache = "processing_cache"
)

// deltaOverlap is subtracted from the high-water mark a run prints. A legacy
// row stamped just before the mark can commit just after the pass read past
// it, and Supabase and Neon keep different clocks. Copies are idempotent, so
// reading a few minutes twice costs nothing.
const deltaOverlap = 5 * time.Minute

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
	// SourceSchema is where the legacy tables are read from. Empty means
	// public, which is where Supabase keeps them.
	SourceSchema string
	// Since switches to a delta pass: only legacy rows that changed after it
	// are read, no resume cursor is read or moved, and every table is
	// scanned from the start of its keyspace.
	Since *time.Time
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
	// Conflicts counts legacy reels whose user already had a save of the same
	// content under another id. The newer of the two is kept; see
	// resolveConflict. ConflictsReplaced is how many of those the legacy row
	// won.
	Conflicts         int `json:"conflicts"`
	ConflictsReplaced int `json:"conflicts_replaced"`
	Failures          int `json:"failures"`

	JobsScanned      int `json:"jobs_scanned"`
	JobsCreated      int `json:"jobs_created"`
	JobsAlreadyThere int `json:"jobs_already_there"`
	JobsUncertain    int `json:"jobs_uncertain"`
	RunsCreated      int `json:"runs_created"`

	Tables map[string]TableReport `json:"tables"`

	// HighWaterMark is what the next --since should be. Only a real run that
	// finished sets it.
	HighWaterMark *time.Time `json:"high_water_mark,omitempty"`
}

type Backfiller struct {
	pool     *pgxpool.Pool
	logger   *slog.Logger
	resolver *sourceidentity.Resolver
	source   source
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

// Run copies reels, then jobs, then the wave 1 tables, resuming from the
// recorded cursor. Jobs run after reels because a job is only copied once its
// content exists, and reminders, price tracks and hidden pins point at saves.
func (b *Backfiller) Run(ctx context.Context, options Options) (Report, error) {
	if options.BatchSize <= 0 {
		options.BatchSize = 500
	}
	b.seen = map[string]bool{}

	var err error
	if b.source, err = newSource(options.SourceSchema); err != nil {
		return Report{}, err
	}
	if err := b.source.describe(ctx, b.pool); err != nil {
		return Report{}, err
	}
	if err := b.source.require(sourceReels, sourceJobs, sourceCache); err != nil {
		return Report{}, err
	}

	release, err := b.lock(ctx)
	if err != nil {
		return Report{}, err
	}
	defer release()

	// Taken before anything is read, so every row this run could have missed
	// is after it. A snapshot resumed from a cursor started when its first
	// attempt did: rows written behind the cursor since then were never read.
	var started time.Time
	if err := b.pool.QueryRow(ctx, `
		SELECT CASE WHEN $2 THEN now() ELSE least(now(), (
			SELECT min(created_at) FROM reelpin.backfill_progress WHERE backfill_version = $1))
		END`, Version, options.Since != nil).Scan(&started); err != nil {
		return Report{}, fmt.Errorf("reading the database clock: %w", err)
	}

	report := Report{Tables: map[string]TableReport{}}
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
	for _, mapping := range wave1Mappings {
		if err := b.copyTable(ctx, options, mapping, &report); err != nil {
			return report, err
		}
	}

	if options.Execute {
		mark := started.Add(-deltaOverlap)
		report.HighWaterMark = &mark
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
	cursor := ""
	if options.Since == nil {
		var err error
		if cursor, err = b.cursor(ctx, sourceReels); err != nil {
			return err
		}
	}

	batch := int64(0)
	for {
		rows, err := b.readReels(ctx, cursor, options)
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

	identity, scopeHash, err := identify(ctx, b.resolver, row.URL, row.NormalizedURL, row.UserID)
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
		cached, err := readCache(ctx, transaction, b.source.table(sourceCache), identity)
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

	kept, err := b.resolveConflict(ctx, transaction, row, contentID, report)
	if err != nil {
		return err
	}
	if kept != "" {
		if err := auditTx(ctx, transaction, batch, sourceReels, row.ID, "skipped_conflict",
			&contentID, &versionID, nil, "kept the newer save "+kept); err != nil {
			return err
		}
		return transaction.Commit(ctx)
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
	if tag.RowsAffected() == 0 {
		// The app saved the same content between the conflict check and the
		// insert. The app's save is the newer one.
		report.Conflicts++
		b.logger.Info("save conflict: kept the newer save", "kept", "concurrent", "dropped", row.ID)
		if err := auditTx(ctx, transaction, batch, sourceReels, row.ID, "skipped_conflict",
			&contentID, &versionID, nil, "a concurrent save of the same content"); err != nil {
			return err
		}
		return transaction.Commit(ctx)
	}
	report.SavesCreated++

	if err := auditTx(ctx, transaction, batch, sourceReels, row.ID, "saved", &contentID, &versionID, nil, ""); err != nil {
		return err
	}
	return transaction.Commit(ctx)
}

// resolveConflict applies the one conflict rule: when this user already has a
// save of the same content under another id, from an earlier legacy row or
// from the new app during the dual run, the newer save is kept and the other
// is logged. It returns the id of the save that was kept when that is the
// existing one; an empty id means the legacy row goes in.
//
// Replacing a save deletes it, and what hangs off it goes with it: collection
// items and reminders cascade, and a job's link to it is cleared. A legacy row
// with no created_at never wins, because nothing says it is newer.
func (b *Backfiller) resolveConflict(
	ctx context.Context,
	tx pgx.Tx,
	row reelRow,
	contentID string,
	report *Report,
) (string, error) {
	var existingID string
	var existingSavedAt time.Time
	err := tx.QueryRow(ctx, `
		SELECT id::text, saved_at FROM reelpin.user_saves
		WHERE user_id = $1 AND content_id = $2
		FOR UPDATE`,
		row.UserID, contentID,
	).Scan(&existingID, &existingSavedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("checking for a conflicting save: %w", err)
	}

	report.Conflicts++
	if row.CreatedAt == nil || !row.CreatedAt.After(existingSavedAt) {
		b.logger.Info("save conflict: kept the newer save", "kept", existingID, "dropped", row.ID)
		return existingID, nil
	}

	if _, err := tx.Exec(ctx, `DELETE FROM reelpin.user_saves WHERE id = $1`, existingID); err != nil {
		return "", fmt.Errorf("replacing an older save: %w", err)
	}
	// The older save may itself have come from a legacy row. Its audit entry
	// would otherwise claim a save that no longer exists.
	if _, err := tx.Exec(ctx, `
		UPDATE reelpin.backfill_audit
		SET action = 'skipped_conflict', note = $3
		WHERE backfill_version = $1 AND source_table = '`+sourceReels+`' AND source_id = $2`,
		Version, existingID, "replaced by the newer save "+row.ID); err != nil {
		return "", fmt.Errorf("recording the replaced save: %w", err)
	}
	report.ConflictsReplaced++
	b.logger.Info("save conflict: kept the newer save", "kept", row.ID, "dropped", existingID)
	return "", nil
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

	cached, err := readCache(ctx, b.pool, b.source.table(sourceCache), identity)
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
func identify(
	ctx context.Context,
	resolver *sourceidentity.Resolver,
	rawURL string,
	normalized *string,
	userID string,
) (sourceidentity.SourceIdentity, string, error) {
	sourceURL := strings.TrimSpace(rawURL)
	if sourceURL == "" && normalized != nil {
		sourceURL = strings.TrimSpace(*normalized)
	}
	identity, err := resolver.Resolve(ctx, sourceURL)
	if err != nil {
		return sourceidentity.SourceIdentity{}, "", err
	}
	scopeHash, err := identity.Scope.ForUser(userID).Hash()
	if err != nil {
		return sourceidentity.SourceIdentity{}, "", err
	}
	return identity, scopeHash, nil
}

// reelColumns and scanReel are shared with the verifier, so the pass that
// writes and the pass that checks can never read a legacy row differently.
const reelColumns = `l.id::text, l.user_id::text, l.url, l.normalized_url, l.thumbnail_url,
	       l.title, l.summary, l.transcript, l.category, l.subcategory,
	       l.secondary_categories, l.key_facts, l.locations, l.people_mentioned,
	       l.actionable_items, l.events, l.parse_status, l.created_at`

func scanReel(source pgx.Row) (reelRow, error) {
	var row reelRow
	var title, summary, transcript *string
	if err := source.Scan(
		&row.ID, &row.UserID, &row.URL, &row.NormalizedURL, &row.ThumbnailURL,
		&title, &summary, &transcript, &row.Category, &row.Subcategory,
		&row.SecondaryCategories, &row.KeyFacts, &row.Locations, &row.PeopleMentioned,
		&row.ActionableItems, &row.Events, &row.ParseStatus, &row.CreatedAt,
	); err != nil {
		return reelRow{}, err
	}
	row.Title, row.Summary, row.Transcript = text(title), text(summary), text(transcript)
	return row, nil
}

func (b *Backfiller) readReels(ctx context.Context, cursor string, options Options) ([]reelRow, error) {
	shape := b.source.shape(sourceReels)
	rows, err := b.pool.Query(ctx, `
		SELECT `+reelColumns+`
		FROM `+b.source.table(sourceReels)+` l
		WHERE ($1::uuid IS NULL OR l.id > $1::uuid)
		  AND ($3::timestamptz IS NULL OR `+shape.changed()+` > $3)`+shape.environmentFilter()+`
		ORDER BY l.id
		LIMIT $2`, nullableUUID(cursor), options.BatchSize, options.Since)
	if err != nil {
		return nil, fmt.Errorf("reading legacy reels: %w", err)
	}
	defer rows.Close()

	collected := []reelRow{}
	for rows.Next() {
		row, err := scanReel(rows)
		if err != nil {
			return nil, fmt.Errorf("reading legacy reels: %w", err)
		}
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
func readCache(ctx context.Context, db querier, table string, identity sourceidentity.SourceIdentity) (*cacheRow, error) {
	if identity.ContentID == "" {
		return nil, nil
	}

	var row cacheRow
	var transcript, caption *string
	err := db.QueryRow(ctx, `
		SELECT transcript, caption, thumbnail_url, extracted_data
		FROM `+table+`
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
	content, err := versionFields(row, extraction, cached)
	if err != nil {
		return "", err
	}

	raw, err := json.Marshal(map[string]any{
		"extraction":  content.Extraction,
		"category":    text(row.Category),
		"subcategory": text(row.Subcategory),
	})
	if err != nil {
		return "", fmt.Errorf("encoding the extraction: %w", err)
	}
	media, err := json.Marshal(map[string]any{"thumbnail_url": content.Thumbnail})
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
		contentID, legacyVersion, content.Extraction.Title, content.Extraction.Summary,
		content.Caption, content.Transcript,
		content.Extraction.TopicalTags, content.Extraction.KeyFacts, string(raw), string(media),
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

// versionContent is what one content version carries once the legacy row and
// the global cache have been folded together.
type versionContent struct {
	Extraction ai.Extraction
	Transcript string
	Caption    string
	Thumbnail  string
}

// versionFields is that fold, kept out of insertVersion so the verifier can run
// the same derivation. A check that reconstructs the expectation its own way
// would disagree with the writer over whitespace and truncation rather than
// over the data, which is the disagreement nobody needs.
func versionFields(row reelRow, extraction ai.Extraction, cached *cacheRow) (versionContent, error) {
	content := versionContent{
		Transcript: row.Transcript,
		Thumbnail:  text(row.ThumbnailURL),
	}

	if cached != nil {
		// The cache is the global copy Python already paid for; a reel row is
		// one user's view of it, so the cache wins wherever it has something.
		cachedExtraction, err := cached.extraction()
		if err != nil {
			return versionContent{}, fmt.Errorf("reading the cached extraction: %w", err)
		}
		extraction = merge(extraction, cachedExtraction)
		if strings.TrimSpace(cached.Transcript) != "" {
			content.Transcript = cached.Transcript
		}
		content.Caption = cached.Caption
		if cached.ThumbnailURL != nil && strings.TrimSpace(*cached.ThumbnailURL) != "" {
			content.Thumbnail = *cached.ThumbnailURL
		}
	}

	content.Extraction = extraction.Normalize()
	return content, nil
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
