package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ObjectStore removes a stored object. It is declared here because this is the
// only thing that deletes one; the storage package owns putting them there.
type ObjectStore interface {
	Delete(ctx context.Context, ref string) error
}

// PurgeTarget names the source to remove, in the same two forms contents
// identifies itself by: a stable source id, or the normalized URL's hash.
type PurgeTarget struct {
	Platform    string
	ContentType string
	ContentID   string
	URLHash     string
}

func (t PurgeTarget) valid() error {
	if t.Platform == "" || t.ContentType == "" {
		return errors.New("a purge target needs a platform and a content type")
	}
	if t.ContentID == "" && t.URLHash == "" {
		return errors.New("a purge target needs a source content id or a normalized url hash")
	}
	return nil
}

// PurgeReport is what a purge removed, and what it could not.
type PurgeReport struct {
	Contents int `json:"contents"`
	Versions int `json:"versions"`
	Saves    int `json:"saves"`
	Runs     int `json:"runs"`
	// Objects counts stored files actually deleted. ObjectsSkipped counts the
	// ones nobody could delete because no object store was configured; the
	// caller is told rather than left to assume.
	Objects        int  `json:"objects"`
	ObjectsSkipped int  `json:"objects_skipped"`
	Blocklisted    bool `json:"blocklisted"`
}

// Purge removes everything derived from one source and blocks it from coming
// back. It is a privileged operation: privacy and legal requests, a private
// source that should never have been global, an operator decision.
type Purge struct {
	pool    *pgxpool.Pool
	objects ObjectStore
	logger  *slog.Logger
}

func NewPurge(pool *pgxpool.Pool, objects ObjectStore, logger *slog.Logger) *Purge {
	return &Purge{pool: pool, objects: objects, logger: logger}
}

// Run removes the content, its versions, every save of it and its runs, then
// records the blocklist row that stops the next share rebuilding it. The
// blocklist is written in the same transaction as the delete: a purge that
// removed the data but not the block would be undone by the next submission.
func (p *Purge) Run(ctx context.Context, target PurgeTarget, reason, blockedBy string) (PurgeReport, error) {
	if err := target.valid(); err != nil {
		return PurgeReport{}, err
	}
	if reason == "" || blockedBy == "" {
		return PurgeReport{}, errors.New("a purge needs a reason and an operator")
	}

	report := PurgeReport{}

	transaction, err := p.pool.Begin(ctx)
	if err != nil {
		return report, fmt.Errorf("starting the purge: %w", err)
	}
	defer transaction.Rollback(ctx)

	// Stored objects are outside the database, so their references are read
	// before the rows go and deleted after the commit.
	rows, err := transaction.Query(ctx, `
		SELECT v.media->>'thumbnail_url'
		FROM reelpin.content_versions v
		JOIN reelpin.contents c ON c.id = v.content_id
		WHERE (($1::text IS NOT NULL AND c.source_platform = $2 AND c.source_content_type = $3
		        AND c.source_content_id = $1)
		    OR ($4::text IS NOT NULL AND c.normalized_url_hash = $4))
		  AND v.media->>'thumbnail_url' IS NOT NULL`,
		nullable(target.ContentID), target.Platform, target.ContentType, nullable(target.URLHash))
	if err != nil {
		return report, fmt.Errorf("reading stored objects: %w", err)
	}
	refs := []string{}
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			rows.Close()
			return report, fmt.Errorf("reading a stored object reference: %w", err)
		}
		refs = append(refs, ref)
	}
	rows.Close()
	if rows.Err() != nil {
		return report, fmt.Errorf("reading stored objects: %w", rows.Err())
	}

	// Counted before the delete, because a cascade reports nothing.
	if err := transaction.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM reelpin.content_versions v JOIN reelpin.contents c ON c.id = v.content_id WHERE `+matchClause+`),
			(SELECT count(*) FROM reelpin.user_saves s JOIN reelpin.contents c ON c.id = s.content_id WHERE `+matchClause+`),
			(SELECT count(*) FROM reelpin.processing_runs r JOIN reelpin.contents c ON c.id = r.content_id WHERE `+matchClause+`)`,
		nullable(target.ContentID), target.Platform, target.ContentType, nullable(target.URLHash),
	).Scan(&report.Versions, &report.Saves, &report.Runs); err != nil {
		return report, fmt.Errorf("counting what the purge removes: %w", err)
	}

	// Versions, saves and runs all cascade from contents.
	tag, err := transaction.Exec(ctx,
		`DELETE FROM reelpin.contents c WHERE `+matchClause,
		nullable(target.ContentID), target.Platform, target.ContentType, nullable(target.URLHash))
	if err != nil {
		return report, fmt.Errorf("deleting the purged content: %w", err)
	}
	report.Contents = int(tag.RowsAffected())

	if _, err := transaction.Exec(ctx, `
		INSERT INTO reelpin.source_blocklist
			(source_platform, source_content_type, source_content_id, normalized_url_hash, reason, blocked_by)
		VALUES ($2, $3, $1, $4, $5, $6)
		ON CONFLICT DO NOTHING`,
		nullable(target.ContentID), target.Platform, target.ContentType, nullable(target.URLHash),
		reason, blockedBy,
	); err != nil {
		return report, fmt.Errorf("recording the blocklist entry: %w", err)
	}
	report.Blocklisted = true

	if err := transaction.Commit(ctx); err != nil {
		return report, fmt.Errorf("committing the purge: %w", err)
	}

	// After the commit: an object delete that fails leaves a file behind, which
	// is a storage cost, not a privacy hole, because nothing references it.
	for _, ref := range refs {
		if p.objects == nil {
			report.ObjectsSkipped++
			continue
		}
		if err := p.objects.Delete(ctx, ref); err != nil {
			p.logger.Error("deleting a purged object failed", "error", err)
			report.ObjectsSkipped++
			continue
		}
		report.Objects++
	}
	if report.ObjectsSkipped > 0 && p.objects == nil {
		p.logger.Warn("no object store configured; purged content's stored files were left in place",
			"objects", report.ObjectsSkipped)
	}

	return report, nil
}

// matchClause selects one source identity by either of its two forms. It is a
// constant rather than built per call so the counts and the delete cannot
// drift apart.
const matchClause = `
	(($1::text IS NOT NULL AND c.source_platform = $2 AND c.source_content_type = $3
	  AND c.source_content_id = $1)
	OR ($4::text IS NOT NULL AND c.normalized_url_hash = $4))`

// Blocked reports whether a source is on the blocklist. The database enforces
// the block on insert; this is for telling a caller why, before they try.
func (p *Purge) Blocked(ctx context.Context, target PurgeTarget) (bool, error) {
	var blocked bool
	err := p.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM reelpin.source_blocklist b
			WHERE (b.source_content_id IS NOT NULL AND b.source_platform = $2
			       AND b.source_content_type = $3 AND b.source_content_id = $1)
			   OR (b.normalized_url_hash IS NOT NULL AND b.normalized_url_hash = $4))`,
		nullable(target.ContentID), target.Platform, target.ContentType, nullable(target.URLHash),
	).Scan(&blocked)
	if err != nil {
		return false, fmt.Errorf("checking the blocklist: %w", err)
	}
	return blocked, nil
}

// nullable turns an empty string into a SQL NULL, so one query can match on
// either identity form without building the statement twice.
func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}
