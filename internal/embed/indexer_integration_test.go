//go:build integration

package embed

import (
	"context"
	"errors"
	"fmt"
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

// countingEmbedder returns a deterministic vector and counts provider calls,
// which is the whole point: a duplicate index event must not cost a second one.
type countingEmbedder struct {
	calls     atomic.Int64
	dimension int
	model     string
	err       error
	// wrongSize makes the provider return a vector the index cannot hold.
	wrongSize bool
}

func (c *countingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	c.calls.Add(1)
	if c.err != nil {
		return nil, c.err
	}
	size := c.dimension
	if c.wrongSize {
		size = c.dimension / 2
	}
	out := make([][]float32, 0, len(texts))
	for range texts {
		vector := make([]float32, size)
		if size > 0 {
			vector[0] = 1
		}
		out = append(out, vector)
	}
	return out, nil
}

func (c *countingEmbedder) Model() string  { return c.model }
func (c *countingEmbedder) Dimension() int { return c.dimension }

func testIndexer(t *testing.T) (*Indexer, *countingEmbedder, *pgxpool.Pool) {
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

	parsed, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + name

	pool, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		t.Fatalf("connecting to %s: %v", name, err)
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

	if _, err := pool.Exec(ctx, `
		CREATE SCHEMA auth;
		CREATE TABLE auth.users (id UUID PRIMARY KEY, email TEXT, created_at TIMESTAMPTZ DEFAULT now())`); err != nil {
		t.Fatalf("creating auth.users: %v", err)
	}
	if _, err := migrations.Up(ctx, parsed.String()); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	embedder := &countingEmbedder{dimension: DefaultDimension, model: DefaultModel}
	return NewIndexer(pool, embedder, slog.New(slog.NewJSONHandler(io.Discard, nil))), embedder, pool
}

func seedVersion(t *testing.T, pool *pgxpool.Pool, sourceID, title string) string {
	t.Helper()
	ctx := context.Background()

	var contentID, versionID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO reelpin.contents
			(source_platform, source_content_type, source_content_id,
			 normalized_url, normalized_url_hash, access_scope_hash)
		VALUES ('instagram', 'reel', $1, 'https://example.com/'||$1, $1, 'public')
		RETURNING id::text`, sourceID).Scan(&contentID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO reelpin.content_versions
			(content_id, processor_version, prompt_version, schema_version, model_version,
			 title, summary, tags, key_facts)
		VALUES ($1, 'v1', 'p1', 's1', 'm1', $2, 'A summary', '{cafe}', '{"Opens at eight"}')
		RETURNING id::text`, contentID, title).Scan(&versionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE reelpin.contents SET current_version_id = $2 WHERE id = $1`, contentID, versionID); err != nil {
		t.Fatal(err)
	}
	return versionID
}

func TestADuplicateIndexEventDoesNotCallTheProviderTwice(t *testing.T) {
	indexer, embedder, pool := testIndexer(t)
	ctx := context.Background()

	versionID := seedVersion(t, pool, "ONE1", "Artjuna cafe")

	if err := indexer.IndexVersion(ctx, versionID); err != nil {
		t.Fatalf("first index: %v", err)
	}
	if err := indexer.IndexVersion(ctx, versionID); err != nil {
		t.Fatalf("second index: %v", err)
	}
	if calls := embedder.calls.Load(); calls != 1 {
		t.Fatalf("the provider was called %d times for one unchanged document", calls)
	}

	var model string
	var dimension int
	var documentVersion string
	if err := pool.QueryRow(ctx, `
		SELECT model, dimension, document_version
		FROM reelpin.content_embeddings WHERE content_version_id = $1`, versionID,
	).Scan(&model, &dimension, &documentVersion); err != nil {
		t.Fatal(err)
	}
	if model != DefaultModel || dimension != DefaultDimension || documentVersion != DocumentVersion {
		t.Fatalf("row = %s/%d/%s; every vector must record how it was made",
			model, dimension, documentVersion)
	}
}

func TestChangingTheModelReEmbeds(t *testing.T) {
	indexer, embedder, pool := testIndexer(t)
	ctx := context.Background()

	versionID := seedVersion(t, pool, "TWO1", "Artjuna cafe")
	if err := indexer.IndexVersion(ctx, versionID); err != nil {
		t.Fatal(err)
	}

	// A different model is a different vector space; the old vector is not
	// comparable and must be replaced.
	embedder.model = "some-other-model"
	if err := indexer.IndexVersion(ctx, versionID); err != nil {
		t.Fatal(err)
	}
	if calls := embedder.calls.Load(); calls != 2 {
		t.Fatalf("provider calls = %d, want a re-embed on a model change", calls)
	}

	var model string
	if err := pool.QueryRow(ctx,
		`SELECT model FROM reelpin.content_embeddings WHERE content_version_id = $1`, versionID).Scan(&model); err != nil {
		t.Fatal(err)
	}
	if model != "some-other-model" {
		t.Fatalf("model = %q, want the new one", model)
	}
}

func TestAWrongSizedVectorIsRefused(t *testing.T) {
	indexer, embedder, pool := testIndexer(t)
	ctx := context.Background()
	embedder.wrongSize = true

	versionID := seedVersion(t, pool, "THREE1", "Artjuna cafe")
	err := indexer.IndexVersion(ctx, versionID)
	if !errors.Is(err, ErrDimensionMismatch) {
		t.Fatalf("err = %v, want the dimension mismatch: storing it would corrupt the set", err)
	}

	var rows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM reelpin.content_embeddings WHERE content_version_id = $1`, versionID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatal("a mismatched vector was stored anyway")
	}
}

