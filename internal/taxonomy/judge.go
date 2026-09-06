package taxonomy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// curationPrompt shows the model the active tree and what people wanted that
// it could not give them. The instructions push toward merging: a taxonomy
// grows badly by accepting every near-synonym, and an alias costs nothing
// while a category costs everyone a wrong choice.
func curationPrompt(tree Tree, proposals []Proposal) string {
	var builder strings.Builder
	builder.WriteString(`You maintain a category taxonomy for saved social media content.

Existing active categories:
`)
	for _, option := range tree.Options {
		fmt.Fprintf(&builder, "- %s: %s\n", option.Name, option.Description)
		for _, sub := range option.Subcategories {
			fmt.Fprintf(&builder, "  - %s: %s\n", sub.Name, sub.Description)
		}
	}

	builder.WriteString(`
Proposed categories, with how many distinct pieces of content wanted each:
`)
	for _, proposal := range proposals {
		fmt.Fprintf(&builder, "- %q (wanted by %d) names=%v descriptions=%v\n",
			proposal.NormalizedName, proposal.ContentCount, proposal.Names, proposal.Descriptions)
	}

	builder.WriteString(`
For each proposal choose exactly one action:
- "alias": it means the same thing as an existing category. Set alias_of to
  that category's exact name. Prefer this whenever it is defensible.
- "add": it is a genuinely distinct concept an existing category cannot hold.
- "reject": it is too specific, too rare, or not a category at all.

Set confidence between 0 and 1 for how sure you are. Answer about every
proposal listed and nothing else.`)
	return builder.String()
}

var judgeSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"actions": {"type": "array", "items": {
			"type": "object",
			"properties": {
				"normalized_name": {"type": "string"},
				"action": {"type": "string", "enum": ["add", "alias", "reject"]},
				"name": {"type": "string"},
				"description": {"type": "string"},
				"alias_of": {"type": "string"},
				"confidence": {"type": "number"}
			},
			"required": ["normalized_name", "action", "confidence"]
		}}
	},
	"required": ["actions"]
}`)

// GeminiJudge calls the pinned curation model.
//
// ponytail: this repeats internal/ai's REST call because that package's
// generate is unexported and this branch does not own it. Export a structured
// generate there and this type becomes a two-line adapter.
type GeminiJudge struct {
	apiKey  string
	baseURL string
	client  *http.Client
}

func NewGeminiJudge(apiKey, baseURL string) *GeminiJudge {
	if baseURL == "" {
		baseURL = "https://generativelanguage.googleapis.com"
	}
	return &GeminiJudge{apiKey: apiKey, baseURL: baseURL,
		client: &http.Client{Timeout: 60 * time.Second}}
}

func (g *GeminiJudge) Judge(ctx context.Context, prompt string) (Decision, error) {
	body, err := json.Marshal(map[string]any{
		"contents": []any{map[string]any{
			"parts": []any{map[string]string{"text": prompt}},
		}},
		"generationConfig": map[string]any{
			"response_mime_type": "application/json",
			"response_schema":    judgeSchema,
			// Curation should answer the same way twice for the same input.
			"temperature": 0,
		},
	})
	if err != nil {
		return Decision{}, fmt.Errorf("encoding the curation request: %w", err)
	}

	url := fmt.Sprintf("%s/v1beta/models/%s:generateContent", g.baseURL, CuratorModel)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Decision{}, fmt.Errorf("building the curation request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-goog-api-key", g.apiKey)

	response, err := g.client.Do(request)
	if err != nil {
		return Decision{}, fmt.Errorf("calling the curation model: %w", err)
	}
	defer response.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return Decision{}, fmt.Errorf("reading the curation response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return Decision{}, fmt.Errorf("curation model returned %d", response.StatusCode)
	}

	var envelope struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return Decision{}, fmt.Errorf("decoding the curation response: %w", err)
	}
	if len(envelope.Candidates) == 0 || len(envelope.Candidates[0].Content.Parts) == 0 {
		return Decision{}, fmt.Errorf("curation model returned no content")
	}

	text := strings.TrimSpace(envelope.Candidates[0].Content.Parts[0].Text)
	text = strings.TrimPrefix(text, "```json")
	text = strings.TrimPrefix(text, "```")
	text = strings.TrimSuffix(strings.TrimSpace(text), "```")

	var decision Decision
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &decision); err != nil {
		return Decision{}, fmt.Errorf("the curation model did not answer with the schema: %w", err)
	}
	return decision, nil
}
