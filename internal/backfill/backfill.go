// Package backfill populates the global content model from the rows the Python
// service already wrote. It changes no API behaviour: existing reel and job IDs
// stay exactly as they are, and the only writes to old tables are the new link
// columns.
package backfill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/sourceidentity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Version names this backfill in the progress and audit tables. Bump it only
// for a genuinely different pass; resuming depends on it staying stable.
const Version = "2026-09-global-content-v1"

// advisoryLockKey keeps two copies of the backfill from running at once.
const advisoryLockKey int64 = 774_120_002

// jobLinkWindow bounds how far back jobs are linked. Older jobs are terminal
// history nobody polls.
const jobLinkWindow = 60 * 24 * time.Hour

// processorVersion labels content produced before the Go pipeline existed.
const processorVersion = "python-legacy"

const extractionSchemaVersion = "python-legacy"

type Options struct {
	// Execute switches from a dry run to real writes. Off by default.
	Execute bool
	// BatchSize bounds one keyset page.
	BatchSize int
	// MaxRows stops early, for a rehearsal on a big table.
	MaxRows int
}

// Report counts what happened. It carries no content and no user data.
type Report struct {
	ReelsScanned       int `json:"reels_scanned"`
	UniqueContent      int `json:"unique_content"`
	ContentVersions    int `json:"content_versions"`
	CacheHits          int `json:"cache_hits"`
	ReelsLinked        int `json:"reels_linked"`
	ReelsAlreadyLinked int `json:"reels_already_linked"`
	InvalidURLs        int `json:"invalid_urls"`
	Conflicts          int `json:"conflicts"`
	Failures           int `json:"failures"`

	JobsScanned   int `json:"jobs_scanned"`
	JobsLinked    int `json:"jobs_linked"`
	JobsUncertain int `json:"jobs_uncertain"`
	RunsCreated   int `json:"runs_created"`
}

type Backfiller struct {
	pool     *pgxpool.Pool
	logger   *slog.Logger
	resolver *sourceidentity.Resolver
}

func New(pool *pgxpool.Pool, logger *slog.Logger) *Backfiller {
	// Identity resolution stays offline here: a backfill must never depend on a
	// provider being reachable, and an unresolvable row is reported instead.
	return &Backfiller{pool: pool, logger: logger, resolver: &sourceidentity.Resolver{}}
}

// Run scans reels and then jobs, resuming from the recorded cursor.
func (b *Backfiller) Run(ctx context.Context, options Options) (Report, error) {
	if options.BatchSize <= 0 {
		options.BatchSize = 500
	}

	release, err := b.lock(ctx)
	if err != nil {
		return Report{}, err
	}
	defer release()

	report := Report{}
	if err := b.backfillReels(ctx, options, &report); err != nil {
		return report, err
	}
	if err := b.linkJobs(ctx, options, &report); err != nil {
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
	SourcePlatform      *string
	SourceContentID     *string
	ProcessingVersion   *string
	IngestionMethod     *string
	TranscriptSource    *string
	ThumbnailURL        *string
	Title               string
	Summary             string
	Transcript          string
	SecondaryCategories json.RawMessage
	KeyFacts            json.RawMessage
	Locations           json.RawMessage
	PeopleMentioned     json.RawMessage
	ActionableItems     json.RawMessage
	Events              json.RawMessage
	ParseStatus         *string
	ContentVersionID    *string
	CreatedAt           *time.Time
}

func (b *Backfiller) backfillReels(ctx context.Context, options Options, report *Report) error {
	cursor, err := b.cursor(ctx, "reels")
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
			return b.finishCursor(ctx, options, "reels")
		}
		batch++

		for _, row := range rows {
			cursor = row.ID
			report.ReelsScanned++
			if err := b.backfillOneReel(ctx, options, batch, row, report); err != nil {
				return err
			}
			if options.MaxRows > 0 && report.ReelsScanned >= options.MaxRows {
				return b.saveCursor(ctx, options, "reels", cursor, report.ReelsScanned)
			}
		}

		if err := b.saveCursor(ctx, options, "reels", cursor, report.ReelsScanned); err != nil {
			return err
		}
	}
}

