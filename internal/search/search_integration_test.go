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

// seedCounter keeps every seeded save a distinct piece of source content.
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

func (v *vectorEmbedder) Model() string  { return embed.DefaultModel }
func (v *vectorEmbedder) Dimension() int { return embed.DefaultDimension }

func (v *vectorEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	v.calls++
	if v.err != nil {
		return nil, v.err
	}
	out := make([][]float32, 0, len(texts))
	for _, text := range texts {
		vector, ok := v.vectors[text]
		if !ok {
			vector = axis(0)
		}
		out = append(out, vector)
	}
	return out, nil
}

func axis(index int) []float32 {
	vector := make([]float32, embed.DefaultDimension)
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

	// Supabase owns auth.users in every real deployment, and user_saves points
	// at it, so the shape has to exist before the migrations run.
	if _, err := pool.Exec(ctx, `
		CREATE SCHEMA auth;
		CREATE TABLE auth.users (
			id         UUID PRIMARY KEY,
			email      TEXT,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		t.Fatalf("creating the production-shaped auth schema: %v", err)
	}
	if _, err := migrations.Up(ctx, parsed.String()); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO auth.users (id) VALUES ($1), ($2)`, userA, userB); err != nil {
		t.Fatalf("seeding users: %v", err)
	}

	embedder := &vectorEmbedder{vectors: map[string][]float32{}}
	return NewService(pool, embedder, slog.New(slog.NewJSONHandler(io.Discard, nil)), time.Now), pool, embedder
}

type seed struct {
	userID      string
	url         string
	title       string
	summary     string
	caption     string
	category    string
	subcategory string
	platform    string
	vectorAxis  int
	// noEmbedding leaves the sidecar row out, which is what a version the
	// indexer has not reached yet looks like.
	noEmbedding bool
}

// seedSave writes one content, one version, its embedding and one user's save,
// mirroring what the pipeline and the indexer write between them.
func seedSave(t *testing.T, pool *pgxpool.Pool, s seed) string {
	t.Helper()
	ctx := context.Background()

	if s.category == "" {
		s.category = "food"
	}
	if s.subcategory == "" {
		s.subcategory = "cafes"
	}
	if s.platform == "" {
		s.platform = "instagram"
	}

	// Two users saving the same title must not collide on the shared content
	// row, so every seeded save gets its own source identity.
	identity := fmt.Sprintf("%s-%d",
		strings.ToLower(strings.ReplaceAll(s.title, " ", "-")), seedCounter.Add(1))
	if s.url == "" {
		s.url = "https://example.com/" + identity
	}

	var contentID, versionID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO reelpin.contents
			(source_platform, source_content_type, source_content_id,
			 normalized_url, normalized_url_hash, access_scope_hash)
		VALUES ($1, 'reel', $2, $3, $2, 'public')
		RETURNING id::text`, s.platform, identity, s.url,
	).Scan(&contentID); err != nil {
		t.Fatalf("seeding content: %v", err)
	}

	if err := pool.QueryRow(ctx, `
		INSERT INTO reelpin.content_versions
			(content_id, processor_version, prompt_version, schema_version, model_version,
			 title, summary, caption, raw_extraction)
		VALUES ($1, 'v1', 'p1', 's1', 'm1', $2, $3, $4,
		        jsonb_build_object('category', $5::text, 'subcategory', $6::text))
		RETURNING id::text`,
		contentID, s.title, s.summary, s.caption, s.category, s.subcategory,
	).Scan(&versionID); err != nil {
		t.Fatalf("seeding a version: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE reelpin.contents SET current_version_id = $1 WHERE id = $2`,
		versionID, contentID); err != nil {
		t.Fatalf("pointing content at its version: %v", err)
	}

	if !s.noEmbedding {
		seedEmbedding(t, pool, versionID, axis(s.vectorAxis))
	}

	var saveID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO reelpin.user_saves (user_id, content_id) VALUES ($1, $2)
		RETURNING id::text`, s.userID, contentID).Scan(&saveID); err != nil {
		t.Fatalf("seeding a save: %v", err)
	}
	return saveID
}

func seedEmbedding(t *testing.T, pool *pgxpool.Pool, versionID string, vector []float32) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO reelpin.content_embeddings
			(content_version_id, embedding, model, dimension, document_version, document_hash)
		VALUES ($1, $2::vector, $3, $4, $5, $6)`,
		versionID, embed.Vector(vector), embed.DefaultModel, embed.DefaultDimension,
		embed.DocumentVersion, versionID); err != nil {
		t.Fatalf("seeding an embedding: %v", err)
	}
}

func TestSearchFindsByMeaningWordsAndSpelling(t *testing.T) {
	service, pool, embedder := testService(t)
	ctx := context.Background()

	// One save matches by vector, one by exact words, and one belongs to
	// somebody else.
	byMeaning := seedSave(t, pool, seed{userID: userA, title: "Quiet garden spot", summary: "A calm place", vectorAxis: 5})
	byWords := seedSave(t, pool, seed{userID: userA, title: "Artjuna cafe", summary: "Coffee and pancakes", vectorAxis: 9})
	seedSave(t, pool, seed{userID: userB, title: "Artjuna cafe", summary: "Someone else's", vectorAxis: 9})

	// The query vector points at the first save's axis.
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
			t.Fatal("another user's save appeared in the results")
		}
	}

	found := map[string]bool{}
	for _, result := range response.Results {
		found[result.Reel.ID] = true
	}
	if !found[byMeaning] || !found[byWords] {
		t.Fatalf("results = %v, want both the vector match and the word match", found)
	}
	if !strings.Contains(response.SearchMode, ArmDense) || !strings.Contains(response.SearchMode, ArmSparse) {
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

	seedSave(t, pool, seed{userID: userA, title: "Artjuna cafe", summary: "Coffee", vectorAxis: 3})
	embedder.err = errors.New("the embedding provider is down")

	response, err := service.Search(ctx, userA, "artjuna", Filters{}, 10)
	if err != nil {
		t.Fatalf("a provider outage failed the search: %v", err)
	}
	if len(response.Results) == 0 {
		t.Fatal("no results without a query vector: words and spelling should still find it")
	}
	if strings.Contains(response.SearchMode, ArmDense) {
		t.Errorf("search mode = %q, want the dense arm reported as not run", response.SearchMode)
	}
}

