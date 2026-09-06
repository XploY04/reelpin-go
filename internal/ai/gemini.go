package ai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultModel is what the Python service runs today, so a migrated reel is
// extracted the same way it was before. ModelVersion is stored with every
// content version; changing the model is a deliberate reprocess.
const DefaultModel = "gemini-2.0-flash-lite"

const defaultBaseURL = "https://generativelanguage.googleapis.com"

// ProviderError is a Gemini failure with enough shape for the pipeline to
// classify it: the status separates quota from outage from a caller bug, and
// RetryAfter carries the provider's own push-back when it gave one.
type ProviderError struct {
	StatusCode int
	RetryAfter time.Duration
	message    string
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("gemini: %s (status %d)", e.message, e.StatusCode)
}

// ErrEmptyResponse means the model answered with no usable candidate.
var ErrEmptyResponse = errors.New("gemini returned no content")

type GeminiConfig struct {
	APIKey string
	Model  string
	// BaseURL exists for tests, which stand up a local server. Empty means the
	// real API.
	BaseURL string
	Timeout time.Duration
}

// Gemini calls the REST generateContent API with the standard library. The
// call shape is small enough that a client SDK would cost more than it saves.
type Gemini struct {
	config GeminiConfig
	client *http.Client
}

func NewGemini(config GeminiConfig) *Gemini {
	if config.Model == "" {
		config.Model = DefaultModel
	}
	if config.BaseURL == "" {
		config.BaseURL = defaultBaseURL
	}
	if config.Timeout <= 0 {
		config.Timeout = 45 * time.Second
	}
	return &Gemini{config: config, client: &http.Client{Timeout: config.Timeout}}
}

// ModelVersion is what persist stores on the content version.
func (g *Gemini) ModelVersion() string { return g.config.Model }

type geminiPart struct {
	Text       string            `json:"text,omitempty"`
	InlineData *geminiInlineData `json:"inline_data,omitempty"`
}

type geminiInlineData struct {
	MIMEType string `json:"mime_type"`
	Data     string `json:"data"`
}

type geminiRequest struct {
	Contents []struct {
		Parts []geminiPart `json:"parts"`
	} `json:"contents"`
	GenerationConfig struct {
		Temperature      float64         `json:"temperature"`
		ResponseMIMEType string          `json:"responseMimeType,omitempty"`
		ResponseSchema   json.RawMessage `json:"responseSchema,omitempty"`
	} `json:"generationConfig"`
}

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
}

// generate makes one call. responseSchema, when set, asks the provider to
// enforce JSON shape; domain validation still happens after.
func (g *Gemini) generate(ctx context.Context, parts []geminiPart, responseSchema json.RawMessage) (string, error) {
	if strings.TrimSpace(g.config.APIKey) == "" {
		return "", &ProviderError{StatusCode: 0, message: "GEMINI_API_KEY is not configured"}
	}

	request := geminiRequest{}
	request.Contents = append(request.Contents, struct {
		Parts []geminiPart `json:"parts"`
	}{Parts: parts})
	request.GenerationConfig.Temperature = 0
	if responseSchema != nil {
		request.GenerationConfig.ResponseMIMEType = "application/json"
		request.GenerationConfig.ResponseSchema = responseSchema
	}

	body, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("encoding the gemini request: %w", err)
	}

	url := fmt.Sprintf("%s/v1beta/models/%s:generateContent",
		strings.TrimSuffix(g.config.BaseURL, "/"), g.config.Model)
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	// The key travels as a header, never in the URL: URLs end up in logs.
	httpRequest.Header.Set("x-goog-api-key", g.config.APIKey)

	response, err := g.client.Do(httpRequest)
	if err != nil {
		return "", &ProviderError{StatusCode: 0, message: err.Error()}
	}
	defer response.Body.Close()

	// The response is bounded: a model answer is text, not a download.
	payload, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return "", &ProviderError{StatusCode: response.StatusCode, message: "reading the response: " + err.Error()}
	}

	if response.StatusCode != http.StatusOK {
		return "", &ProviderError{
			StatusCode: response.StatusCode,
			RetryAfter: retryAfter(response.Header.Get("Retry-After"), payload),
			message:    fmt.Sprintf("status %d", response.StatusCode),
		}
	}

	var decoded geminiResponse
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return "", fmt.Errorf("decoding the gemini response: %w", err)
	}
	if len(decoded.Candidates) == 0 || len(decoded.Candidates[0].Content.Parts) == 0 {
		return "", ErrEmptyResponse
	}
	var text strings.Builder
	for _, part := range decoded.Candidates[0].Content.Parts {
		text.WriteString(part.Text)
	}
	return strings.TrimSpace(text.String()), nil
}

