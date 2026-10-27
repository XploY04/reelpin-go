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
	"strconv"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/ai"
	"github.com/XploY04/reelpin-go/internal/embed"
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

// legacyEmbeddingModel is what Python embedded reels with when a row does not
// record its own embedding_model. A vector is only comparable with others from
// the same model, so the tag travels with it.
const legacyEmbeddingModel = "gemini-embedding-001"

// sourceReelEmbeddings is the audit table name for a legacy vector left
// behind. It is not sourceReels: the reel itself has its own audit row, which
// one row per source id would overwrite.
const sourceReelEmbeddings = "reel_embeddings"

// skippedEmbeddingWidth is the audit action for a vector whose model tag
// names a width other than the canonical column's.
const skippedEmbeddingWidth = "skipped_embedding_width"

const (
	sourceReels = "reels"
	sourceJobs  = "processing_jobs"
	sourceCache = "processing_cache"
)

// deltaOverlap is subtracted from the dump time to make the high-water mark a
// run prints. A legacy row stamped just before the dump can commit just after
// the dump's snapshot was taken, and so be missing from it. Copies are
// idempotent, so reading a few minutes twice costs nothing.
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
	// LegacySchema is where the legacy tables are read from. Empty means
	// DefaultLegacySchema.
	LegacySchema string
	// Since switches to a delta pass: only legacy rows that changed after it
	// are read, no resume cursor is read or moved, and every table is
	// scanned from the start of its keyspace. Every run, a delta or not,
	// leaves alone every row owned by a user in reelpin.user_migrations: Go
	// has served them, so their Go rows are theirs to change and their Python
	// rows are frozen. A user with an account deletion request counts as
	// served too; see servedUsers.
	Since *time.Time
	// DumpedAt is when the dump the legacy schema was restored from was taken,
	// read off Supabase's clock just before pg_dump. The next delta's mark is
	// derived from it and from nothing else: a row Python wrote after the dump
	// is not in this restore however long the restore took. A real run and
	// every delta need it.
	DumpedAt *time.Time
	// PropagateDeletes makes a delta delete the canonical rows it carried for
	// users Go has not served whose legacy row is gone. It is for
	// the final delta only, with Python stopped: while Python still runs, a
	// user can move between the dump and the delete.
	PropagateDeletes bool
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
	// content under another id; see resolveConflict. ConflictsMerged is how
	// many of those were newer and moved the existing save's saved_at.
	Conflicts       int `json:"conflicts"`
	ConflictsMerged int `json:"conflicts_merged"`
	Failures        int `json:"failures"`
	// InvalidOwners counts legacy reels whose user_id is not a UUID, and
	// UnknownOwners those whose user is not in auth.users. Both are skipped.
	InvalidOwners int `json:"invalid_owners"`
	UnknownOwners int `json:"unknown_owners"`

	// LocationsCopied and EmbeddingsCopied count content_locations and
	// content_embeddings rows this run wrote or refreshed from the legacy
	// reels; see copyReelPlacesAndEmbeddings. EmbeddingsSkipped counts vectors
	// left behind because their model tag names another width.
	LocationsCopied   int `json:"locations_copied"`
	EmbeddingsCopied  int `json:"embeddings_copied"`
	EmbeddingsSkipped int `json:"embeddings_skipped"`

	JobsScanned      int `json:"jobs_scanned"`
	JobsCreated      int `json:"jobs_created"`
	JobsAlreadyThere int `json:"jobs_already_there"`
	JobsUncertain    int `json:"jobs_uncertain"`
	RunsCreated      int `json:"runs_created"`
	// JobsInvalidOwners and JobsUnknownOwners are InvalidOwners and
	// UnknownOwners for jobs.
	JobsInvalidOwners int `json:"jobs_invalid_owners"`
	JobsUnknownOwners int `json:"jobs_unknown_owners"`

	Tables map[string]TableReport `json:"tables"`

	// MigratedUsers is how many users Go has served. A delta leaves their rows
	// alone.
	MigratedUsers int `json:"migrated_users"`
	// Deleted counts, per legacy table, the canonical rows --propagate-deletes
	// removed because their legacy row is gone.
	Deleted map[string]int `json:"deleted"`

	// UsersWritten is true once the users step committed. It commits on a
	// dry run too, so a dry run is not a run that wrote nothing.
	UsersWritten bool `json:"users_written"`

	// HighWaterMark is what the next --since should be: the dump time less
	// deltaOverlap. Only a real run that finished sets it.
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