func (b *Backfiller) backfillOneReel(ctx context.Context, options Options, batch int64, row reelRow, report *Report) error {
	if row.ContentVersionID != nil {
		report.ReelsAlreadyLinked++
		return nil
	}

	sourceURL := strings.TrimSpace(row.URL)
	if sourceURL == "" && row.NormalizedURL != nil {
		sourceURL = strings.TrimSpace(*row.NormalizedURL)
	}
	identity, err := b.resolver.Resolve(ctx, sourceURL)
	if err != nil {
		report.InvalidURLs++
		return b.audit(ctx, options, batch, "reels", row.ID, "skipped_invalid_url", nil, nil, nil, "identity could not be derived")
	}

	if !options.Execute {
		// A dry run still reports what it would have done, including whether a
		// cache entry would have supplied the transcript.
		cached, err := b.readCache(ctx, identity)
		if err != nil {
			return err
		}
		if cached != nil {
			report.CacheHits++
		}
		report.UniqueContent++
		report.ContentVersions++
		report.ReelsLinked++
		return nil
	}

	transaction, err := b.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("starting the reel transaction: %w", err)
	}
	defer transaction.Rollback(ctx)

	contentID, created, err := upsertContent(ctx, transaction, identity)
	if err != nil {
		report.Failures++
		return err
	}
	if created {
		report.UniqueContent++
	}

	cached, err := b.readCacheTx(ctx, transaction, identity)
	if err != nil {
		return err
	}
	if cached != nil {
		report.CacheHits++
	}

	versionID, createdVersion, err := upsertContentVersion(ctx, transaction, contentID, identity, row, cached)
	if err != nil {
		report.Failures++
		return err
	}
	if createdVersion {
		report.ContentVersions++
	}

	// The link is the only write to an existing row, and only when it is unset.
	tag, err := transaction.Exec(ctx,
		`UPDATE public.reels SET content_version_id = $1 WHERE id = $2 AND content_version_id IS NULL`,
		versionID, row.ID,
	)
	if err != nil {
		report.Failures++
		return fmt.Errorf("linking reel: %w", err)
	}
	if tag.RowsAffected() == 1 {
		report.ReelsLinked++
	} else {
		report.Conflicts++
	}

	if err := auditTx(ctx, transaction, batch, "reels", row.ID, "linked", &contentID, &versionID, nil, ""); err != nil {
		return err
	}
	return transaction.Commit(ctx)
}

func (b *Backfiller) readReels(ctx context.Context, cursor string, limit int) ([]reelRow, error) {
	query := `
		SELECT id, user_id, url, normalized_url, source_platform, source_content_id,
		       processing_version, ingestion_method, transcript_source, thumbnail_url,
		       title, summary, transcript, secondary_categories, key_facts, locations,
		       people_mentioned, actionable_items, events, parse_status,
		       content_version_id, created_at
		FROM public.reels
		WHERE ($1::uuid IS NULL OR id > $1::uuid)
		ORDER BY id
		LIMIT $2`

	rows, err := b.pool.Query(ctx, query, nullableUUID(cursor), limit)
	if err != nil {
		return nil, fmt.Errorf("reading reels: %w", err)
	}
	defer rows.Close()

	var collected []reelRow
	for rows.Next() {
		var row reelRow
		var title, summary, transcript *string
		if err := rows.Scan(
			&row.ID, &row.UserID, &row.URL, &row.NormalizedURL, &row.SourcePlatform,
			&row.SourceContentID, &row.ProcessingVersion, &row.IngestionMethod,
			&row.TranscriptSource, &row.ThumbnailURL, &title, &summary, &transcript,
			&row.SecondaryCategories, &row.KeyFacts, &row.Locations, &row.PeopleMentioned,
			&row.ActionableItems, &row.Events, &row.ParseStatus, &row.ContentVersionID, &row.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("reading reels: %w", err)
		}
		row.Title, row.Summary, row.Transcript = text(title), text(summary), text(transcript)
		collected = append(collected, row)
	}
	return collected, rows.Err()
}

type cacheRow struct {
	SourceContentType string
	ProcessingVersion string
	IngestionMethod   string
	TranscriptSource  string
	Transcript        string
	Caption           string
	ThumbnailURL      *string
	ExtractedData     json.RawMessage
}

// readCache prefers the global extraction Python already paid for.
func (b *Backfiller) readCache(ctx context.Context, identity sourceidentity.SourceIdentity) (*cacheRow, error) {
	return scanCache(ctx, b.pool, identity)
}

func (b *Backfiller) readCacheTx(ctx context.Context, tx pgx.Tx, identity sourceidentity.SourceIdentity) (*cacheRow, error) {
	return scanCache(ctx, tx, identity)
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func scanCache(ctx context.Context, db querier, identity sourceidentity.SourceIdentity) (*cacheRow, error) {
	if identity.ContentID == "" {
		return nil, nil
	}

	var row cacheRow
	var contentType, processingVersion, ingestion, transcriptSource, transcript, caption *string
	err := db.QueryRow(ctx, `
		SELECT source_content_type, processing_version, ingestion_method, transcript_source,
		       transcript, caption, thumbnail_url, extracted_data
		FROM public.processing_cache
		WHERE source_platform = $1 AND source_content_id = $2
		ORDER BY updated_at DESC NULLS LAST
		LIMIT 1`,
		identity.Platform, identity.ContentID,
	).Scan(&contentType, &processingVersion, &ingestion, &transcriptSource,
		&transcript, &caption, &row.ThumbnailURL, &row.ExtractedData)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the processing cache: %w", err)
	}

	row.SourceContentType = text(contentType)
	row.ProcessingVersion = text(processingVersion)
	row.IngestionMethod = text(ingestion)
	row.TranscriptSource = text(transcriptSource)
	row.Transcript = text(transcript)
	row.Caption = text(caption)
	return &row, nil
}

