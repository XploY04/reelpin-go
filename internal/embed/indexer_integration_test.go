//go:build integration

package embed

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/XploY04/reelpin-go/internal/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

// countingEmbedder stands in for the provider and returns deterministic
// vectors, so a test can assert what was and was not paid for.
type countingEmbedder struct {
	calls atomic.Int64
	texts atomic.Int64
	err   error
}

func (c *countingEmbedder) Embed(_ context.Context, texts []string, _ TaskType) ([][]float32, error) {
	c.calls.Add(1)
	c.texts.Add(int64(len(texts)))
	if c.err != nil {
		return nil, c.err
	}

	vectors := make([][]float32, 0, len(texts))
	for index := range texts {
		vector := make([]float32, Dimension)
		vector[index%Dimension] = 1
		vectors = append(vectors, vector)
	}
	return vectors, nil
}

func testIndexer(t *testing.T) (*Indexer, *pgxpool.Pool, *countingEmbedder) {
	t.Helper()
	adminURL := os.Getenv("TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer admin.Close()

	name := "reelpin_embed_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
	if len(name) > 60 {
		name = name[:60]
	}
	for _, statement := range []string{
		`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`,
		`CREATE DATABASE ` + name,
	} {
		if _, err := admin.Exec(ctx, statement); err != nil {
			t.Fatalf("preparing %s: %v", name, err)
		}
	}

	parsed, _ := url.Parse(adminURL)
	parsed.Path = "/" + name
	if _, err := migrations.Up(ctx, parsed.String()); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	pool, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		cleanup, err := pgxpool.New(context.Background(), adminURL)
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
	})

	embedder := &countingEmbedder{}
	return NewIndexer(pool, embedder, slog.New(slog.NewJSONHandler(io.Discard, nil))), pool, embedder
}

func seedVersion(t *testing.T, pool *pgxpool.Pool, title string, chunks ...string) string {
	t.Helper()
	ctx := context.Background()

	var versionID string
	if err := pool.QueryRow(ctx, `
		WITH content AS (
			INSERT INTO reelpin.contents
				(source_platform, source_content_type, source_content_id, normalized_url, normalized_url_hash)
			VALUES ('instagram','reel',$1,'https://www.instagram.com/reel/'||$1||'/', $1)
			RETURNING id
		)
		INSERT INTO reelpin.content_versions
			(content_id, processor_version, extraction_schema_version, title, summary, structured)
		SELECT id, 'v1', 'v1', $2, 'A summary', '{"topical_tags":["Cafes"]}'::jsonb FROM content
		RETURNING id::text`, title, title).Scan(&versionID); err != nil {
		t.Fatalf("seeding a version: %v", err)
	}

	for index, chunk := range chunks {
		if _, err := pool.Exec(ctx, `
			INSERT INTO reelpin.content_chunks (content_version_id, ordinal, chunk_text, content_hash)
			VALUES ($1, $2, $3, $4)`, versionID, index, chunk, ContentHash(chunk)); err != nil {
			t.Fatalf("seeding a chunk: %v", err)
		}
	}
	return versionID
}

func TestIndexingStoresVectorsWithTheirTags(t *testing.T) {
	indexer, pool, embedder := testIndexer(t)
	ctx := context.Background()

	versionID := seedVersion(t, pool, "cafes", "the cafe opens at eight", "and closes at ten")

	indexed, chunks, err := indexer.IndexVersion(ctx, versionID)
	if err != nil {
		t.Fatalf("IndexVersion: %v", err)
	}
	if !indexed || chunks != 2 {
		t.Fatalf("indexed=%v chunks=%d", indexed, chunks)
	}

	var model, documentVersion, contentHash string
	var dimension int
	if err := pool.QueryRow(ctx, `
		SELECT embedding_model, embedding_dimension, embedding_document_version, embedding_content_hash
		FROM reelpin.content_versions WHERE id = $1`, versionID,
	).Scan(&model, &dimension, &documentVersion, &contentHash); err != nil {
		t.Fatalf("reading the vector tags: %v", err)
	}
	if model != Model || dimension != Dimension || documentVersion != DocumentVersion || contentHash == "" {
		t.Fatalf("tags = %s/%d/%s/%q", model, dimension, documentVersion, contentHash)
	}

	// Re-running costs nothing: the tags and the document still match.
	before := embedder.calls.Load()
	indexed, chunks, err = indexer.IndexVersion(ctx, versionID)
	if err != nil {
		t.Fatalf("second IndexVersion: %v", err)
	}
	if indexed || chunks != 0 {
		t.Fatalf("re-indexed an unchanged version: indexed=%v chunks=%d", indexed, chunks)
	}
	if embedder.calls.Load() != before {
		t.Fatalf("the provider was called again for unchanged content")
	}
}