// Run copies users, then reels, their places and embeddings, then jobs, then
// the wave 1 tables, resuming from the recorded cursor. Users come first
// because every user-owned row points at one. Jobs run after reels because a
// job is only copied once its content exists, and reminders, price tracks and
// hidden pins point at saves and at the places on them.
func (b *Backfiller) Run(ctx context.Context, options Options) (Report, error) {
	if options.BatchSize <= 0 {
		options.BatchSize = 500
	}
	if err := options.validate(); err != nil {
		return Report{}, err
	}
	b.seen = map[string]bool{}

	var err error
	if b.source, err = newSource(options.LegacySchema); err != nil {
		return Report{}, err
	}
	if err := b.source.describe(ctx, b.pool); err != nil {
		return Report{}, err
	}
	// Off Supabase the users table is auth.users' only source. Without it
	// every user-owned row would be skipped as an unknown reference.
	if err := b.source.require(sourceUsers, sourceReels, sourceJobs, sourceCache); err != nil {
		return Report{}, err
	}

	release, err := b.lock(ctx)
	if err != nil {
		return Report{}, err
	}
	defer release()

	report := Report{Tables: map[string]TableReport{}}
	if err := b.pool.QueryRow(ctx, `SELECT count(*) FROM reelpin.user_migrations`).
		Scan(&report.MigratedUsers); err != nil {
		return report, fmt.Errorf("counting the users Go has served: %w", err)
	}
	if err := b.copyUsers(ctx, &report); err != nil {
		return report, err
	}
	if err := b.backfillReels(ctx, options, &report); err != nil {
		return report, err
	}
	// A rehearsal that stopped part-way through the reels has no business
	// judging jobs: every job past the cursor would be recorded as having no
	// content, which is a decision about the rehearsal, not about the job.
	if options.MaxRows > 0 && report.ReelsScanned >= options.MaxRows {
		return report, nil
	}
	if err := b.copyReelPlacesAndEmbeddings(ctx, options, &report); err != nil {
		return report, err
	}
	if err := b.copyJobs(ctx, options, &report); err != nil {
		return report, err
	}
	for _, mapping := range wave1Mappings {
		if err := b.copyTable(ctx, options, mapping, &report); err != nil {
			return report, err
		}
	}
	if options.PropagateDeletes {
		if err := b.propagateDeletes(ctx, options, &report); err != nil {
			return report, err
		}
	}

	if options.Execute {
		mark := options.DumpedAt.Add(-deltaOverlap)
		report.HighWaterMark = &mark
	}
	return report, nil
}

func (o Options) validate() error {
	if (o.Execute || o.Since != nil) && o.DumpedAt == nil {
		return errors.New("a real run or a delta needs the time the legacy dump was taken")
	}
	// A mark at or after the dump reads nothing from it, and the mark this
	// run prints would then skip every row written in between.
	if o.Since != nil && !o.Since.Before(*o.DumpedAt) {
		return fmt.Errorf("the delta mark %s is not before the dump time %s",
			o.Since.UTC().Format(time.RFC3339), o.DumpedAt.UTC().Format(time.RFC3339))
	}
	if o.PropagateDeletes && o.Since == nil {
		return errors.New("deletes are propagated by the final delta only, which needs a mark")
	}
	return nil
}

// skipMigrated is the predicate every run adds to a per-user legacy table, over
// the legacy row l and its owner column. A delta, a resumed snapshot and a
// rerun all leave a user Go has served alone; before the dual run the table is
// empty and it filters nothing.
func skipMigrated(owner string) string {
	if owner == "" {
		return ""
	}
	return ` AND NOT ` + migrated(owner)
}

// servedUsers is every user whose rows Go owns: those it has served, and those
// who asked it to delete their account. The deletion cascades the first record
// away, so the request row, which has no foreign key, stands in for it after.
const servedUsers = `(SELECT user_id FROM reelpin.user_migrations
	UNION ALL SELECT user_id FROM reelpin.account_deletion_requests)`

