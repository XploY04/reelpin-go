package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ErrNotConfigured means no API key. It is an explicit state rather than a nil
// panic: a development worker runs happily without embeddings.
var ErrNotConfigured = errors.New("no embedding API key configured")

// Gemini calls the embedding endpoint. It is stdlib only: the call is one POST,
// and a client library for that costs more than it saves.
type Gemini struct {
	apiKey    string
	model     string
	dimension int
	baseURL   string
	client    *http.Client
}

type GeminiConfig struct {
	APIKey    string
	Model     string
	Dimension int
	Timeout   time.Duration
	// BaseURL is a test seam. Empty means the real endpoint.
	BaseURL string
}

func NewGemini(config GeminiConfig) *Gemini {
	if config.Model == "" {
		config.Model = DefaultModel
	}
	if config.Dimension == 0 {
		config.Dimension = DefaultDimension
	}
	if config.Timeout == 0 {
		config.Timeout = 30 * time.Second
	}
	if config.BaseURL == "" {
		config.BaseURL = "https://generativelanguage.googleapis.com"
	}
	return &Gemini{
		apiKey:    config.APIKey,
		model:     config.Model,
		dimension: config.Dimension,
		baseURL:   config.BaseURL,
		client:    &http.Client{Timeout: config.Timeout},
	}
}

func (g *Gemini) Model() string  { return g.model }
func (g *Gemini) Dimension() int { return g.dimension }

type embedRequest struct {
	Requests []embedOne `json:"requests"`
}

type embedOne struct {
	Model                string       `json:"model"`
	Content              embedContent `json:"content"`
	TaskType             string       `json:"taskType"`
	OutputDimensionality int          `json:"outputDimensionality"`
}

type embedContent struct {
	Parts []embedPart `json:"parts"`
}

type embedPart struct {
	Text string `json:"text"`
}

type embedResponse struct {
	Embeddings []struct {
		Values []float32 `json:"values"`
	} `json:"embeddings"`
}

// Embed returns one vector per text, in order. The requested dimension is sent
// explicitly rather than trusted as a default, because the index holds exactly
// one size.
func (g *Gemini) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if g.apiKey == "" {
		return nil, ErrNotConfigured
	}
	if len(texts) == 0 {
		return nil, nil
	}

	payload := embedRequest{Requests: make([]embedOne, 0, len(texts))}
	for _, text := range texts {
		payload.Requests = append(payload.Requests, embedOne{
			Model:   "models/" + g.model,
			Content: embedContent{Parts: []embedPart{{Text: text}}},
			// A stored document, not a query: the two are embedded
			// differently and mixing them degrades retrieval.
			TaskType:             "RETRIEVAL_DOCUMENT",
			OutputDimensionality: g.dimension,
		})
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encoding the embedding request: %w", err)
	}

	url := fmt.Sprintf("%s/v1beta/models/%s:batchEmbedContents", g.baseURL, g.model)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-goog-api-key", g.apiKey)

	response, err := g.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("calling the embedding provider: %w", err)
	}
	defer response.Body.Close()

	// Bounded: a provider that answers with something enormous is a provider
	// having a bad day, not a reason to fill memory.
	raw, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("reading the embedding response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		// The provider's body can quote the request; only the status is kept.
		return nil, fmt.Errorf("the embedding provider answered %d", response.StatusCode)
	}

	var decoded embedResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("decoding the embedding response: %w", err)
	}
	if len(decoded.Embeddings) != len(texts) {
		return nil, fmt.Errorf("asked for %d embeddings and received %d",
			len(texts), len(decoded.Embeddings))
	}

	vectors := make([][]float32, 0, len(decoded.Embeddings))
	for _, embedding := range decoded.Embeddings {
		vectors = append(vectors, embedding.Values)
	}
	return vectors, nil
}
