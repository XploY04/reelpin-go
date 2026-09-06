package embed

import (
	"strings"
	"testing"
)

func sample() Fields {
	return Fields{
		Title:    "Artjuna cafe",
		Summary:  "A quiet garden cafe in Anjuna.",
		Category: "food",
		Tags:     []string{"cafe", "garden"},
		Facts:    []string{"Opens at eight"},
		Places:   []string{"Anjuna, Goa"},
	}
}

func TestADocumentIsDeterministic(t *testing.T) {
	first := Document(sample())
	for i := 0; i < 20; i++ {
		if Document(sample()) != first {
			t.Fatal("the same fields produced different text; the skip-if-unchanged check would be useless")
		}
	}
	if !strings.HasPrefix(first, "Title: Artjuna cafe") {
		t.Fatalf("document = %q", first)
	}
}

func TestEmptyFieldsAreOmittedNotBlank(t *testing.T) {
	// A label with nothing after it is noise in the vector.
	document := Document(Fields{Title: "Only a title", Tags: []string{"", "  "}})
	if strings.Contains(document, "Tags:") || strings.Contains(document, "Summary:") {
		t.Fatalf("document = %q", document)
	}
}

func TestATranscriptIsNotPartOfTheDocument(t *testing.T) {
	// Fields has no transcript on purpose: it is long, noisy, and would
	// dominate the vector with filler.
	document := Document(sample())
	if strings.Contains(strings.ToLower(document), "transcript") {
		t.Fatal("the document mentions a transcript")
	}
}

func TestTheHashCoversTheModelAndDimension(t *testing.T) {
	document := Document(sample())
	base := Hash(document, DefaultModel, DefaultDimension)

	if Hash(document, DefaultModel, DefaultDimension) != base {
		t.Fatal("the same inputs hashed differently")
	}
	// A different model or dimension is a different vector space entirely, so
	// it must re-embed even though the text is identical.
	if Hash(document, "another-model", DefaultDimension) == base {
		t.Error("changing the model did not change the hash")
	}
	if Hash(document, DefaultModel, 1536) == base {
		t.Error("changing the dimension did not change the hash")
	}
	if Hash(document+" ", DefaultModel, DefaultDimension) == base {
		t.Error("changing the text did not change the hash")
	}
}

func TestVectorRendersForPgvector(t *testing.T) {
	if got := Vector([]float32{1, -0.5, 0}); got != "[1,-0.5,0]" {
		t.Fatalf("vector = %q", got)
	}
	if got := Vector(nil); got != "[]" {
		t.Fatalf("empty vector = %q", got)
	}
}
