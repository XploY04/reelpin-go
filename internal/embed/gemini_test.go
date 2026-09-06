package embed

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/spend"
)

func TestEmbedSendsTheModelTaskAndDimension(t *testing.T) {
	var seen embedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") == "" {
			t.Error("the request carried no api key")
		}
		json.NewDecoder(r.Body).Decode(&seen)
		json.NewEncoder(w).Encode(embedResponse{Embeddings: []struct {
			Values []float32 `json:"values"`
		}{{Values: make([]float32, DefaultDimension)}}})
	}))
	defer server.Close()

	client := NewGemini(GeminiConfig{APIKey: "test-key", BaseURL: server.URL})
	vectors, err := client.Embed(context.Background(), []string{"a document"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vectors) != 1 || len(vectors[0]) != DefaultDimension {
		t.Fatalf("vectors = %d of %d", len(vectors), len(vectors[0]))
	}
	if len(seen.Requests) != 1 {
		t.Fatalf("sent %d requests", len(seen.Requests))
	}
	request := seen.Requests[0]
	if request.OutputDimensionality != DefaultDimension {
		t.Errorf("dimension = %d; the index holds exactly one size", request.OutputDimensionality)
	}
	if request.TaskType != "RETRIEVAL_DOCUMENT" {
		t.Errorf("task type = %q; a stored document and a query embed differently", request.TaskType)
	}
	if !strings.Contains(request.Model, DefaultModel) {
		t.Errorf("model = %q", request.Model)
	}
}

func TestNoKeyIsAnExplicitState(t *testing.T) {
	if _, err := NewGemini(GeminiConfig{}).Embed(context.Background(), []string{"x"}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}

func TestAProviderErrorDoesNotLeakItsBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"API key AIzaSyEXAMPLE is invalid"}`))
	}))
	defer server.Close()

	_, err := NewGemini(GeminiConfig{APIKey: "k", BaseURL: server.URL}).
		Embed(context.Background(), []string{"a document"})
	if err == nil {
		t.Fatal("a 403 was reported as success")
	}
	if strings.Contains(err.Error(), "AIzaSy") {
		t.Fatalf("the provider's body leaked into the error: %v", err)
	}
}

func TestAShortResponseIsAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(embedResponse{})
	}))
	defer server.Close()

	// Two documents in, zero vectors back: silently pairing them would attach
	// one document's vector to another.
	if _, err := NewGemini(GeminiConfig{APIKey: "k", BaseURL: server.URL}).
		Embed(context.Background(), []string{"one", "two"}); err == nil {
		t.Fatal("a short response was accepted")
	}
}

// countingUsage collects what the embedder reported it spent.
type countingUsage struct {
	seen []spend.Usage
}

func (c *countingUsage) Record(_ context.Context, usage spend.Usage) {
	c.seen = append(c.seen, usage)
}

func TestEmbeddingIsCountedPerDocument(t *testing.T) {
	// batchEmbedContents reports no usage block, so the cost gate can only be
	// told how many documents were embedded. Pricing one is configuration.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(embedResponse{Embeddings: []struct {
			Values []float32 `json:"values"`
		}{
			{Values: make([]float32, DefaultDimension)},
			{Values: make([]float32, DefaultDimension)},
		}})
	}))
	defer server.Close()

	usage := &countingUsage{}
	client := NewGemini(GeminiConfig{APIKey: "test-key", BaseURL: server.URL, Usage: usage})
	if _, err := client.Embed(context.Background(), []string{"one", "two"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}

	if len(usage.seen) != 1 {
		t.Fatalf("recorded %d batches, want 1", len(usage.seen))
	}
	got := usage.seen[0]
	if got.Calls != 2 || got.Measured {
		t.Errorf("usage = %+v, want two unmeasured documents", got)
	}
	if got.Operation != "embed" || got.Model != DefaultModel {
		t.Errorf("usage = %+v, want it attributed to the embedding model", got)
	}
}
