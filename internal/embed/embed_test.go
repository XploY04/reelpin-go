package embed

import (
	"math"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/ai"
)

func floatPtr(value float64) *float64 { return &value }

func TestDocumentCarriesWhatPeopleSearchFor(t *testing.T) {
	extraction := ai.Extraction{
		Title:       "Best cafes in Goa",
		Summary:     "Three cafes worth the ride.",
		TopicalTags: []string{"Cafes", "Goa"},
		KeyFacts:    []string{"Artjuna opens at 8am"},
		Locations: []ai.Location{
			{Name: "Artjuna", City: "Anjuna", Country: "India",
				Latitude: floatPtr(15.58), Longitude: floatPtr(73.74)},
		},
		PeopleMentioned: []string{"A creator"},
		ActionableItems: []string{"Book a table"},
		Events:          []ai.Event{{Name: "Sunday market", Date: "2026-11-20"}},
	}

	document := Document(extraction, "instagram", "reel")
	for _, want := range []string{
		"Best cafes in Goa", "worth the ride", "Cafes, Goa", "opens at 8am",
		"Artjuna, Anjuna, India", "A creator", "Book a table", "Sunday market on 2026-11-20",
		"instagram reel",
	} {
		if !strings.Contains(document, want) {
			t.Errorf("the document is missing %q:\n%s", want, document)
		}
	}

	// Empty sections are left out rather than written as empty labels.
	sparse := Document(ai.Extraction{Title: "Only a title"}, "", "")
	if strings.Contains(sparse, "Places:") || strings.Contains(sparse, "People:") {
		t.Errorf("empty sections were written:\n%s", sparse)
	}
}

func TestNeedsEmbeddingComparesEveryTag(t *testing.T) {
	document := "Title: A reel"
	current := StoredVector{
		Present: true, Model: Model, Dimension: Dimension,
		DocumentVersion: DocumentVersion, ContentHash: ContentHash(document),
	}

	if NeedsEmbedding(current, document) {
		t.Fatal("an up-to-date vector was marked for re-embedding")
	}

	tests := []struct {
		name   string
		mutate func(*StoredVector)
	}{
		{name: "no vector", mutate: func(v *StoredVector) { v.Present = false }},
		{name: "another model", mutate: func(v *StoredVector) { v.Model = "older-model" }},
		{name: "another dimension", mutate: func(v *StoredVector) { v.Dimension = 1536 }},
		{name: "another document shape", mutate: func(v *StoredVector) { v.DocumentVersion = "content-doc-v0" }},
		{name: "changed text", mutate: func(v *StoredVector) { v.ContentHash = ContentHash("something else") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stored := current
			tt.mutate(&stored)
			if !NeedsEmbedding(stored, document) {
				t.Fatalf("%s did not trigger a re-embed", tt.name)
			}
		})
	}
}

func TestNormalizeMakesUnitVectors(t *testing.T) {
	normalized := Normalize([]float32{3, 4})
	if math.Abs(float64(normalized[0])-0.6) > 1e-6 || math.Abs(float64(normalized[1])-0.8) > 1e-6 {
		t.Fatalf("normalized = %v", normalized)
	}

	length := 0.0
	for _, value := range normalized {
		length += float64(value) * float64(value)
	}
	if math.Abs(math.Sqrt(length)-1) > 1e-6 {
		t.Errorf("length = %v, want 1", math.Sqrt(length))
	}

	// A zero vector has no direction to preserve, and dividing would produce
	// NaN, which poisons every later comparison.
	if zero := Normalize([]float32{0, 0}); zero[0] != 0 || zero[1] != 0 {
		t.Errorf("zero vector = %v", zero)
	}

	// An already-unit vector is returned untouched.
	unit := []float32{1, 0}
	if got := Normalize(unit); got[0] != 1 || got[1] != 0 {
		t.Errorf("unit vector = %v", got)
	}
}

func TestVectorLiteral(t *testing.T) {
	if got := Vector([]float32{0.5, -0.25}); got != "[0.5,-0.25]" {
		t.Fatalf("literal = %q", got)
	}
	if got := Vector(nil); got != "[]" {
		t.Fatalf("empty literal = %q", got)
	}
}

func TestContentHashChangesWithTheText(t *testing.T) {
	first := ContentHash("Title: A")
	if first != ContentHash("Title: A") {
		t.Fatal("hashing is not stable")
	}
	if first == ContentHash("Title: B") {
		t.Fatal("two documents share a hash")
	}
}