func upsertContent(ctx context.Context, tx pgx.Tx, identity sourceidentity.SourceIdentity) (string, bool, error) {
	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO reelpin.contents
			(source_platform, source_content_type, source_content_id, normalized_url, normalized_url_hash)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT DO NOTHING
		RETURNING id`,
		identity.Platform, identity.ContentType, publicContentID(identity),
		identity.NormalizedURL, urlHash(identity),
	).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, fmt.Errorf("creating content: %w", err)
	}

	// Another user's save already created it.
	if publicContentID(identity) != nil {
		err = tx.QueryRow(ctx, `
			SELECT id FROM reelpin.contents
			WHERE source_platform = $1 AND source_content_type = $2
			  AND source_content_id = $3 AND access_scope_hash = 'public'`,
			identity.Platform, identity.ContentType, identity.ContentID,
		).Scan(&id)
	} else {
		err = tx.QueryRow(ctx, `
			SELECT id FROM reelpin.contents
			WHERE normalized_url_hash = $1 AND access_scope_hash = 'public'
			  AND source_content_id IS NULL`,
			urlHash(identity),
		).Scan(&id)
	}
	if err != nil {
		return "", false, fmt.Errorf("reading existing content: %w", err)
	}
	return id, false, nil
}

func upsertContentVersion(
	ctx context.Context,
	tx pgx.Tx,
	contentID string,
	identity sourceidentity.SourceIdentity,
	row reelRow,
	cached *cacheRow,
) (string, bool, error) {
	transcript := row.Transcript
	caption := ""
	thumbnail := row.ThumbnailURL
	structured := structuredFromReel(row)

	if cached != nil {
		// The cache is the global copy; the reel row is one user's view of it.
		if strings.TrimSpace(cached.Transcript) != "" {
			transcript = cached.Transcript
		}
		caption = cached.Caption
		if cached.ThumbnailURL != nil && strings.TrimSpace(*cached.ThumbnailURL) != "" {
			thumbnail = cached.ThumbnailURL
		}
		if len(cached.ExtractedData) > 0 && string(cached.ExtractedData) != "null" {
			structured = cached.ExtractedData
		}
	}

	parseStatus := "parsed"
	if row.ParseStatus != nil && strings.TrimSpace(*row.ParseStatus) != "" {
		parseStatus = *row.ParseStatus
	}

	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO reelpin.content_versions
			(content_id, processor_version, extraction_schema_version, transcript, caption,
			 title, summary, structured, thumbnail_url, parse_status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (content_id, processor_version) DO NOTHING
		RETURNING id`,
		contentID, processorVersion, extractionSchemaVersion, transcript, caption,
		row.Title, row.Summary, structured, thumbnail, parseStatus,
	).Scan(&id)
	if err == nil {
		if _, err := tx.Exec(ctx,
			`UPDATE reelpin.contents SET current_content_version_id = $1, updated_at = now()
			 WHERE id = $2 AND current_content_version_id IS NULL`,
			id, contentID,
		); err != nil {
			return "", false, fmt.Errorf("pointing content at its version: %w", err)
		}
		return id, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, fmt.Errorf("creating content version: %w", err)
	}

	if err := tx.QueryRow(ctx,
		`SELECT id FROM reelpin.content_versions WHERE content_id = $1 AND processor_version = $2`,
		contentID, processorVersion,
	).Scan(&id); err != nil {
		return "", false, fmt.Errorf("reading existing content version: %w", err)
	}
	return id, false, nil
}

// structuredFromReel keeps the user-neutral extraction. Category and
// subcategory stay on the reel row: they are that user's filing, not the
// content's.
func structuredFromReel(row reelRow) json.RawMessage {
	payload := map[string]json.RawMessage{}
	for key, value := range map[string]json.RawMessage{
		"key_facts":        row.KeyFacts,
		"locations":        row.Locations,
		"people_mentioned": row.PeopleMentioned,
		"actionable_items": row.ActionableItems,
		"events":           row.Events,
	} {
		if len(value) > 0 && string(value) != "null" {
			payload[key] = value
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return encoded
}
