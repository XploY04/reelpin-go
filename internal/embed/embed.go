// Package embed turns content into vectors for search.
//
// A vector is only reusable while the thing that produced it is unchanged, so
// every stored vector carries the model, the dimension and the document shape
// that made it. Re-embedding is decided by comparing those, not by checking
// whether the column is null.
package embed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strings"

	"github.com/XploY04/reelpin-go/internal/ai"
)

const (
	// Model and Dimension are what the search index is built from. Changing
	// either invalidates every stored vector, which is why they are recorded
	// with each one.
	Model     = "gemini-embedding-001"
	Dimension = 768

	// DocumentVersion is the shape of the text handed to the model. Changing
	// what goes into a document changes what it means, so it is versioned too.
	DocumentVersion = "content-doc-v1"
)

// TaskType tells the provider whether it is embedding something to be found or
// something doing the finding. Using the wrong one costs real relevance.
type TaskType string

const (
	TaskDocument TaskType = "RETRIEVAL_DOCUMENT"
	TaskQuery    TaskType = "RETRIEVAL_QUERY"
)

// Embedder is the provider seam.
type Embedder interface {
	Embed(ctx context.Context, texts []string, task TaskType) ([][]float32, error)
}

// Document is the text a content version is found by. It is deliberately not
// the transcript: a summary, the places, the people and the facts are what a
// person searches for, and the raw transcript drowns them in filler.
func Document(extraction ai.Extraction, platform, contentType string) string {
	sections := []string{}

	add := func(label, value string) {
		if cleaned := strings.TrimSpace(value); cleaned != "" {
			sections = append(sections, label+": "+cleaned)
		}
	}
	addList := func(label string, values []string) {
		cleaned := []string{}
		for _, value := range values {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				cleaned = append(cleaned, trimmed)
			}
		}
		if len(cleaned) > 0 {
			sections = append(sections, label+": "+strings.Join(cleaned, ", "))
		}
	}

	add("Title", extraction.Title)
	add("Summary", extraction.Summary)
	addList("Topics", extraction.TopicalTags)
	addList("Facts", extraction.KeyFacts)

	places := []string{}
	for _, location := range extraction.Locations {
		label := strings.TrimSpace(location.Name)
		if address := location.Address(); address != "" {
			label = strings.TrimSpace(label + ", " + address)
		}
		if label != "" {
			places = append(places, label)
		}
	}
	addList("Places", places)

	addList("People", extraction.PeopleMentioned)
	addList("Actions", extraction.ActionableItems)

	events := []string{}
	for _, event := range extraction.Events {
		label := strings.TrimSpace(event.Name)
		if event.Date != "" {
			label += " on " + event.Date
		}
		if label != "" {
			events = append(events, label)
		}
	}
	addList("Events", events)

	// The source is a weak but real signal: people remember where they saw it.
	add("Source", strings.TrimSpace(platform+" "+contentType))

	return strings.Join(sections, "\n")
}

// ContentHash identifies the exact text a vector was made from, so a document
// that changed is re-embedded and one that did not is left alone.
func ContentHash(document string) string {
	sum := sha256.Sum256([]byte(document))
	return hex.EncodeToString(sum[:])
}

// NeedsEmbedding decides whether to spend a provider call. A null vector is
// only one of the reasons; a changed model, dimension, document shape or text
// all mean the stored vector no longer describes this content.
func NeedsEmbedding(stored StoredVector, document string) bool {
	switch {
	case !stored.Present:
		return true
	case stored.Model != Model:
		return true
	case stored.Dimension != Dimension:
		return true
	case stored.DocumentVersion != DocumentVersion:
		return true
	case stored.ContentHash != ContentHash(document):
		return true
	}
	return false
}

// StoredVector is what a row already has.
type StoredVector struct {
	Present         bool
	Model           string
	Dimension       int
	DocumentVersion string
	ContentHash     string
}

// Normalize scales a vector to unit length. Cosine distance assumes it, and a
// provider that does not normalize would otherwise let long documents win on
// magnitude rather than meaning.
func Normalize(vector []float32) []float32 {
	var sum float64
	for _, value := range vector {
		sum += float64(value) * float64(value)
	}
	if sum == 0 {
		return vector
	}
	length := float32(math.Sqrt(sum))
	// Already unit length within floating-point noise: leave it exactly as the
	// provider returned it.
	if math.Abs(float64(length)-1) < 1e-6 {
		return vector
	}

	normalized := make([]float32, len(vector))
	for index, value := range vector {
		normalized[index] = value / length
	}
	return normalized
}

// Vector renders a slice in the literal form pgvector accepts.
func Vector(values []float32) string {
	if len(values) == 0 {
		return "[]"
	}
	var builder strings.Builder
	builder.WriteByte('[')
	for index, value := range values {
		if index > 0 {
			builder.WriteByte(',')
		}
		fmt.Fprintf(&builder, "%g", value)
	}
	builder.WriteByte(']')
	return builder.String()
}
