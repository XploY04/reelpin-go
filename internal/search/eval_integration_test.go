//go:build integration

package search

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/embed"
	"github.com/jackc/pgx/v5/pgxpool"
)

// bagOfWords stands in for Gemini so the harness runs offline and
// deterministically. It is lexical, not semantic, so the numbers it produces
// measure the harness and the SQL, never Gemini's relevance. The measured
// report comes from the same tests run with GEMINI_API_KEY set.
type bagOfWords struct{ calls int }

func (b *bagOfWords) Model() string  { return embed.DefaultModel }
func (b *bagOfWords) Dimension() int { return embed.DefaultDimension }

func (b *bagOfWords) Embed(_ context.Context, texts []string) ([][]float32, error) {
	b.calls++
	out := make([][]float32, 0, len(texts))
	for _, text := range texts {
		vector := make([]float32, embed.DefaultDimension)
		for _, word := range strings.Fields(strings.ToLower(text)) {
			digest := fnv.New32a()
			digest.Write([]byte(strings.Trim(word, ".,!?'\"")))
			vector[digest.Sum32()%embed.DefaultDimension]++
		}
		out = append(out, vector)
	}
	return out, nil
}

type corpusReel struct {
	URL         string   `json:"url"`
	Platform    string   `json:"platform"`
	Title       string   `json:"title"`
	Summary     string   `json:"summary"`
	Caption     string   `json:"caption"`
	Category    string   `json:"category"`
	Subcategory string   `json:"subcategory"`
	Tags        []string `json:"tags"`
	Facts       []string `json:"facts"`
	Places      []string `json:"places"`
	Transcript  string   `json:"transcript"`
	SavedAt     string   `json:"saved_at"`
}

type corpus struct {
	Version string       `json:"version"`
	Reels   []corpusReel `json:"reels"`
}

func loadCorpus(t *testing.T) corpus {
	t.Helper()
	raw, err := os.ReadFile("testdata/corpus-v1.json")
	if err != nil {
		t.Fatalf("reading the corpus: %v", err)
	}
	var library corpus
	if err := json.Unmarshal(raw, &library); err != nil {
		t.Fatalf("parsing the corpus: %v", err)
	}
	return library
}

// cachingEmbedder answers a text it has already seen without calling through.
// A sweep runs the same queries at every threshold, so without this a 41-point
// pass would pay the provider 41 times over and stop being comparable between
// passes.
type cachingEmbedder struct {
	inner  embed.Embedder
	cached map[string][]float32
}

func newCachingEmbedder(inner embed.Embedder) *cachingEmbedder {
	return &cachingEmbedder{inner: inner, cached: map[string][]float32{}}
}

func (c *cachingEmbedder) Model() string  { return c.inner.Model() }
func (c *cachingEmbedder) Dimension() int { return c.inner.Dimension() }

func (c *cachingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	missing := []string{}
	for _, text := range texts {
		if _, ok := c.cached[text]; !ok {
			missing = append(missing, text)
		}
	}
	// Chunked because the provider caps how many texts one batch may carry,
	// and the warm-up hands it the whole corpus and the whole query set at once.
	for start := 0; start < len(missing); start += 50 {
		end := min(start+50, len(missing))
		vectors, err := c.inner.Embed(ctx, missing[start:end])
		if err != nil {
			return nil, err
		}
		for index, text := range missing[start:end] {
			c.cached[text] = vectors[index]
		}
	}

	out := make([][]float32, 0, len(texts))
	for _, text := range texts {
		out = append(out, c.cached[text])
	}
	return out, nil
}

// evalEmbedder is the real provider when a key is present and the offline
// stand-in otherwise. The stand-in is lexical, so a run without a key measures
// the SQL and the fusion; only a run with a key measures relevance.
func evalEmbedder(t *testing.T) (*cachingEmbedder, string) {
	t.Helper()
	if key := os.Getenv("GEMINI_API_KEY"); key != "" {
		return newCachingEmbedder(embed.NewGemini(embed.GeminiConfig{
			APIKey:    key,
			Model:     embed.DefaultModel,
			Dimension: embed.DefaultDimension,
			Timeout:   2 * time.Minute,
		})), embed.DefaultModel
	}
	return newCachingEmbedder(&bagOfWords{}), "offline lexical stand-in"
}