func TestMisspellingsAreFound(t *testing.T) {
	service, pool, embedder := testService(t)
	ctx := context.Background()

	seedSave(t, pool, seed{userID: userA, title: "Artjuna cafe", summary: "Coffee", vectorAxis: 3})
	embedder.err = errors.New("no vectors, so only words and spelling")

	// "artjunna" is not a word in the row, so only the trigram arm can find it.
	response, err := service.Search(ctx, userA, "artjunna", Filters{}, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(response.Results) != 1 {
		t.Fatalf("results = %d, want the misspelled query to still find it", len(response.Results))
	}
	if !strings.Contains(response.SearchMode, ArmFuzzy) {
		t.Errorf("search mode = %q, want fuzzy", response.SearchMode)
	}
}

// A version the indexer has not reached has no sidecar row at all, which is
// the common state right after a save. It must not disappear from search.
func TestASaveWithNoEmbeddingIsStillFoundByWords(t *testing.T) {
	service, pool, embedder := testService(t)
	embedder.vectors["artjuna cafe"] = axis(3)

	seedSave(t, pool, seed{userID: userA, title: "Artjuna cafe", summary: "Coffee", noEmbedding: true})

	response, err := service.Search(context.Background(), userA, "artjuna cafe", Filters{}, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(response.Results) != 1 {
		t.Fatalf("results = %d, want the unindexed save found by words", len(response.Results))
	}
	if strings.Contains(response.SearchMode, ArmDense) {
		t.Errorf("search mode = %q: there is no vector to match", response.SearchMode)
	}
}

// Vectors made by another model are not comparable with this one's, so the
// dense arm must not rank them at all.
func TestVectorsFromAnotherModelAreIgnored(t *testing.T) {
	service, pool, embedder := testService(t)
	ctx := context.Background()

	seedSave(t, pool, seed{userID: userA, title: "Quiet garden spot", summary: "A calm place", vectorAxis: 5})
	if _, err := pool.Exec(ctx,
		`UPDATE reelpin.content_embeddings SET model = 'some-other-model'`); err != nil {
		t.Fatalf("restamping the embedding: %v", err)
	}
	embedder.vectors["a calm quiet place"] = axis(5)

	response, err := service.Search(ctx, userA, "a calm quiet place", Filters{}, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if strings.Contains(response.SearchMode, ArmDense) {
		t.Errorf("search mode = %q, want the vector arm to skip a foreign model", response.SearchMode)
	}
}

func TestFiltersApplyInsideTheSearch(t *testing.T) {
	service, pool, embedder := testService(t)
	ctx := context.Background()

	seedSave(t, pool, seed{userID: userA, title: "Artjuna cafe", category: "food", platform: "instagram", vectorAxis: 4})
	seedSave(t, pool, seed{userID: userA, title: "Artjuna trek", category: "travel", platform: "youtube", vectorAxis: 4})
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

	seedSave(t, pool, seed{userID: userA, title: "Artjuna cafe", summary: "Coffee", vectorAxis: 3})
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
	seedSave(t, pool, seed{userID: userA, title: "Artjuna cafe", summary: "Coffee", vectorAxis: 3})
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

func TestCoverageRefusesALibraryTheSetCannotMeasure(t *testing.T) {
	service, pool, _ := testService(t)
	ctx := context.Background()

	set := LabeledSet{Queries: []LabeledQuery{
		{ID: "one", Query: "artjuna", Relevant: map[string]int{"https://example.com/eval/artjuna": 3}},
		{ID: "two", Query: "trek", Relevant: map[string]int{"https://example.com/eval/trek": 3}},
		{ID: "none", Query: "nothing", Relevant: map[string]int{}},
	}}

	if judged := set.JudgedURLs(); len(judged) != 2 {
		t.Fatalf("judged = %v, want the two reels with opinions", judged)
	}

	// An empty library holds none of them, which is the case that would
	// otherwise score zero everywhere and look like a search regression.
	present, total, err := service.Coverage(ctx, userA, set)
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	if present != 0 || total != 2 {
		t.Fatalf("coverage = %d of %d, want none present", present, total)
	}

	seedSave(t, pool, seed{userID: userA, url: "https://example.com/eval/artjuna", title: "Artjuna cafe", vectorAxis: 1})

	present, total, err = service.Coverage(ctx, userA, set)
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	if present != 1 || total != 2 {
		t.Fatalf("coverage = %d of %d, want one of two", present, total)
	}

	// Another user holding the reel does not count for this one.
	if other, _, err := service.Coverage(ctx, userB, set); err != nil || other != 0 {
		t.Fatalf("coverage for another user = %d (%v), want none", other, err)
	}
}
