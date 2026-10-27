package embed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Embedder turns text into vectors. Implemented by the Gemini client; faked in
// tests, which never call a real provider.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Model() string
	Dimension() int
}

// ErrDimensionMismatch means the provider returned a vector the index cannot
// hold. Storing it would corrupt the set silently.
var ErrDimensionMismatch = errors.New("embedding dimension does not match the index")

type Indexer struct {
	pool     *pgxpool.Pool
	embedder Embedder
	logger   *slog.Logger
}

func NewIndexer(pool *pgxpool.Pool, embedder Embedder, logger *slog.Logger) *Indexer {
	return &Indexer{pool: pool, embedder: embedder, logger: logger}
}

// IndexVersion embeds one content version, skipping the provider call when the
// same document under the same model and dimension is already stored. That
// check is what makes a duplicate index event free rather than a second bill.
func (i *Indexer) IndexVersion(ctx context.Context, versionID string) error {
	fields, existingHash, err := i.load(ctx, versionID)
	if err != nil {
		return err
	}

	document := Document(fields)
	if document == "" {
		// Nothing worth embedding. Recording the attempt stops it being
		// retried forever by every redelivery.
		return i.markEmpty(ctx, versionID)
	}

	hash := Hash(document, i.embedder.Model(), i.embedder.Dimension())
	if existingHash == hash {
		return nil
	}

	vectors, err := i.embedder.Embed(ctx, []string{document})
	if err != nil {
		return fmt.Errorf("embedding version %s: %w", versionID, err)
	}
	if len(vectors) != 1 {
		return fmt.Errorf("the provider returned %d vectors for one document", len(vectors))
	}
	if len(vectors[0]) != i.embedder.Dimension() {
		return fmt.Errorf("%w: got %d, index holds %d",
			ErrDimensionMismatch, len(vectors[0]), i.embedder.Dimension())
	}

	if _, err := i.pool.Exec(ctx, `
		INSERT INTO reelpin.content_embeddings
			(content_version_id, embedding, model, dimension, document_version, document_hash)
		VALUES ($1, $2::vector, $3, $4, $5, $6)
		ON CONFLICT (content_version_id) DO UPDATE
		SET embedding = EXCLUDED.embedding,
		    model = EXCLUDED.model,
		    dimension = EXCLUDED.dimension,
		    document_version = EXCLUDED.document_version,
		    document_hash = EXCLUDED.document_hash,
		    embedded_at = now()`,
		versionID, Vector(vectors[0]), i.embedder.Model(), i.embedder.Dimension(),
		DocumentVersion, hash); err != nil {
		return fmt.Errorf("storing the embedding for %s: %w", versionID, err)
	}
	return nil
}

func (i *Indexer) load(ctx context.Context, versionID string) (Fields, string, error) {
	var fields Fields
	var existingHash *string
	var tags, facts []string
	var category *string

	// Places come from the extraction the pipeline stored, not from the
	// locations table: an embedding must not wait on geocoding, and a version
	// with no geocoded point still has place names worth searching.
	var places []string
	err := i.pool.QueryRow(ctx, `
		SELECT coalesce(title, ''), coalesce(summary, ''),
		       coalesce(tags, '{}'), coalesce(key_facts, '{}'),
		       raw_extraction->>'category',
		       (SELECT document_hash FROM reelpin.content_embeddings
		         WHERE content_version_id = content_versions.id),
		       coalesce(
		           ARRAY(SELECT jsonb_array_elements_text(
		               CASE WHEN jsonb_typeof(raw_extraction->'places') = 'array'
		                    THEN raw_extraction->'places' ELSE '[]'::jsonb END)),
		           '{}')
		FROM reelpin.content_versions
		WHERE id = $1`,
		versionID,
	).Scan(&fields.Title, &fields.Summary, &tags, &facts, &category, &existingHash, &places)
	if errors.Is(err, pgx.ErrNoRows) {
		return Fields{}, "", fmt.Errorf("content version %s does not exist", versionID)
	}
	if err != nil {
		return Fields{}, "", fmt.Errorf("loading version %s: %w", versionID, err)
	}

	fields.Tags = tags
	fields.Facts = facts
	fields.Places = places
	if category != nil {
		fields.Category = *category
	}
	if existingHash == nil {
		return fields, "", nil
	}
	return fields, *existingHash, nil
}

// markEmpty records that there was nothing to embed, so a redelivered event
// does not try again forever.
func (i *Indexer) markEmpty(ctx context.Context, versionID string) error {
	_, err := i.pool.Exec(ctx, `
		INSERT INTO reelpin.content_embeddings
			(content_version_id, embedding, model, dimension, document_version, document_hash)
		VALUES ($1, NULL, $2, $3, $4, 'empty')
		ON CONFLICT (content_version_id) DO UPDATE SET document_hash = 'empty'`,
		versionID, i.embedder.Model(), i.embedder.Dimension(), DocumentVersion)
	if err != nil {
		return fmt.Errorf("recording an empty document for %s: %w", versionID, err)
	}
	return nil
}

// BackfillReport is what one bounded pass did.
type BackfillReport struct {
	Scanned  int
	Embedded int
	Skipped  int
	Failed   int
	// LastID is the checkpoint: the next pass resumes after it.
	LastID string
}

// Backfill embeds versions that have none, in bounded batches, resuming from a
// checkpoint. It is safe to interrupt and safe to rerun.
func (i *Indexer) Backfill(ctx context.Context, after string, batchSize int) (BackfillReport, error) {
	if batchSize <= 0 || batchSize > 500 {
		batchSize = 100
	}

	// Keyset by id: a version added mid-backfill cannot shift the page.
	rows, err := i.pool.Query(ctx, `
		SELECT v.id::text FROM reelpin.content_versions v
		LEFT JOIN reelpin.content_embeddings e ON e.content_version_id = v.id
		WHERE e.content_version_id IS NULL AND ($1 = '' OR v.id::text > $1)
		ORDER BY v.id
		LIMIT $2`, after, batchSize)
	if err != nil {
		return BackfillReport{}, fmt.Errorf("finding versions to embed: %w", err)
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return BackfillReport{}, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if rows.Err() != nil {
		return BackfillReport{}, rows.Err()
	}

	report := BackfillReport{Scanned: len(ids), LastID: after}
	for _, id := range ids {
		report.LastID = id
		before := time.Now()
		err := i.IndexVersion(ctx, id)
		switch {
		case err == nil:
			report.Embedded++
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			// Stop cleanly: the checkpoint is the last id attempted, so a
			// rerun resumes rather than starting over.
			return report, err
		default:
			report.Failed++
			i.logger.Error("embedding failed", "version_id", id,
				"took", time.Since(before).String(), "error", err)
		}
	}
	return report, nil
}
