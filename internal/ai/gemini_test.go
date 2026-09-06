package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
		body := make([]byte, r.ContentLength)
		r.Body.Read(body)
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
	answer := `{"title":"  Artjuna cafe  ","summary":"A quiet garden cafe.","topical_tags":["cafe","cafe","goa"],"locations":[{"name":"Artjuna","city":"Anjuna","country":"India"}],"events":[{"name":"Live music","date":"2026-02-31","time":"19:00"}]}`
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
