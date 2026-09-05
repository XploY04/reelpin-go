package embed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/XploY04/reelpin-go/internal/ai"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Indexer embeds content versions and their transcript chunks. It is the only
// writer of vector columns.
type Indexer struct {
	pool     *pgxpool.Pool
	embedder Embedder
	logger   *slog.Logger
}

func NewIndexer(pool *pgxpool.Pool, embedder Embedder, logger *slog.Logger) *Indexer {
	return &Indexer{pool: pool, embedder: embedder, logger: logger}
}

// Report counts one indexing pass.
type Report struct {
	Execute         bool `json:"execute"`
	VersionsSeen    int  `json:"versions_seen"`
	VersionsIndexed int  `json:"versions_indexed"`
	VersionsSkipped int  `json:"versions_skipped"`
	ChunksIndexed   int  `json:"chunks_indexed"`
	Failures        int  `json:"failures"`
}

// IndexVersion embeds one content version and any of its chunks that need it.
// It is idempotent: a version whose document and tags still match is skipped
// without a provider call.
func (i *Indexer) IndexVersion(ctx context.Context, contentVersionID string) (bool, int, error) {
	var (
		title, summary                 string
		structured                     []byte
		platform                       string
		contentType                    string
		stored                         StoredVector
		model, docVersion, contentHash *string
		dimension                      *int
	)
	err := i.pool.QueryRow(ctx, `
		SELECT v.title, v.summary, v.structured, c.source_platform, c.source_content_type,
		       v.embedding IS NOT NULL, v.embedding_model, v.embedding_dimension,
		       v.embedding_document_version, v.embedding_content_hash
		FROM reelpin.content_versions v
		JOIN reelpin.contents c ON c.id = v.content_id
		WHERE v.id = $1`, contentVersionID,
	).Scan(&title, &summary, &structured, &platform, &contentType,
		&stored.Present, &model, &dimension, &docVersion, &contentHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, 0, fmt.Errorf("content version %s not found", contentVersionID)
	}
	if err != nil {
		return false, 0, fmt.Errorf("reading the content version: %w", err)
	}

	stored.Model, stored.DocumentVersion, stored.ContentHash = text(model), text(docVersion), text(contentHash)
	if dimension != nil {
		stored.Dimension = *dimension
	}

	var extraction ai.Extraction
	_ = json.Unmarshal(structured, &extraction)
	if extraction.Title == "" {
		extraction.Title = title
	}
	if extraction.Summary == "" {
		extraction.Summary = summary
	}

	document := Document(extraction, platform, contentType)
	indexed := false

	if NeedsEmbedding(stored, document) {
		vectors, err := i.embedder.Embed(ctx, []string{document}, TaskDocument)
		if err != nil {
			return false, 0, err
		}
		if len(vectors) != 1 {
			return false, 0, fmt.Errorf("the provider returned %d vectors for one document", len(vectors))
		}
		if _, err := i.pool.Exec(ctx, `
			UPDATE reelpin.content_versions
			SET embedding = $2::vector,
			    embedding_model = $3, embedding_dimension = $4,
			    embedding_document_version = $5, embedding_content_hash = $6,
			    embedded_at = now(), updated_at = now()
			WHERE id = $1`,
			contentVersionID, Vector(vectors[0]), Model, Dimension, DocumentVersion, ContentHash(document),
		); err != nil {
			return false, 0, fmt.Errorf("storing the content vector: %w", err)
		}
		indexed = true
	}

	chunks, err := i.indexChunks(ctx, contentVersionID)
	if err != nil {
		return indexed, 0, err
	}
	return indexed, chunks, nil
}

// indexChunks embeds the transcript chunks that do not have a current vector.
func (i *Indexer) indexChunks(ctx context.Context, contentVersionID string) (int, error) {
	rows, err := i.pool.Query(ctx, `
		SELECT id, chunk_text
		FROM reelpin.content_chunks
		WHERE content_version_id = $1
		  AND (embedding IS NULL
		       OR embedding_model IS DISTINCT FROM $2
		       OR embedding_dimension IS DISTINCT FROM $3
		       OR embedding_document_version IS DISTINCT FROM $4)
		ORDER BY ordinal`,
		contentVersionID, Model, Dimension, DocumentVersion)
	if err != nil {
		return 0, fmt.Errorf("reading chunks: %w", err)
	}
	defer rows.Close()

	ids := []int64{}
	texts := []string{}
	for rows.Next() {
		var id int64
		var chunk string
		if err := rows.Scan(&id, &chunk); err != nil {
			return 0, fmt.Errorf("reading chunks: %w", err)
		}
		ids = append(ids, id)
		texts = append(texts, chunk)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("reading chunks: %w", err)
	}
	if len(texts) == 0 {
		return 0, nil
	}

	vectors, err := i.embedder.Embed(ctx, texts, TaskDocument)
	if err != nil {
		return 0, err
	}
	if len(vectors) != len(ids) {
		return 0, fmt.Errorf("the provider returned %d vectors for %d chunks", len(vectors), len(ids))
	}

	for index, id := range ids {
		if _, err := i.pool.Exec(ctx, `
			UPDATE reelpin.content_chunks
			SET embedding = $2::vector, embedding_model = $3, embedding_dimension = $4,
			    embedding_document_version = $5, embedded_at = now()
			WHERE id = $1`,
			id, Vector(vectors[index]), Model, Dimension, DocumentVersion,
		); err != nil {
			return index, fmt.Errorf("storing a chunk vector: %w", err)
		}
	}
	return len(ids), nil
}

// Backfill indexes everything that needs it, in stable id order so it can be
// stopped and resumed. Dry-run by default, like every other backfill here.
func (i *Indexer) Backfill(ctx context.Context, execute bool, limit int) (Report, error) {
	report := Report{Execute: execute}
	if limit <= 0 {
		limit = 500
	}

	rows, err := i.pool.Query(ctx, `
		SELECT v.id::text
		FROM reelpin.content_versions v
		WHERE v.embedding IS NULL
		   OR v.embedding_model IS DISTINCT FROM $1
		   OR v.embedding_dimension IS DISTINCT FROM $2
		   OR v.embedding_document_version IS DISTINCT FROM $3
		ORDER BY v.id
		LIMIT $4`, Model, Dimension, DocumentVersion, limit)
	if err != nil {
		return report, fmt.Errorf("listing content to index: %w", err)
	}
	defer rows.Close()

	pending := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return report, fmt.Errorf("listing content to index: %w", err)
		}
		pending = append(pending, id)
	}
	if err := rows.Err(); err != nil {
		return report, fmt.Errorf("listing content to index: %w", err)
	}
	report.VersionsSeen = len(pending)

	if !execute {
		report.VersionsIndexed = len(pending)
		return report, nil
	}

	for _, contentVersionID := range pending {
		indexed, chunks, err := i.IndexVersion(ctx, contentVersionID)
		if err != nil {
			// One bad row must not stop a backfill of thousands.
			report.Failures++
			i.logger.Warn("indexing a content version failed",
				"content_version_id", contentVersionID, "error", err)
			continue
		}
		if indexed {
			report.VersionsIndexed++
		} else {
			report.VersionsSkipped++
		}
		report.ChunksIndexed += chunks
	}
	return report, nil
}

func text(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