// retryAfter reads the provider's push-back from the standard header or the
// google.rpc RetryInfo detail some quota errors carry.
func retryAfter(header string, payload []byte) time.Duration {
	if header != "" {
		if seconds, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
	}
	var body struct {
		Error struct {
			Details []struct {
				Type       string `json:"@type"`
				RetryDelay string `json:"retryDelay"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(payload, &body) != nil {
		return 0
	}
	for _, detail := range body.Error.Details {
		if !strings.HasSuffix(detail.Type, "RetryInfo") {
			continue
		}
		if delay, err := time.ParseDuration(detail.RetryDelay); err == nil && delay > 0 {
			return delay
		}
	}
	return 0
}

func (g *Gemini) Transcribe(ctx context.Context, audio Media) (string, error) {
	body, err := os.ReadFile(audio.Path)
	if err != nil {
		return "", fmt.Errorf("reading audio: %w", err)
	}
	return g.generate(ctx, []geminiPart{
		{Text: transcriptionPrompt},
		{InlineData: &geminiInlineData{MIMEType: audio.MIMEType, Data: base64.StdEncoding.EncodeToString(body)}},
	}, nil)
}

func (g *Gemini) ReadText(ctx context.Context, images []Media) (string, error) {
	if len(images) == 0 {
		return "", nil
	}
	parts := []geminiPart{{Text: imageTextPrompt}}
	for _, image := range images {
		body, err := os.ReadFile(image.Path)
		if err != nil {
			return "", fmt.Errorf("reading image: %w", err)
		}
		parts = append(parts, geminiPart{
			InlineData: &geminiInlineData{MIMEType: image.MIMEType, Data: base64.StdEncoding.EncodeToString(body)},
		})
	}
	return g.generate(ctx, parts, nil)
}

// extractionSchema is the provider-side shape hint. Domain validation in
// schema.go remains the real gate.
var extractionSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"title": {"type": "string"},
		"summary": {"type": "string"},
		"content_domain": {"type": "string"},
		"content_format": {"type": "string"},
		"topical_tags": {"type": "array", "items": {"type": "string"}},
		"key_facts": {"type": "array", "items": {"type": "string"}},
		"locations": {"type": "array", "items": {"type": "object", "properties": {
			"name": {"type": "string"}, "neighborhood": {"type": "string"},
			"city": {"type": "string"}, "state": {"type": "string"}, "country": {"type": "string"}
		}}},
		"people_mentioned": {"type": "array", "items": {"type": "string"}},
		"actionable_items": {"type": "array", "items": {"type": "string"}},
		"events": {"type": "array", "items": {"type": "object", "properties": {
			"name": {"type": "string"}, "date": {"type": "string"}, "time": {"type": "string"}
		}}}
	},
	"required": ["title", "summary"]
}`)

func (g *Gemini) Extract(ctx context.Context, transcript, caption string) (Extraction, error) {
	text, err := g.generate(ctx, []geminiPart{{Text: extractionUserPrompt(transcript, caption)}}, extractionSchema)
	if err != nil {
		return Extraction{}, err
	}
	var extraction Extraction
	if err := json.Unmarshal([]byte(stripFences(text)), &extraction); err != nil {
		return Extraction{}, fmt.Errorf("%w: %v", ErrInvalidExtraction, err)
	}
	return extraction.Normalize(), nil
}

var categorySchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"category": {"type": "string"},
		"subcategory": {"type": "string"},
		"proposal": {"type": "object", "properties": {
			"name": {"type": "string"}, "description": {"type": "string"}
		}}
	},
	"required": ["category"]
}`)

func (g *Gemini) Categorize(ctx context.Context, extraction Extraction, taxonomy []TaxonomyOption) (Category, error) {
	text, err := g.generate(ctx, []geminiPart{{Text: categoryUserPrompt(extraction, taxonomy)}}, categorySchema)
	if err != nil {
		return Category{}, err
	}
	var category Category
	if err := json.Unmarshal([]byte(stripFences(text)), &category); err != nil {
		return Category{}, fmt.Errorf("decoding the category: %w", err)
	}
	if category.Proposal != nil && strings.TrimSpace(category.Proposal.Name) == "" {
		category.Proposal = nil
	}
	return category, nil
}

// stripFences tolerates a model that wraps JSON in a markdown fence despite
// the response mime type.
func stripFences(text string) string {
	trimmed := strings.TrimSpace(text)
	trimmed = strings.TrimPrefix(trimmed, "```json")
	trimmed = strings.TrimPrefix(trimmed, "```")
	trimmed = strings.TrimSuffix(trimmed, "```")
	return strings.TrimSpace(trimmed)
}
