package embed

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/genai"
)

// MaxBatch is how many texts go in one request. Bigger batches are cheaper per
// text but a single failure costs more, so this stays modest.
const MaxBatch = 32

// Gemini is the provider adapter. One client is built on first use and shared.
type Gemini struct {
	once    sync.Once
	client  *genai.Client
	err     error
	apiKey  string
	timeout time.Duration
}

func NewGemini(apiKey string, timeout time.Duration) *Gemini {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &Gemini{apiKey: apiKey, timeout: timeout}
}

func (g *Gemini) connect(ctx context.Context) (*genai.Client, error) {
	g.once.Do(func() {
		if strings.TrimSpace(g.apiKey) == "" {
			g.err = fmt.Errorf("GEMINI_API_KEY is not configured")
			return
		}
		g.client, g.err = genai.NewClient(ctx, &genai.ClientConfig{
			APIKey:  g.apiKey,
			Backend: genai.BackendGeminiAPI,
		})
	})
	return g.client, g.err
}

// Embed returns one vector per text, in order, normalized. The order matters:
// callers pair results back to rows by index.
func (g *Gemini) Embed(ctx context.Context, texts []string, task TaskType) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	client, err := g.connect(ctx)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()

	vectors := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += MaxBatch {
		end := start + MaxBatch
		if end > len(texts) {
			end = len(texts)
		}

		contents := make([]*genai.Content, 0, end-start)
		for _, text := range texts[start:end] {
			contents = append(contents, &genai.Content{Parts: []*genai.Part{{Text: text}}})
		}

		response, err := client.Models.EmbedContent(ctx, Model, contents, &genai.EmbedContentConfig{
			TaskType:             string(task),
			OutputDimensionality: genai.Ptr[int32](Dimension),
		})
		if err != nil {
			return nil, fmt.Errorf("embedding: %w", err)
		}
		if len(response.Embeddings) != end-start {
			return nil, fmt.Errorf("the provider returned %d vectors for %d texts",
				len(response.Embeddings), end-start)
		}

		for _, embedding := range response.Embeddings {
			if len(embedding.Values) != Dimension {
				return nil, fmt.Errorf("the provider returned a %d dimension vector, want %d",
					len(embedding.Values), Dimension)
			}
			// Truncated output dimensions are not unit length, so normalizing
			// is what makes cosine distance mean what the query assumes.
			vectors = append(vectors, Normalize(embedding.Values))
		}
	}
	return vectors, nil
}