func TestNothingWorthEmbeddingIsRecordedNotRetriedForever(t *testing.T) {
	indexer, embedder, pool := testIndexer(t)
	ctx := context.Background()

	var contentID, versionID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO reelpin.contents
			(source_platform, source_content_type, source_content_id,
			 normalized_url, normalized_url_hash, access_scope_hash)
		VALUES ('instagram', 'reel', 'EMPTY1', 'https://example.com/EMPTY1', 'EMPTY1', 'public')
		RETURNING id::text`).Scan(&contentID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO reelpin.content_versions
			(content_id, processor_version, prompt_version, schema_version, model_version)
		VALUES ($1, 'v1', 'p1', 's1', 'm1')
		RETURNING id::text`, contentID).Scan(&versionID); err != nil {
		t.Fatal(err)
	}

	if err := indexer.IndexVersion(ctx, versionID); err != nil {
		t.Fatalf("index: %v", err)
	}
	if calls := embedder.calls.Load(); calls != 0 {
		t.Fatalf("the provider was called %d times for an empty document", calls)
	}

	var hash string
	if err := pool.QueryRow(ctx,
		`SELECT document_hash FROM reelpin.content_embeddings WHERE content_version_id = $1`, versionID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if hash != "empty" {
		t.Fatalf("hash = %q, want the attempt recorded", hash)
	}
}

func TestBackfillResumesFromItsCheckpoint(t *testing.T) {
	indexer, embedder, pool := testIndexer(t)
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		seedVersion(t, pool, fmt.Sprintf("BULK%02d", i), fmt.Sprintf("Reel %d", i))
	}

	first, err := indexer.Backfill(ctx, "", 4)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if first.Scanned != 4 || first.Embedded != 4 {
		t.Fatalf("first pass = %+v", first)
	}

	second, err := indexer.Backfill(ctx, first.LastID, 4)
	if err != nil {
		t.Fatal(err)
	}
	third, err := indexer.Backfill(ctx, second.LastID, 4)
	if err != nil {
		t.Fatal(err)
	}
	if total := first.Embedded + second.Embedded + third.Embedded; total != 10 {
		t.Fatalf("embedded %d of 10 across three passes", total)
	}

	// Rerunning from the start finds nothing left and spends nothing.
	before := embedder.calls.Load()
	again, err := indexer.Backfill(ctx, "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if again.Scanned != 0 {
		t.Fatalf("a rerun scanned %d rows that are already embedded", again.Scanned)
	}
	if embedder.calls.Load() != before {
		t.Fatal("a rerun cost provider calls")
	}
}

func TestOneFailureDoesNotStopTheBatch(t *testing.T) {
	indexer, embedder, pool := testIndexer(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		seedVersion(t, pool, fmt.Sprintf("FAIL%d", i), fmt.Sprintf("Reel %d", i))
	}
	embedder.err = errors.New("the provider is away")

	report, err := indexer.Backfill(ctx, "", 10)
	if err != nil {
		t.Fatalf("the pass returned an error instead of a report: %v", err)
	}
	if report.Failed != 3 || report.Embedded != 0 {
		t.Fatalf("report = %+v", report)
	}
	if report.LastID == "" {
		t.Error("no checkpoint to resume from after failures")
	}
}

func TestTheVectorIndexIsUsable(t *testing.T) {
	indexer, _, pool := testIndexer(t)
	ctx := context.Background()

	versionID := seedVersion(t, pool, "SEARCH1", "Artjuna cafe")
	if err := indexer.IndexVersion(ctx, versionID); err != nil {
		t.Fatal(err)
	}

	// The stored vector is readable back through a cosine-distance query,
	// which is what search will do.
	probe := make([]float32, DefaultDimension)
	probe[0] = 1
	var distance float64
	if err := pool.QueryRow(ctx, `
		SELECT embedding <=> $1::vector FROM reelpin.content_embeddings
		WHERE content_version_id = $2`,
		Vector(probe), versionID).Scan(&distance); err != nil {
		t.Fatalf("querying by cosine distance: %v", err)
	}
	if distance > 0.0001 {
		t.Fatalf("distance to an identical vector = %g", distance)
	}
}
