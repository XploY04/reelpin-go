package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/spend"
)

// fakeGemini answers like the REST API. The body it returns is scripted per
// test; the assertions on the request are shared.
func fakeGemini(t *testing.T, status int, answer string, check func(*http.Request, []byte)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ":generateContent") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("x-goog-api-key") == "" {
			t.Error("the request carries no api key header")
		}
		if strings.Contains(r.URL.RawQuery, "key=") {
			t.Error("the api key is in the URL, where logs would keep it")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if check != nil {
			check(r, body)
		}
		w.WriteHeader(status)
		if status == http.StatusOK {
			response := map[string]any{"candidates": []any{map[string]any{
				"content": map[string]any{"parts": []any{map[string]any{"text": answer}}},
			}}}
			json.NewEncoder(w).Encode(response)
		} else {
			w.Write([]byte(answer))
		}
	}))
}

func client(server *httptest.Server) *Gemini {
	return NewGemini(GeminiConfig{APIKey: "test-key", BaseURL: server.URL, Timeout: 5 * time.Second})
}

func TestExtractParsesAndNormalizes(t *testing.T) {
	answer := `{"title":"  Artjuna cafe  ","summary":"A quiet garden cafe.","content_domain":"Food","content_format":"Cafe recommendations","topical_tags":["cafe","cafe","goa"],"key_facts":["Serves breakfast"],"people_mentioned":["Artjuna"],"actionable_items":["Visit Artjuna"],"locations":[{"name":"Artjuna","neighborhood":"","city":"Anjuna","state":"Goa","country":"India"}],"events":[{"name":"Live music","date":"2026-02-31","time":"19:00"}]}`
	server := fakeGemini(t, http.StatusOK, answer, func(_ *http.Request, body []byte) {
		if !strings.Contains(string(body), "responseSchema") {
			t.Error("extraction did not ask for a structured response")
		}
	})
	defer server.Close()

	extraction, err := client(server).Extract(context.Background(), "transcript", "caption")
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if extraction.Title != "Artjuna cafe" {
		t.Errorf("title = %q", extraction.Title)
	}
	if len(extraction.TopicalTags) != 2 {
		t.Errorf("tags = %v, want deduplicated", extraction.TopicalTags)
	}
	if len(extraction.KeyFacts) != 1 || len(extraction.PeopleMentioned) != 1 || len(extraction.ActionableItems) != 1 || len(extraction.Locations) != 1 {
		t.Errorf("structured detail was lost: %+v", extraction)
	}
	// 2026-02-31 is not a date the model meant; the event stays, its date goes.
	if len(extraction.Events) != 1 || extraction.Events[0].Date != "" {
		t.Errorf("events = %+v", extraction.Events)
	}
}

func TestExtractRejectsUnparseableOutput(t *testing.T) {
	server := fakeGemini(t, http.StatusOK, "I could not help with that.", nil)
	defer server.Close()

	_, err := client(server).Extract(context.Background(), "t", "c")
	if !errors.Is(err, ErrInvalidExtraction) {
		t.Fatalf("err = %v, want ErrInvalidExtraction", err)
	}
}