// isMigrated re-reads the record inside a row's own transaction. The page the
// row came from was filtered when it was read, and a user can move between
// that read and this write.
func isMigrated(ctx context.Context, db querier, userID string) (bool, error) {
	var moved bool
	if err := db.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM `+servedUsers+` m WHERE m.user_id::text = lower($1))`,
		userID).Scan(&moved); err != nil {
		return false, fmt.Errorf("checking whether Go has served the user: %w", err)
	}
	return moved, nil
}

// skippedInvalidOwner is the audit action for a legacy row whose owner is not
// a UUID. Python's user_id is text, and test rows such as 'default-user' sit
// beside real users; no canonical row can belong to one.
const skippedInvalidOwner = "skipped_invalid_owner"

// skippedUnknownReference is the audit action for a row whose parent is not
// there: the constraint a mapped table names it by, and an owner a reel or a
// job names that auth.users does not have.
const skippedUnknownReference = "skipped_unknown_reference"

// ownerSkip judges the user a legacy reel or job names before anything binds
// it to a uuid column, where a test owner would abort the whole run. It
// returns the skip action and its note, or "" for an owner who can hold rows.
// The dry run asks the same question, so its counts are the real run's.
func ownerSkip(ctx context.Context, db querier, userID string) (string, string, error) {
	if !uuidShape.MatchString(userID) {
		return skippedInvalidOwner, "the owner is not a uuid", nil
	}
	var known bool
	if err := db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM auth.users WHERE id = $1::uuid)`, userID).Scan(&known); err != nil {
		return "", "", fmt.Errorf("checking the owner in auth.users: %w", err)
	}
	if !known {
		return skippedUnknownReference, "the owner is not in auth.users", nil
	}
	return "", "", nil
}