func TestAChangedDocumentIsReEmbedded(t *testing.T) {
	indexer, pool, embedder := testIndexer(t)
	ctx := context.Background()

	versionID := seedVersion(t, pool, "cafes")
	if _, _, err := indexer.IndexVersion(ctx, versionID); err != nil {
		t.Fatalf("IndexVersion: %v", err)
	}
	before := embedder.calls.Load()

	// The extraction improved, so what this content means has changed.
	if _, err := pool.Exec(ctx,
		`UPDATE reelpin.content_versions SET summary = 'A much better summary' WHERE id = $1`, versionID,
	); err != nil {
		t.Fatal(err)
	}

	indexed, _, err := indexer.IndexVersion(ctx, versionID)
	if err != nil {
		t.Fatalf("IndexVersion: %v", err)
	}
	if !indexed || embedder.calls.Load() == before {
		t.Fatal("a changed document was not re-embedded")
	}
}

func TestAModelChangeInvalidatesStoredVectors(t *testing.T) {
	indexer, pool, _ := testIndexer(t)
	ctx := context.Background()

	versionID := seedVersion(t, pool, "cafes", "a chunk")
	if _, _, err := indexer.IndexVersion(ctx, versionID); err != nil {
		t.Fatalf("IndexVersion: %v", err)
	}

	// Pretend the stored vectors came from an older model.
	pool.Exec(ctx, `UPDATE reelpin.content_versions SET embedding_model = 'older-model'`)
	pool.Exec(ctx, `UPDATE reelpin.content_chunks SET embedding_model = 'older-model'`)

	report, err := indexer.Backfill(ctx, true, 100)
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if report.VersionsIndexed != 1 || report.ChunksIndexed != 1 {
		t.Fatalf("report = %+v, want both re-embedded", report)
	}
}

func TestBackfillIsDryRunByDefaultAndResumable(t *testing.T) {
	indexer, pool, embedder := testIndexer(t)
	ctx := context.Background()

	for _, title := range []string{"a", "b", "c"} {
		seedVersion(t, pool, title)
	}

	dry, err := indexer.Backfill(ctx, false, 100)
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if dry.VersionsSeen != 3 || dry.VersionsIndexed != 3 {
		t.Fatalf("dry run = %+v", dry)
	}
	if embedder.calls.Load() != 0 {
		t.Fatal("a dry run called the provider")
	}

	// A bounded pass leaves the rest for the next one.
	first, err := indexer.Backfill(ctx, true, 2)
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if first.VersionsIndexed != 2 {
		t.Fatalf("first pass = %+v", first)
	}
	second, err := indexer.Backfill(ctx, true, 100)
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if second.VersionsIndexed != 1 {
		t.Fatalf("second pass = %+v, want the remaining one", second)
	}
	// And a third finds nothing to do.
	third, err := indexer.Backfill(ctx, true, 100)
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	if third.VersionsSeen != 0 {
		t.Fatalf("third pass = %+v", third)
	}
}

func TestOneBadRowDoesNotStopABackfill(t *testing.T) {
	indexer, pool, embedder := testIndexer(t)
	ctx := context.Background()
	seedVersion(t, pool, "a")
	seedVersion(t, pool, "b")

	embedder.err = errors.New("the provider refused")
	report, err := indexer.Backfill(ctx, true, 100)
	if err != nil {
		t.Fatalf("Backfill returned an error instead of counting failures: %v", err)
	}
	if report.Failures != 2 || report.VersionsIndexed != 0 {
		t.Fatalf("report = %+v", report)
	}
}

func TestVectorsAreQueryableAsVectors(t *testing.T) {
	indexer, pool, _ := testIndexer(t)
	ctx := context.Background()

	versionID := seedVersion(t, pool, "cafes")
	if _, _, err := indexer.IndexVersion(ctx, versionID); err != nil {
		t.Fatalf("IndexVersion: %v", err)
	}

	// The point of storing a vector is cosine distance, so prove the column
	// really is one.
	var distance float64
	probe := make([]float32, Dimension)
	probe[0] = 1
	if err := pool.QueryRow(ctx,
		`SELECT embedding <=> $2::vector FROM reelpin.content_versions WHERE id = $1`,
		versionID, Vector(probe),
	).Scan(&distance); err != nil {
		t.Fatalf("cosine distance: %v", err)
	}
	if distance < 0 || distance > 2 {
		t.Fatalf("distance = %v, outside the cosine range", distance)
	}
}