func TestTheLabeledSetRunsAgainstTheCorpus(t *testing.T) {
	service, pool, _ := testService(t)
	embedder := &bagOfWords{}
	service.embedder = embedder
	// The stand-in is lexical, so its vectors sit far apart on the cosine
	// scale the real gate is tuned for. Open the gate here so the dense arm is
	// still exercised end to end.
	service.MaxDistance = 1
	ctx := context.Background()

	for _, reel := range loadCorpus(t).Reels {
		seedCorpusReel(t, pool, embedder, reel)
	}

	set, err := LoadLabeledSet("../../api/eval/search-v1.json")
	if err != nil {
		t.Fatalf("LoadLabeledSet: %v", err)
	}

	present, total, err := service.Coverage(ctx, userA, set)
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	if present != total {
		t.Fatalf("the seeded library holds %d of the set's %d judged reels", present, total)
	}

	report, _, err := service.Evaluate(ctx, userA, set)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	if report.Queries != len(set.Queries) {
		t.Fatalf("measured %d of %d queries", report.Queries, len(set.Queries))
	}
	if report.MRR == 0 {
		t.Fatal("no labeled query found its answer at any rank")
	}
	if report.DenseQueries != report.Queries {
		t.Errorf("the vector arm ran for %d of %d queries", report.DenseQueries, report.Queries)
	}
	if report.P95 == 0 {
		t.Error("no latency was measured")
	}

	// The comparison the task asks for, minus the Pinecone arm, which needs
	// production credentials: the same set with no vector arm at all.
	service.embedder = nil
	lexical, _, err := service.Evaluate(ctx, userA, set)
	if err != nil {
		t.Fatalf("lexical-only Evaluate: %v", err)
	}
	if lexical.DenseQueries != 0 {
		t.Errorf("the vector arm ran %d times with no embedder", lexical.DenseQueries)
	}

	// The reports are the artifact; print them so a CI run leaves the numbers
	// behind rather than only a pass.
	for _, measured := range []Report{report, lexical} {
		encoded, _ := json.Marshal(measured)
		t.Logf("search evaluation (offline, lexical stand-in embedder): %s", encoded)
	}
}

// seedCorpusReel saves one fixture reel for userA, embedding exactly the
// document the indexer would have built for it.
func seedCorpusReel(t *testing.T, pool *pgxpool.Pool, embedder embed.Embedder, reel corpusReel) {
	t.Helper()
	ctx := context.Background()

	identity := strings.TrimPrefix(reel.URL, "https://example.com/eval/")

	var contentID, versionID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO reelpin.contents
			(source_platform, source_content_type, source_content_id,
			 normalized_url, normalized_url_hash, access_scope_hash)
		VALUES ($1, 'reel', $2, $3, $2, 'public')
		RETURNING id::text`, reel.Platform, identity, reel.URL,
	).Scan(&contentID); err != nil {
		t.Fatalf("seeding %s: %v", reel.URL, err)
	}

	if err := pool.QueryRow(ctx, `
		INSERT INTO reelpin.content_versions
			(content_id, processor_version, prompt_version, schema_version, model_version,
			 title, summary, caption, transcript, tags, key_facts, raw_extraction)
		VALUES ($1, 'v1', 'p1', 's1', 'm1', $2, $3, $4, $5, $6, $7,
		        jsonb_build_object('category', $8::text, 'subcategory', $9::text,
		                           'places', $10::jsonb))
		RETURNING id::text`,
		contentID, reel.Title, reel.Summary, reel.Caption, reel.Transcript,
		reel.Tags, reel.Facts, reel.Category, reel.Subcategory, mustJSON(t, reel.Places),
	).Scan(&versionID); err != nil {
		t.Fatalf("seeding a version of %s: %v", reel.URL, err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE reelpin.contents SET current_version_id = $1 WHERE id = $2`,
		versionID, contentID); err != nil {
		t.Fatalf("pointing %s at its version: %v", reel.URL, err)
	}

	document := embed.Document(embed.Fields{
		Title:    reel.Title,
		Summary:  reel.Summary,
		Category: reel.Category,
		Tags:     reel.Tags,
		Facts:    reel.Facts,
		Places:   reel.Places,
	})
	vectors, err := embedder.Embed(ctx, []string{document})
	if err != nil {
		t.Fatalf("embedding %s: %v", reel.URL, err)
	}
	seedEmbedding(t, pool, versionID, vectors[0])

	if _, err := pool.Exec(ctx, `
		INSERT INTO reelpin.user_saves (user_id, content_id, saved_at)
		VALUES ($1, $2, $3::date)`, userA, contentID, reel.SavedAt); err != nil {
		t.Fatalf("saving %s: %v", reel.URL, err)
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encoding %v: %v", value, err)
	}
	return string(encoded)
}

