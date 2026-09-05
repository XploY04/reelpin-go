package ai

import "context"

// Category is one user's filing of a piece of content. It is deliberately
// separate from Extraction: the same reel can be "Travel" to one person and
// "Food" to another, so it is never stored on the shared content.
type Category struct {
	Category            string   `json:"category"`
	Subcategory         string   `json:"subcategory"`
	SecondaryCategories []string `json:"secondary_categories"`
}

// Media is one file handed to a model.
type Media struct {
	Path     string
	MIMEType string
}

// Transcriber turns audio into text.
type Transcriber interface {
	Transcribe(ctx context.Context, audio Media) (string, error)
}

// ImageReader reads the text in images, for posts that carry no audio.
type ImageReader interface {
	ReadText(ctx context.Context, images []Media) (string, error)
}

// Extractor produces the content-neutral structured half.
type Extractor interface {
	Extract(ctx context.Context, transcript, caption string) (Extraction, error)
}

// Categorizer files content for one user, given the categories they already
// use, so a library stays coherent instead of growing a new label per save.
type Categorizer interface {
	Categorize(ctx context.Context, extraction Extraction, existing []string) (Category, error)
}
