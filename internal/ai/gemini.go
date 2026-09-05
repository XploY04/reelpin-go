package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/genai"
)

// Models are the ones the Python service runs today, so a migrated reel is
// extracted the same way it was before.
const (
	DefaultTextModel  = "gemini-2.0-flash-lite"
	DefaultAudioModel = "gemini-2.0-flash-lite"
	DefaultImageModel = "gemini-2.0-flash-lite"
)

type GeminiConfig struct {
	APIKey     string
	TextModel  string
	AudioModel string
	ImageModel string
	Timeout    time.Duration
}

// Gemini is one client shared by every stage. The SDK holds its own connection
// pool, so building a client per call would throw that away.
type Gemini struct {
	once   sync.Once
	client *genai.Client
	err    error
	config GeminiConfig
}

func NewGemini(config GeminiConfig) *Gemini {
	if config.TextModel == "" {
		config.TextModel = DefaultTextModel
	}
	if config.AudioModel == "" {
		config.AudioModel = DefaultAudioModel
	}
	if config.ImageModel == "" {
		config.ImageModel = DefaultImageModel
	}
	if config.Timeout <= 0 {
		config.Timeout = 45 * time.Second
	}
	return &Gemini{config: config}
}

// connect builds the client on first use, so constructing the service never
// depends on the network.
func (g *Gemini) connect(ctx context.Context) (*genai.Client, error) {
	g.once.Do(func() {
		if strings.TrimSpace(g.config.APIKey) == "" {
			g.err = fmt.Errorf("GEMINI_API_KEY is not configured")
			return
		}
		g.client, g.err = genai.NewClient(ctx, &genai.ClientConfig{
			APIKey:  g.config.APIKey,
			Backend: genai.BackendGeminiAPI,
		})
	})
	return g.client, g.err
}

func (g *Gemini) Transcribe(ctx context.Context, audio Media) (string, error) {
	client, err := g.connect(ctx)
	if err != nil {
		return "", err
	}
	body, err := os.ReadFile(audio.Path)
	if err != nil {
		return "", fmt.Errorf("reading audio: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, g.config.Timeout)
	defer cancel()

	response, err := client.Models.GenerateContent(ctx, g.config.AudioModel,
		[]*genai.Content{{
			Role: genai.RoleUser,
			Parts: []*genai.Part{
				{Text: transcriptionPrompt},
				{InlineData: &genai.Blob{MIMEType: audio.MIMEType, Data: body}},
			},
		}},
		&genai.GenerateContentConfig{Temperature: genai.Ptr[float32](0)},
	)
	if err != nil {
		return "", fmt.Errorf("transcribing: %w", err)
	}
	return strings.TrimSpace(response.Text()), nil
}

func (g *Gemini) ReadText(ctx context.Context, images []Media) (string, error) {
	if len(images) == 0 {
		return "", nil
	}
	client, err := g.connect(ctx)
	if err != nil {
		return "", err
	}

	parts := []*genai.Part{{Text: imageTextPrompt}}
	for _, image := range images {
		body, err := os.ReadFile(image.Path)
		if err != nil {
			return "", fmt.Errorf("reading image: %w", err)
		}
		parts = append(parts, &genai.Part{InlineData: &genai.Blob{MIMEType: image.MIMEType, Data: body}})
	}

	ctx, cancel := context.WithTimeout(ctx, g.config.Timeout)
	defer cancel()

	response, err := client.Models.GenerateContent(ctx, g.config.ImageModel,
		[]*genai.Content{{Role: genai.RoleUser, Parts: parts}},
		&genai.GenerateContentConfig{Temperature: genai.Ptr[float32](0)},
	)
	if err != nil {
		return "", fmt.Errorf("reading image text: %w", err)
	}
	return strings.TrimSpace(response.Text()), nil
}

func (g *Gemini) Extract(ctx context.Context, transcript, caption string) (Extraction, error) {
	client, err := g.connect(ctx)
	if err != nil {
		return Extraction{}, err
	}

	prompt := strings.NewReplacer(
		"{transcript}", fallback(transcript, "(no audio/transcript available)"),
		"{caption}", fallback(caption, "(no caption provided)"),
	).Replace(extractionPrompt)

	ctx, cancel := context.WithTimeout(ctx, g.config.Timeout)
	defer cancel()

	response, err := client.Models.GenerateContent(ctx, g.config.TextModel,
		[]*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{{Text: prompt}}}},
		&genai.GenerateContentConfig{
			Temperature:      genai.Ptr[float32](0),
			ResponseMIMEType: "application/json",
			SystemInstruction: &genai.Content{
				Parts: []*genai.Part{{Text: "You are a precise data extraction assistant. Always respond with valid JSON only."}},
			},
		},
	)
	if err != nil {
		return Extraction{}, fmt.Errorf("extracting: %w", err)
	}

	var extraction Extraction
	if err := json.Unmarshal([]byte(jsonBody(response.Text())), &extraction); err != nil {
		return Extraction{}, fmt.Errorf("decoding the extraction: %w", err)
	}
	// The model's output becomes data only after it survives validation.
	return extraction.Normalize(), nil
}