// TestSweepingTheDenseGate measures MaxDenseDistance instead of assuming it.
//
// The gate has two jobs pulling against each other: let a real match through,
// and keep a query about something the user never saved from returning the
// nearest thing in the library anyway. The sweep runs the whole labeled set at
// every candidate cutoff and prints both sides, so the constant is chosen from
// a table rather than copied.
//
// With GEMINI_API_KEY set this measures the shipped model. Without it, it
// measures the lexical stand-in, whose distances live on a different scale, and
// the winner it prints must not be copied into the constant.
func TestSweepingTheDenseGate(t *testing.T) {
	service, pool, _ := testService(t)
	embedder, model := evalEmbedder(t)
	service.embedder = embedder
	ctx := context.Background()

	library := loadCorpus(t)
	set, err := LoadLabeledSet("../../api/eval/search-v1.json")
	if err != nil {
		t.Fatalf("LoadLabeledSet: %v", err)
	}

	// One batched call for every document and every query, before anything is
	// timed or swept. After this the cache answers and the passes are free.
	warm := make([]string, 0, len(library.Reels)+len(set.Queries))
	for _, reel := range library.Reels {
		warm = append(warm, embed.Document(embed.Fields{
			Title: reel.Title, Summary: reel.Summary, Category: reel.Category,
			Tags: reel.Tags, Facts: reel.Facts, Places: reel.Places,
		}))
	}
	for _, labeled := range set.Queries {
		warm = append(warm, NormalizeQuery(labeled.Query))
	}
	if _, err := embedder.Embed(ctx, warm); err != nil {
		t.Fatalf("embedding the corpus and the queries: %v", err)
	}

	for _, reel := range library.Reels {
		seedCorpusReel(t, pool, embedder, reel)
	}

	present, total, err := service.Coverage(ctx, userA, set)
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	if present != total {
		t.Fatalf("the seeded library holds %d of the set's %d judged reels", present, total)
	}

	t.Logf("sweeping the dense gate over %d queries and %d reels, model %q",
		len(set.Queries), len(library.Reels), model)
	t.Log("distance  recall@10  ndcg@10  mrr  p@5  unrelated-correct")

	best, bestReport := 0.0, Report{}
	for step := 10; step <= 50; step++ {
		distance := float64(step) / 50
		service.MaxDistance = distance
		report, _, err := service.Evaluate(ctx, userA, set)
		if err != nil {
			t.Fatalf("evaluating at %.2f: %v", distance, err)
		}
		t.Logf("%8.2f  %9.3f  %7.3f  %.3f  %.3f  %d/%d",
			distance, report.RecallAt10, report.NDCGAt10, report.MRR,
			report.PrecisionAt5, report.UnrelatedCorrect, report.UnrelatedQueries)

		// The widest gate that still returns nothing for every unrelated
		// query. Widening it further only ever adds dense candidates, so this
		// is also the best recall available without breaking that promise.
		if report.UnrelatedCorrect == report.UnrelatedQueries {
			best, bestReport = distance, report
		}
	}

	if bestReport.Queries == 0 {
		t.Fatal("no cutoff in the sweep returned nothing for every unrelated query")
	}
	encoded, _ := json.Marshal(bestReport)
	t.Logf("widest gate holding the zero-result promise: %.2f", best)
	t.Logf("report at that gate: %s", encoded)

	// Every pass above answered from the embedding cache, so its latencies are
	// the SQL and the fusion with the provider call taken out. One uncached
	// pass puts it back, which is the only number the p95 budget can be judged
	// against.
	service.MaxDistance = best
	service.embedder = embedder.inner
	live, _, err := service.Evaluate(ctx, userA, set)
	if err != nil {
		t.Fatalf("uncached pass: %v", err)
	}
	t.Logf("end to end including one embedding call per query: p50 %s p95 %s",
		live.P50, live.P95)
	service.embedder = embedder

	// Name what is still wrong at the chosen gate. An average hides which kind
	// of query is failing, and the kind is the only part worth acting on.
	_, scores, err := service.Evaluate(ctx, userA, set)
	if err != nil {
		t.Fatalf("re-running at %.2f: %v", best, err)
	}
	for index, score := range scores {
		switch {
		case score.Judged && score.RecallAt10 < 1:
			t.Logf("incomplete at %.2f: %s (recall %.2f, ndcg %.2f) %q",
				best, set.Queries[index].ID, score.RecallAt10, score.NDCGAt10,
				set.Queries[index].Query)
		case !score.Judged && !score.ZeroResults:
			t.Logf("unrelated query returned results at %.2f: %s %q",
				best, set.Queries[index].ID, set.Queries[index].Query)
		}
	}
}
