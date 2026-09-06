//go:build integration

package search

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"os"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/embed"
	"github.com/jackc/pgx/v5/pgxpool"
)

// bagOfWords stands in for Gemini so the harness runs offline and
// deterministically. It is lexical, not semantic, so the numbers it produces
// measure the harness and the SQL, never Gemini's relevance. The real report
// comes from `maintenance search-eval` against a library with real embeddings.
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
}

type corpus struct {
	Version string       `json:"version"`
	Reels   []corpusReel `json:"reels"`
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

	raw, err := os.ReadFile("testdata/corpus-v1.json")
	if err != nil {
		t.Fatalf("reading the corpus: %v", err)
	}
	var library corpus
	if err := json.Unmarshal(raw, &library); err != nil {
		t.Fatalf("parsing the corpus: %v", err)
	}

	for _, reel := range library.Reels {
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

	if _, err := pool.Exec(ctx,
		`INSERT INTO reelpin.user_saves (user_id, content_id) VALUES ($1, $2)`,
		userA, contentID); err != nil {
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
