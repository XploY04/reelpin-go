// Package embed turns a content version into a vector and keeps the vectors
// comparable. Every embedding records the model, the requested dimension and
// the document-builder version that produced it, because a vector is only
// meaningful against others made exactly the same way.
package embed

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Model, Dimension and DocumentVersion are configuration, validated once at
// startup rather than spread as string literals through queries.
const (
	// DefaultModel is the evaluation starting point, not a permanent choice.
	DefaultModel = "gemini-embedding-2"
	// DefaultDimension matches the index. Changing it needs a new column.
	DefaultDimension = 768
	// DocumentVersion changes whenever the text below changes shape. A bump
	// invalidates every hash and re-embeds on next demand.
	DocumentVersion = "content-doc-v1"
)

// Fields is what a document is built from. Transcripts are deliberately absent
// in this version: they are long, noisy, and dominate the vector with filler
// rather than with what the reel is about.
type Fields struct {
	Title    string
	Summary  string
	Category string
	Tags     []string
	Facts    []string
	Places   []string
}

// Document builds the text to embed in a fixed labelled order. Fixed order is
// what makes the hash meaningful: the same content must produce the same bytes
// on every machine and every run, or the skip-if-unchanged check is useless.
func Document(fields Fields) string {
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

	add("Title", fields.Title)
	add("Summary", fields.Summary)
	add("Category", fields.Category)
	addList("Tags", fields.Tags)
	addList("Facts", fields.Facts)
	addList("Places", fields.Places)

	return strings.Join(sections, "\n")
}

// Hash identifies one document under one model and builder version. All three
// go into the hash, so changing the model or the builder re-embeds even when
// the text is identical.
func Hash(document, model string, dimension int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d|%s",
		DocumentVersion, model, dimension, document)))
	return hex.EncodeToString(sum[:])
}

// Vector renders a slice for pgvector's text input. pgx has no native vector
// type, and this is the format the extension parses.
func Vector(values []float32) string {
	parts := make([]string, len(values))
	for index, value := range values {
		parts[index] = fmt.Sprintf("%g", value)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// AssertConfigured fails startup when configuration and this package disagree
// about the dimension. The index holds exactly one size, so a mismatch is a
// corrupt vector set rather than a slow query, and it is worth refusing to
// start over.
func AssertConfigured(model string, dimension int) error {
	if dimension != DefaultDimension {
		return fmt.Errorf(
			"embedding dimension %d does not match the index dimension %d: a new dimension needs a new column",
			dimension, DefaultDimension)
	}
	if strings.TrimSpace(model) == "" {
		return fmt.Errorf("no embedding model configured")
	}
	return nil
}
