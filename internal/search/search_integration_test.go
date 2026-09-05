//go:build integration

package search

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
	"time"

	"github.com/XploY04/reelpin-go/internal/embed"
	"github.com/XploY04/reelpin-go/internal/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

const legacySchema = `
CREATE TABLE public.reels (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id TEXT NOT NULL,
    url TEXT NOT NULL,
    normalized_url TEXT,
    source_platform TEXT,
    source_content_type TEXT,
    source_content_id TEXT,
    processing_version TEXT,
    ingestion_method TEXT,
    transcript_source TEXT,
    thumbnail_url TEXT,
    title TEXT NOT NULL DEFAULT 'Untitled',
    summary TEXT DEFAULT '',
    transcript TEXT DEFAULT '',
    category TEXT DEFAULT 'Other',
    subcategory TEXT DEFAULT 'Other',
    secondary_categories JSONB DEFAULT '[]'::jsonb,
    key_facts JSONB DEFAULT '[]'::jsonb,
    locations JSONB DEFAULT '[]'::jsonb,
    people_mentioned JSONB DEFAULT '[]'::jsonb,
    actionable_items JSONB DEFAULT '[]'::jsonb,
    events JSONB NOT NULL DEFAULT '[]'::jsonb,
    parse_status TEXT NOT NULL DEFAULT 'parsed',
    created_at TIMESTAMPTZ DEFAULT now()
);
`

// seedCounter keeps every seeded reel a distinct piece of source content.
var seedCounter atomic.Int64

const (
	userA = "11111111-1111-4111-8111-111111111111"
	userB = "22222222-2222-4222-8222-222222222222"
)

// vectorEmbedder returns a fixed vector per text, so dense ranking is
// deterministic and no provider is called.
type vectorEmbedder struct {
	vectors map[string][]float32
	err     error
	calls   int
}

func (v *vectorEmbedder) Embed(_ context.Context, texts []string, _ embed.TaskType) ([][]float32, error) {
	v.calls++
	if v.err != nil {
		return nil, v.err
	}
	out := make([][]float32, 0, len(texts))
	for _, text := range texts {
		vector, ok := v.vectors[text]
		if !ok {
			vector = make([]float32, embed.Dimension)
			vector[0] = 1
		}
		out = append(out, vector)
	}
	return out, nil
}

func axis(index int) []float32 {
	vector := make([]float32, embed.Dimension)
	vector[index] = 1
	return vector
}

func testService(t *testing.T) (*Service, *pgxpool.Pool, *vectorEmbedder) {
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

	name := "reelpin_search_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
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

	if _, err := pool.Exec(ctx, legacySchema); err != nil {
		t.Fatalf("legacy schema: %v", err)
	}
	if _, err := migrations.Up(ctx, parsed.String()); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	embedder := &vectorEmbedder{vectors: map[string][]float32{}}
	return NewService(pool, embedder, slog.New(slog.NewJSONHandler(io.Discard, nil)), time.Now), pool, embedder
}

type seed struct {
	userID     string
	title      string
	summary    string
	category   string
	platform   string
	locations  string
	transcript string
	vectorAxis int
	chunk      string
	chunkAxis  int
}