func TestExtractionRequestsAllStructuredFields(t *testing.T) {
	server := fakeGemini(t, http.StatusOK, completeExtractionAnswer, func(_ *http.Request, body []byte) {
		var request struct {
			GenerationConfig struct {
				Schema struct {
					Required   []string `json:"required"`
					Properties map[string]struct {
						Items struct {
							Required []string `json:"required"`
						} `json:"items"`
					} `json:"properties"`
				} `json:"responseSchema"`
			} `json:"generationConfig"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Error(err)
			return
		}
		schema := request.GenerationConfig.Schema
		for _, name := range []string{"title", "summary", "content_domain", "content_format", "topical_tags", "key_facts", "locations", "people_mentioned", "actionable_items", "events"} {
			if !containsField(schema.Required, name) {
				t.Errorf("extraction schema does not require %s", name)
			}
		}
		for _, name := range []string{"name", "neighborhood", "city", "state", "country"} {
			if !containsField(schema.Properties["locations"].Items.Required, name) {
				t.Errorf("location schema does not require %s", name)
			}
		}
		for _, name := range []string{"name", "date", "time"} {
			if !containsField(schema.Properties["events"].Items.Required, name) {
				t.Errorf("event schema does not require %s", name)
			}
		}
	})
	defer server.Close()
	if _, err := client(server).Extract(context.Background(), "transcript", "caption"); err != nil {
		t.Fatal(err)
	}
}

const completeExtractionAnswer = `{"title":"t","summary":"s","content_domain":"","content_format":"","topical_tags":[],"key_facts":[],"locations":[],"people_mentioned":[],"actionable_items":[],"events":[]}`

func containsField(fields []string, name string) bool {
	for _, field := range fields {
		if field == name {
			return true
		}
	}
	return false
}

func TestExtractRejectsMissingOrNullStructuredFields(t *testing.T) {
	for _, field := range []string{"title", "summary", "content_domain", "content_format", "topical_tags", "key_facts", "locations", "people_mentioned", "actionable_items", "events"} {
		for _, missing := range []bool{true, false} {
			name := field + "/null"
			if missing {
				name = field + "/missing"
			}
			t.Run(name, func(t *testing.T) {
				var answer map[string]any
				if err := json.Unmarshal([]byte(completeExtractionAnswer), &answer); err != nil {
					t.Fatal(err)
				}
				if missing {
					delete(answer, field)
				} else {
					answer[field] = nil
				}
				body, err := json.Marshal(answer)
				if err != nil {
					t.Fatal(err)
				}
				server := fakeGemini(t, http.StatusOK, string(body), nil)
				defer server.Close()
				_, err = client(server).Extract(context.Background(), "Visit Gali Six in HSR Bengaluru", "")
				if !errors.Is(err, ErrInvalidExtraction) {
					t.Fatalf("err = %v, want missing %s rejected before it becomes empty data", err, field)
				}
			})
		}
	}
}

func TestCategorizeRejectsSentenceSubcategory(t *testing.T) {
	for _, subcategory := range []string{
		"Winter Outfit Style Inspiration Reviews",
		"Men's fashion style guide: outfit ideas and aesthetic inspiration for men's clothing and accessories across various seasons and trends.",
	} {
		t.Run(subcategory, func(t *testing.T) {
			answer, err := json.Marshal(Category{Category: "Fashion", Subcategory: subcategory})
			if err != nil {
				t.Fatal(err)
			}
			server := fakeGemini(t, http.StatusOK, string(answer), nil)
			defer server.Close()
			_, err = client(server).Categorize(context.Background(), Extraction{Title: "Brown jacket outfit"}, []TaxonomyOption{{Name: "Fashion"}})
			if err == nil {
				t.Fatal("an oversized subcategory was accepted")
			}
		})
	}
}

func TestCategorizeKeepsShortReusableSubcategories(t *testing.T) {
	for _, subcategory := range []string{"", "Reviews", "Winter Outfit Style Inspiration", "  Outfit\n inspiration  "} {
		t.Run(subcategory, func(t *testing.T) {
			answer, err := json.Marshal(Category{Category: "Fashion", Subcategory: subcategory})
			if err != nil {
				t.Fatal(err)
			}
			server := fakeGemini(t, http.StatusOK, string(answer), nil)
			defer server.Close()
			category, err := client(server).Categorize(context.Background(), Extraction{Title: "Brown jacket"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if want := strings.Join(strings.Fields(subcategory), " "); category.Subcategory != want {
				t.Errorf("subcategory = %q, want %q", category.Subcategory, want)
			}
		})
	}
}

func TestCategorizeCarriesTheProposal(t *testing.T) {
	answer := `{"category":"Food","subcategory":"Cafes","proposal":{"name":"Coffee Culture","description":"Specialty coffee content."}}`
	server := fakeGemini(t, http.StatusOK, answer, func(_ *http.Request, body []byte) {
		// encoding/json escapes '>' in strings, so match the names, not the arrow.
		if !strings.Contains(string(body), "Cafes") || !strings.Contains(string(body), "Eating and drinking") {
			t.Error("the active taxonomy did not reach the prompt")
		}
	})
	defer server.Close()

	category, err := client(server).Categorize(context.Background(), Extraction{Title: "x"}, []TaxonomyOption{{
		Name: "Food", Description: "Eating and drinking",
		Subcategories: []TaxonomyOption{{Name: "Cafes", Description: "Cafes"}},
	}})
	if err != nil {
		t.Fatalf("Categorize: %v", err)
	}
	if category.Category != "Food" || category.Subcategory != "Cafes" {
		t.Errorf("category = %+v", category)
	}
	if category.Proposal == nil || category.Proposal.Name != "Coffee Culture" {
		t.Errorf("proposal = %+v", category.Proposal)
	}
}

func TestAnEmptyProposalIsDropped(t *testing.T) {
	server := fakeGemini(t, http.StatusOK, `{"category":"Food","subcategory":"","proposal":{"name":"  ","description":""}}`, nil)
	defer server.Close()

	category, err := client(server).Categorize(context.Background(), Extraction{Title: "x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if category.Proposal != nil {
		t.Fatalf("an empty proposal survived: %+v", category.Proposal)
	}
}

func TestQuotaPushBackIsTyped(t *testing.T) {
	body := `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"37s"}]}}`
	server := fakeGemini(t, http.StatusTooManyRequests, body, nil)
	defer server.Close()

	_, err := client(server).Extract(context.Background(), "t", "c")
	var provider *ProviderError
	if !errors.As(err, &provider) {
		t.Fatalf("err = %v, want a ProviderError", err)
	}
	if provider.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d", provider.StatusCode)
	}
	if provider.RetryAfter != 37*time.Second {
		t.Errorf("retry after = %s, want the provider's own push-back", provider.RetryAfter)
	}
}

func TestRetryAfterHeaderWins(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	_, err := client(server).Extract(context.Background(), "t", "c")
	var provider *ProviderError
	if !errors.As(err, &provider) || provider.RetryAfter != 2*time.Minute {
		t.Fatalf("err = %v", err)
	}
}

func TestAMissingKeyFailsBeforeTheNetwork(t *testing.T) {
	gemini := NewGemini(GeminiConfig{BaseURL: "http://127.0.0.1:1"})
	_, err := gemini.Extract(context.Background(), "t", "c")
	var provider *ProviderError
	if !errors.As(err, &provider) {
		t.Fatalf("err = %v", err)
	}
}

// recordedUsage collects what the client reported it spent.
type recordedUsage struct {
	seen []spend.Usage
}

func (r *recordedUsage) Record(_ context.Context, usage spend.Usage) {
	r.seen = append(r.seen, usage)
}

func TestUsageComesFromTheProvidersOwnCount(t *testing.T) {
	// The cost gate runs on what the provider says it billed, never on an
	// estimate made here.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"candidates": []any{map[string]any{
				"content": map[string]any{"parts": []any{map[string]any{"text": completeExtractionAnswer}}},
			}},
			"usageMetadata": map[string]any{
				"promptTokenCount": 4321, "candidatesTokenCount": 210,
			},
		})
	}))
	defer server.Close()

	usage := &recordedUsage{}
	client := NewGemini(GeminiConfig{APIKey: "test-key", BaseURL: server.URL, Usage: usage})
	if _, err := client.Extract(context.Background(), "transcript", "caption"); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if len(usage.seen) != 1 {
		t.Fatalf("recorded %d calls, want 1", len(usage.seen))
	}
	got := usage.seen[0]
	if got.InputTokens != 4321 || got.OutputTokens != 210 || !got.Measured {
		t.Errorf("usage = %+v, want the provider's own counts", got)
	}
	if got.Provider != "gemini" || got.Operation != "extract" || got.Model != DefaultModel {
		t.Errorf("usage = %+v, want it attributed to the model and the stage", got)
	}
}

func TestAnAnswerWithNoUsageBlockIsCountedNotEstimated(t *testing.T) {
	server := fakeGemini(t, http.StatusOK, completeExtractionAnswer, nil)
	defer server.Close()

	usage := &recordedUsage{}
	client := NewGemini(GeminiConfig{APIKey: "test-key", BaseURL: server.URL, Usage: usage})
	if _, err := client.Extract(context.Background(), "t", "c"); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if len(usage.seen) != 1 || usage.seen[0].Measured {
		t.Fatalf("usage = %+v, want one unmeasured call rather than zero tokens", usage.seen)
	}
	if usage.seen[0].Calls != 1 {
		t.Errorf("calls = %d, want the call itself to be on record", usage.seen[0].Calls)
	}
}

func TestARefusalIsNotBilled(t *testing.T) {
	server := fakeGemini(t, http.StatusTooManyRequests, `{"error":{"code":429}}`, nil)
	defer server.Close()

	usage := &recordedUsage{}
	client := NewGemini(GeminiConfig{APIKey: "test-key", BaseURL: server.URL, Usage: usage})
	if _, err := client.Extract(context.Background(), "t", "c"); err == nil {
		t.Fatal("a 429 was not an error")
	}
	if len(usage.seen) != 0 {
		t.Errorf("a refused call was recorded as spending: %+v", usage.seen)
	}
}
