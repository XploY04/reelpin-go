//go:build integration

package search

import (
	"context"
	"encoding/json"
	"fmt"
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

func (b *bagOfWords) Embed(_ context.Context, texts []string, _ embed.TaskType) ([][]float32, error) {
	b.calls++
	out := make([][]float32, 0, len(texts))
	for _, text := range texts {
		vector := make([]float32, embed.Dimension)
		for _, word := range strings.Fields(strings.ToLower(text)) {
			digest := fnv.New32a()
			digest.Write([]byte(strings.Trim(word, ".,!?'\"")))
			vector[digest.Sum32()%embed.Dimension]++
		}
		out = append(out, embed.Normalize(vector))
	}
	return out, nil
}

type corpusReel struct {
	URL         string   `json:"url"`
	Platform    string   `json:"platform"`
	Title       string   `json:"title"`
	Summary     string   `json:"summary"`
	Category    string   `json:"category"`
	Subcategory string   `json:"subcategory"`
	Locations   []string `json:"locations"`
	People      []string `json:"people"`
	Chunks      []string `json:"chunks"`
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

// seedCorpusReel saves one fixture reel for userA with embeddings from the
// stand-in embedder, mirroring what the pipeline writes.
func seedCorpusReel(t *testing.T, pool *pgxpool.Pool, embedder embed.Embedder, reel corpusReel) {
	t.Helper()
	ctx := context.Background()

	document := strings.Join([]string{
		"Title: " + reel.Title,
		"Summary: " + reel.Summary,
		"Places: " + strings.Join(reel.Locations, ", "),
		"People: " + strings.Join(reel.People, ", "),
		"Category: " + reel.Category + " / " + reel.Subcategory,
	}, "\n")

	texts := append([]string{document}, reel.Chunks...)
	vectors, err := embedder.Embed(ctx, texts, embed.TaskDocument)
	if err != nil {
		t.Fatalf("embedding the corpus: %v", err)
	}

	identity := strings.TrimPrefix(reel.URL, "https://example.com/eval/")

	var versionID string
	if err := pool.QueryRow(ctx, `
		WITH content AS (
			INSERT INTO reelpin.contents
				(source_platform, source_content_type, source_content_id, normalized_url, normalized_url_hash)
			VALUES ($1,'reel',$2,$3,$2)
			RETURNING id
		)
		INSERT INTO reelpin.content_versions
			(content_id, processor_version, extraction_schema_version, title, summary,
			 embedding, embedding_model, embedding_dimension, embedding_document_version)
		SELECT id, 'v1','v1',$4,$5,$6::vector,$7,$8,$9 FROM content
		RETURNING id::text`,
		reel.Platform, identity, reel.URL, reel.Title, reel.Summary,
		embed.Vector(vectors[0]), embed.Model, embed.Dimension, embed.DocumentVersion,
	).Scan(&versionID); err != nil {
		t.Fatalf("seeding %s: %v", reel.URL, err)
	}

	for ordinal, chunk := range reel.Chunks {
		if _, err := pool.Exec(ctx, `
			INSERT INTO reelpin.content_chunks
				(content_version_id, ordinal, chunk_text, content_hash,
				 embedding, embedding_model, embedding_dimension, embedding_document_version)
			VALUES ($1, $2, $3, $4, $5::vector, $6, $7, $8)`,
			versionID, ordinal, chunk, fmt.Sprintf("%s-%d", identity, ordinal),
			embed.Vector(vectors[ordinal+1]), embed.Model, embed.Dimension, embed.DocumentVersion); err != nil {
			t.Fatalf("seeding a chunk of %s: %v", reel.URL, err)
		}
	}

	locations := make([]map[string]string, 0, len(reel.Locations))
	for _, name := range reel.Locations {
		locations = append(locations, map[string]string{"name": name})
	}
	locationsJSON, _ := json.Marshal(locations)
	peopleJSON, _ := json.Marshal(reel.People)

	if _, err := pool.Exec(ctx, `
		INSERT INTO public.reels
			(user_id, url, normalized_url, source_platform, title, summary, transcript,
			 category, subcategory, locations, people_mentioned, content_version_id)
		VALUES ($1, $2, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10::jsonb, $11)`,
		userA, reel.URL, reel.Platform, reel.Title, reel.Summary,
		strings.Join(reel.Chunks, " "), reel.Category, reel.Subcategory,
		string(locationsJSON), string(peopleJSON), versionID); err != nil {
		t.Fatalf("seeding the reel %s: %v", reel.URL, err)
	}
}