func seedReel(t *testing.T, pool *pgxpool.Pool, s seed) string {
	t.Helper()
	ctx := context.Background()

	if s.category == "" {
		s.category = "food"
	}
	if s.platform == "" {
		s.platform = "instagram"
	}
	if s.locations == "" {
		s.locations = "[]"
	}

	// Each seeded reel needs its own source identity: two users saving the same
	// title must not collide on the shared content row.
	identity := fmt.Sprintf("%s-%d", strings.ToLower(strings.ReplaceAll(s.title, " ", "-")), seedCounter.Add(1))

	var versionID string
	if err := pool.QueryRow(ctx, `
		WITH content AS (
			INSERT INTO reelpin.contents
				(source_platform, source_content_type, source_content_id, normalized_url, normalized_url_hash)
			VALUES ($1,'reel',$2,'https://www.instagram.com/reel/'||$2||'/',$2)
			RETURNING id
		)
		INSERT INTO reelpin.content_versions
			(content_id, processor_version, extraction_schema_version, title, summary,
			 embedding, embedding_model, embedding_dimension, embedding_document_version)
		SELECT id, 'v1','v1',$3,$4,$5::vector,$6,$7,$8 FROM content
		RETURNING id::text`,
		s.platform, identity, s.title, s.summary,
		embed.Vector(axis(s.vectorAxis)), embed.Model, embed.Dimension, embed.DocumentVersion,
	).Scan(&versionID); err != nil {
		t.Fatalf("seeding content: %v", err)
	}

	if s.chunk != "" {
		if _, err := pool.Exec(ctx, `
			INSERT INTO reelpin.content_chunks
				(content_version_id, ordinal, chunk_text, content_hash,
				 embedding, embedding_model, embedding_dimension, embedding_document_version)
			VALUES ($1, 0, $2, 'hash', $3::vector, $4, $5, $6)`,
			versionID, s.chunk, embed.Vector(axis(s.chunkAxis)),
			embed.Model, embed.Dimension, embed.DocumentVersion); err != nil {
			t.Fatalf("seeding a chunk: %v", err)
		}
	}

	var reelID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO public.reels
			(user_id, url, normalized_url, source_platform, title, summary, transcript,
			 category, subcategory, locations, content_version_id)
		VALUES ($1, 'https://example.com/'||$9, 'https://example.com/'||$9, $3, $2, $4, $5,
		        $6, 'cafes', $7::jsonb, $8)
		RETURNING id::text`,
		s.userID, s.title, s.platform, s.summary, s.transcript, s.category, s.locations, versionID, identity,
	).Scan(&reelID); err != nil {
		t.Fatalf("seeding a reel: %v", err)
	}
	return reelID
}

func TestSearchFindsByMeaningWordsAndSpelling(t *testing.T) {
	service, pool, embedder := testService(t)
	ctx := context.Background()

	// One reel matches by vector, one by exact words, one by a misspelling.
	byMeaning := seedReel(t, pool, seed{userID: userA, title: "Quiet garden spot", summary: "A calm place", vectorAxis: 5})
	byWords := seedReel(t, pool, seed{userID: userA, title: "Artjuna cafe", summary: "Coffee and pancakes", vectorAxis: 9})
	seedReel(t, pool, seed{userID: userB, title: "Artjuna cafe", summary: "Someone else's", vectorAxis: 9})

	// The query vector points at the first reel's axis.
	embedder.vectors["artjuna cafe"] = axis(5)

	response, err := service.Search(ctx, userA, "artjuna cafe", Filters{}, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(response.Results) != 2 {
		t.Fatalf("results = %d, want this user's two", len(response.Results))
	}
	for _, result := range response.Results {
		if strings.Contains(result.Reel.Summary, "Someone else") {
			t.Fatal("another user's reel appeared in the results")
		}
	}

	found := map[string]bool{}
	for _, result := range response.Results {
		found[result.Reel.ID] = true
	}
	if !found[byMeaning] || !found[byWords] {
		t.Fatalf("results = %v, want both the vector match and the word match", found)
	}
	if !strings.Contains(response.SearchMode, "dense") || !strings.Contains(response.SearchMode, "sparse") {
		t.Errorf("search mode = %q, want the arms that ran", response.SearchMode)
	}

	// Relevance is display-relative: the top hit anchors it.
	if response.Results[0].RelevancePercent != 100 {
		t.Errorf("top result = %d%%, want 100", response.Results[0].RelevancePercent)
	}
	if response.Results[0].DisplayScoreLabel == "" {
		t.Error("no score label")
	}
}

func TestAMissingQueryVectorStillSearches(t *testing.T) {
	service, pool, embedder := testService(t)
	ctx := context.Background()

	seedReel(t, pool, seed{userID: userA, title: "Artjuna cafe", summary: "Coffee", vectorAxis: 3})
	embedder.err = errors.New("the embedding provider is down")

	response, err := service.Search(ctx, userA, "artjuna", Filters{}, 10)
	if err != nil {
		t.Fatalf("a provider outage failed the search: %v", err)
	}
	if len(response.Results) == 0 {
		t.Fatal("no results without a query vector: words and spelling should still find it")
	}
	if strings.Contains(response.SearchMode, "dense") {
		t.Errorf("search mode = %q, want the dense arm reported as not run", response.SearchMode)
	}
}

func TestMisspellingsAreFound(t *testing.T) {
	service, pool, embedder := testService(t)
	ctx := context.Background()

	seedReel(t, pool, seed{userID: userA, title: "Artjuna cafe", summary: "Coffee", vectorAxis: 3})
	embedder.err = errors.New("no vectors, so only words and spelling")

	// "artjunna" is not a word in the row, so only the trigram arm can find it.
	response, err := service.Search(ctx, userA, "artjunna", Filters{}, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(response.Results) != 1 {
		t.Fatalf("results = %d, want the misspelled query to still find it", len(response.Results))
	}
	if !strings.Contains(response.SearchMode, "fuzzy") {
		t.Errorf("search mode = %q, want fuzzy", response.SearchMode)
	}
}

func TestChunksMakeASpecificLineFindable(t *testing.T) {
	service, pool, embedder := testService(t)
	ctx := context.Background()

	// The content vector is far from the query; one chunk is close.
	reelID := seedReel(t, pool, seed{
		userID: userA, title: "A long vlog", summary: "About many things",
		vectorAxis: 100, chunk: "the bakery opens at six", chunkAxis: 7,
	})
	embedder.vectors["bakery hours"] = axis(7)

	response, err := service.Search(ctx, userA, "bakery hours", Filters{}, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(response.Results) != 1 || response.Results[0].Reel.ID != reelID {
		t.Fatalf("results = %+v, want the reel found through its chunk", response.Results)
	}
}

func TestFiltersApplyInsideTheSearch(t *testing.T) {
	service, pool, embedder := testService(t)
	ctx := context.Background()

	seedReel(t, pool, seed{userID: userA, title: "Artjuna cafe", category: "food", platform: "instagram", vectorAxis: 4})
	seedReel(t, pool, seed{userID: userA, title: "Artjuna trek", category: "travel", platform: "youtube", vectorAxis: 4})
	embedder.vectors["artjuna"] = axis(4)

	byCategory, err := service.Search(ctx, userA, "artjuna", Filters{Category: "food"}, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(byCategory.Results) != 1 || byCategory.Results[0].Reel.Category != "food" {
		t.Fatalf("category filter = %+v", byCategory.Results)
	}

	byPlatform, err := service.Search(ctx, userA, "artjuna", Filters{Platforms: []string{"youtube"}}, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(byPlatform.Results) != 1 || byPlatform.Results[0].Reel.Title != "Artjuna trek" {
		t.Fatalf("platform filter = %+v", byPlatform.Results)
	}
}

func TestShortQueriesDoNotSearch(t *testing.T) {
	service, _, embedder := testService(t)

	response, err := service.Search(context.Background(), userA, "a", Filters{}, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if response.SearchMode != "empty" || len(response.Results) != 0 {
		t.Fatalf("response = %+v", response)
	}
	if embedder.calls != 0 {
		t.Error("a one-character query cost a provider call")
	}
}

func TestAnEmptyLibraryReturnsAnEmptyList(t *testing.T) {
	service, _, _ := testService(t)

	response, err := service.Search(context.Background(), userA, "anything at all", Filters{}, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if response.Results == nil {
		t.Fatal("results is nil, which serializes as null")
	}
	if response.Total != 0 {
		t.Errorf("total = %d", response.Total)
	}
}

func TestAQueryAboutSomethingUnsavedComesBackEmpty(t *testing.T) {
	service, pool, embedder := testService(t)

	seedReel(t, pool, seed{userID: userA, title: "Artjuna cafe", summary: "Coffee", vectorAxis: 3})
	// An unrelated query: orthogonal to everything saved, and sharing no words.
	embedder.vectors["scuba diving in switzerland"] = axis(500)

	response, err := service.Search(context.Background(), userA, "scuba diving in switzerland", Filters{}, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(response.Results) != 0 {
		t.Fatalf("results = %+v, want nothing: the vector arm has a relevance gate", response.Results)
	}
}

func TestTheDenseGateCanBeOpened(t *testing.T) {
	service, pool, embedder := testService(t)
	seedReel(t, pool, seed{userID: userA, title: "Artjuna cafe", summary: "Coffee", vectorAxis: 3})
	embedder.vectors["scuba diving in switzerland"] = axis(500)

	service.MaxDistance = 1
	response, err := service.Search(context.Background(), userA, "scuba diving in switzerland", Filters{}, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(response.Results) != 1 {
		t.Fatalf("results = %d, want the gate to be what held it back", len(response.Results))
	}
}