// migrated is whether the owner of the legacy row l has been served, or
// deleted, by Go.
func migrated(owner string) string {
	return `EXISTS (SELECT 1 FROM ` + servedUsers + ` m WHERE m.user_id::text = lower(l.` + owner + `::text))`
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

	action, note, err := ownerSkip(ctx, b.pool, row.UserID)
	if err != nil {
		return err
	}
	switch action {
	case skippedInvalidOwner:
		report.InvalidOwners++
		return b.audit(ctx, options, batch, sourceReels, row.ID, action, nil, nil, nil, note)
	case skippedUnknownReference:
		report.UnknownOwners++
		return b.audit(ctx, options, batch, sourceReels, row.ID, action, nil, nil, nil, note)
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
		moved, err := isMigrated(ctx, b.pool, row.UserID)
		if err != nil {
			return err
		}
		if moved {
			return nil
		}
		return b.reportOneReel(ctx, identity, scopeHash, report)
	}

	transaction, err := b.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("starting the reel transaction: %w", err)
	}
	defer transaction.Rollback(ctx)

	moved, err := isMigrated(ctx, transaction, row.UserID)
	if err != nil {
		return err
	}
	if moved {
		return nil
	}

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

	kept, merged, err := b.resolveConflict(ctx, transaction, row, contentID, report)
	if err != nil {
		return err
	}
	if kept != "" {
		note := "kept the newer save " + kept
		if merged {
			note = "merged into the existing save " + kept
		}
		if err := auditTx(ctx, transaction, batch, sourceReels, row.ID, "skipped_conflict",
			&contentID, &versionID, nil, note); err != nil {
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
// from the new app during the dual run, the existing save always stays and
// the legacy row never becomes a second one. It returns the existing save's
// id, or "" when there is none and the legacy row goes in.
//
// The existing save is never deleted: collection items, reminders and price
// tracks cascade from it. When the legacy row is newer, its saved_at is
// written onto the existing save instead, which keeps its id and everything
// hanging off it, and merged is true. A legacy row with no created_at is never
// newer, because nothing says it is.
func (b *Backfiller) resolveConflict(
	ctx context.Context,
	tx pgx.Tx,
	row reelRow,
	contentID string,
	report *Report,
) (string, bool, error) {
	var existingID string
	var existingSavedAt time.Time
	err := tx.QueryRow(ctx, `
		SELECT id::text, saved_at FROM reelpin.user_saves
		WHERE user_id = $1 AND content_id = $2
		FOR UPDATE`,
		row.UserID, contentID,
	).Scan(&existingID, &existingSavedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("checking for a conflicting save: %w", err)
	}

	report.Conflicts++
	if row.CreatedAt == nil || !row.CreatedAt.After(existingSavedAt) {
		b.logger.Info("save conflict: kept the newer save", "kept", existingID, "dropped", row.ID)
		return existingID, false, nil
	}

	if _, err := tx.Exec(ctx, `UPDATE reelpin.user_saves SET saved_at = $2 WHERE id = $1`,
		existingID, *row.CreatedAt); err != nil {
		return "", false, fmt.Errorf("merging a newer legacy save: %w", err)
	}
	report.ConflictsMerged++
	b.logger.Info("save conflict: merged the newer legacy save into the existing one",
		"kept", existingID, "merged", row.ID,
		"saved_at_was", existingSavedAt, "saved_at", *row.CreatedAt)
	return existingID, true, nil
}

// copyReelPlacesAndEmbeddings carries what Python keeps on the reel row beside
// the extraction: the geocoded places and the embedding. Python writes both
// after the reel row exists, the embedding at the index step and places again
// from the geocode backfill, and the row has no updated_at to show it. So this
// reads every carried reel on every run, --since or not, and writes only what
// is new or changed.
//
// Only versions this backfill built are touched; a version the Go pipeline
// built has places and an embedding of its own. A dry run executes the same
// statements and rolls them back.
func (b *Backfiller) copyReelPlacesAndEmbeddings(ctx context.Context, options Options, report *Report) error {
	shape := b.source.shape(sourceReels)
	reels := b.source.table(sourceReels)

	transaction, err := b.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("starting the places and embeddings transaction: %w", err)
	}
	defer transaction.Rollback(ctx)

	if shape.has("locations") {
		tag, err := transaction.Exec(ctx, `
			WITH chosen AS (`+chosenReels(reels, shape, `l.locations AS value`,
			`jsonb_typeof(l.locations) = 'array'`)+`)
			INSERT INTO reelpin.content_locations AS cl
				(content_version_id, ordinal, name, address, locality, country, point, source)
			SELECT chosen.version_id, p.ordinal - 1,
			       coalesce(nullif(btrim(p.entry->>'name'), ''), nullif(btrim(p.entry->>'address'), ''), ''),
			       nullif(btrim(p.entry->>'address'), ''),
			       nullif(btrim(p.entry->>'city'), ''),
			       nullif(btrim(p.entry->>'country'), ''),
			       ST_MakePoint((p.entry->>'longitude')::float8, (p.entry->>'latitude')::float8)::geography,
			       'provider'
			FROM chosen
			CROSS JOIN LATERAL jsonb_array_elements(chosen.value) WITH ORDINALITY AS p(entry, ordinal)
			WHERE `+locatedEntry+`
			ON CONFLICT (content_version_id, ordinal) DO UPDATE SET
				name = EXCLUDED.name,
				address = EXCLUDED.address,
				locality = EXCLUDED.locality,
				country = EXCLUDED.country,
				point = EXCLUDED.point
			WHERE (cl.name, cl.address, cl.locality, cl.country, cl.point::text) IS DISTINCT FROM
			      (EXCLUDED.name, EXCLUDED.address, EXCLUDED.locality, EXCLUDED.country, EXCLUDED.point::text)`,
			legacyVersion)
		if err != nil {
			return fmt.Errorf("copying legacy reel places: %w", err)
		}
		report.LocationsCopied = int(tag.RowsAffected())
	}

	if shape.has("embedding") {
		// The vector is chosen among the reels whose tag fits, so a content
		// whose first reel carries another width still takes a vector that
		// fits from another of its reels.
		tag, err := transaction.Exec(ctx, `
			WITH chosen AS (`+chosenReels(reels, shape,
			`l.embedding::text AS embedding, `+carriedModel(shape)+` AS model`,
			`l.embedding IS NOT NULL AND `+carriedModel(shape)+` IS NOT NULL`)+`)
			INSERT INTO reelpin.content_embeddings AS e
				(content_version_id, embedding, model, dimension, document_version, document_hash)
			SELECT chosen.version_id, chosen.embedding::public.vector, chosen.model, $2, $1,
			       md5(chosen.model || '|' || chosen.embedding)
			FROM chosen
			ON CONFLICT (content_version_id) DO UPDATE SET
				embedding = EXCLUDED.embedding,
				model = EXCLUDED.model,
				dimension = EXCLUDED.dimension,
				document_hash = EXCLUDED.document_hash,
				embedded_at = now()
			WHERE e.document_version = EXCLUDED.document_version
			  AND e.document_hash IS DISTINCT FROM EXCLUDED.document_hash`,
			legacyVersion, embed.DefaultDimension)
		if err != nil {
			return fmt.Errorf("copying legacy reel embeddings: %w", err)
		}
		report.EmbeddingsCopied = int(tag.RowsAffected())

		// Every reel whose tag does not fit is audited, chosen or not, which is
		// what backfill-verify counts as skipped. The audit stores identifiers
		// only, and a model tag is not user text.
		skipped, err := transaction.Exec(ctx, `
			INSERT INTO reelpin.backfill_audit
				(backfill_version, batch, source_table, source_id, action, note)
			SELECT $2, 0, $3, l.id, $4,
			       'embedding_model ' || `+legacyModel(shape)+` || ' does not fit vector(' || $5::int || ')'
			`+builtReels(reels, shape, `l.embedding IS NOT NULL AND `+carriedModel(shape)+` IS NULL`)+`
			ON CONFLICT (backfill_version, source_table, source_id)
			DO UPDATE SET action = EXCLUDED.action, note = EXCLUDED.note`,
			legacyVersion, Version, sourceReelEmbeddings, skippedEmbeddingWidth, embed.DefaultDimension)
		if err != nil {
			return fmt.Errorf("auditing legacy reel embeddings left behind: %w", err)
		}
		report.EmbeddingsSkipped = int(skipped.RowsAffected())
	}

	if !options.Execute {
		return nil
	}
	return transaction.Commit(ctx)
}

// legacyModel is the model a legacy reel's vector was made with, as Python
// recorded it, over the legacy row l.
func legacyModel(shape tableShape) string {
	return `coalesce(nullif(btrim((` + shape.col("embedding_model") + `)::text), ''), '` + legacyEmbeddingModel + `')`
}

// carriedModel is the model a carried vector is stored under. Python tagged
// its vectors model@dimensions, gemini-embedding-001@768, but Go searches by
// EMBEDDING_MODEL and sends it to Gemini as the model name, so a tag carried
// verbatim would hide every vector from search. The suffix is dropped when it
// names the canonical width; any other width is NULL, a vector that cannot
// sit in the column under an honest tag.
func carriedModel(shape tableShape) string {
	model := legacyModel(shape)
	width := strconv.Itoa(embed.DefaultDimension)
	return `(CASE
		WHEN ` + model + ` !~ '@[0-9]+$' THEN ` + model + `
		WHEN substring(` + model + ` from '@([0-9]+)$') = '` + width + `'
		THEN regexp_replace(` + model + `, '@[0-9]+$', '')
		END)`
}

// chosenReels picks, for every content version this backfill built, the one
// carried legacy reel passing filter that its places or its embedding come
// from: the reel the version was built from, which shares its created_at, and
// otherwise the lowest id.
// Taking every field from one reel keeps a location's ordinal its index in
// that reel's locations array, which is what a hidden pin's location_index
// and a reel:{id}:{ordinal} pin id name.
func chosenReels(reels string, shape tableShape, columns, filter string) string {
	return `
		SELECT DISTINCT ON (v.id) v.id AS version_id, ` + columns + builtReels(reels, shape, filter) + `
		ORDER BY v.id, (l.created_at IS NOT DISTINCT FROM v.created_at) DESC, l.id`
}

// builtReels is the FROM and WHERE over the carried legacy reels l whose
// content's current version v this backfill built.
func builtReels(reels string, shape tableShape, filter string) string {
	return `
		FROM ` + reels + ` l
		JOIN reelpin.user_saves s ON s.id = l.id
		JOIN reelpin.contents c ON c.id = s.content_id
		JOIN reelpin.content_versions v ON v.id = c.current_version_id AND v.processor_version = $1
		WHERE ` + filter + shape.environmentFilter()
}

// locatedEntry is a legacy location the Python map drew a pin for: numeric
// latitude and longitude, in range. The entry is aliased p.entry. The CASE
// keeps the casts away from an entry whose coordinates are not numbers.
const locatedEntry = `CASE
	WHEN jsonb_typeof(p.entry->'latitude') = 'number' AND jsonb_typeof(p.entry->'longitude') = 'number'
	THEN (p.entry->>'latitude')::float8 BETWEEN -90 AND 90
	 AND (p.entry->>'longitude')::float8 BETWEEN -180 AND 180
	ELSE false END`

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
		  AND ($3::timestamptz IS NULL OR `+shape.changed()+` > $3)`+shape.environmentFilter()+
		skipMigrated("user_id")+`
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