func (g *Gemini) Categorize(ctx context.Context, extraction Extraction, existing []string) (Category, error) {
	client, err := g.connect(ctx)
	if err != nil {
		return Category{}, err
	}

	summary, err := json.Marshal(map[string]any{
		"title":          extraction.Title,
		"summary":        extraction.Summary,
		"content_domain": extraction.ContentDomain,
		"content_format": extraction.ContentFormat,
		"topical_tags":   extraction.TopicalTags,
	})
	if err != nil {
		return Category{}, fmt.Errorf("encoding the extraction: %w", err)
	}

	prompt := strings.NewReplacer(
		"{content}", string(summary),
		"{existing}", strings.Join(existing, ", "),
	).Replace(categoryPrompt)

	ctx, cancel := context.WithTimeout(ctx, g.config.Timeout)
	defer cancel()

	response, err := client.Models.GenerateContent(ctx, g.config.TextModel,
		[]*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{{Text: prompt}}}},
		&genai.GenerateContentConfig{
			Temperature:      genai.Ptr[float32](0),
			ResponseMIMEType: "application/json",
		},
	)
	if err != nil {
		return Category{}, fmt.Errorf("categorizing: %w", err)
	}

	var category Category
	if err := json.Unmarshal([]byte(jsonBody(response.Text())), &category); err != nil {
		return Category{}, fmt.Errorf("decoding the category: %w", err)
	}
	return NormalizeCategory(category), nil
}

// NormalizeCategory keeps a user's filing usable: labels are trimmed, bounded
// and never empty.
func NormalizeCategory(category Category) Category {
	normalized := Category{
		Category:            truncate(strings.TrimSpace(category.Category), MaxLabelRunes),
		Subcategory:         truncate(strings.TrimSpace(category.Subcategory), MaxLabelRunes),
		SecondaryCategories: cleanList(category.SecondaryCategories, MaxLabelRunes),
	}
	if normalized.Category == "" {
		normalized.Category = "Other"
	}
	if normalized.Subcategory == "" {
		normalized.Subcategory = "Other"
	}
	return normalized
}

// jsonBody tolerates a model that wraps its JSON in a fenced code block.
func jsonBody(text string) string {
	cleaned := strings.TrimSpace(text)
	if !strings.HasPrefix(cleaned, "```") {
		return cleaned
	}
	cleaned = strings.TrimPrefix(cleaned, "```json")
	cleaned = strings.TrimPrefix(cleaned, "```")
	cleaned = strings.TrimSuffix(cleaned, "```")
	return strings.TrimSpace(cleaned)
}

func fallback(value, whenEmpty string) string {
	if strings.TrimSpace(value) == "" {
		return whenEmpty
	}
	return value
}
