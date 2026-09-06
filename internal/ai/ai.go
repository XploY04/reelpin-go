package ai

import "context"

// TaxonomyOption is one active category the model may choose from. The tree is
// two levels: a subcategory is an option inside its parent's Subcategories.
type TaxonomyOption struct {
	ID            string
	Name          string
	Description   string
	Subcategories []TaxonomyOption
}

// Category is the model's filing of one piece of content against the active
// taxonomy. When nothing fits, the model still picks the nearest active
// category and carries its wish as a Proposal; a processing job can propose a
// category but never activate one.
type Category struct {
	Category    string            `json:"category"`
	Subcategory string            `json:"subcategory"`
	Proposal    *CategoryProposal `json:"proposal,omitempty"`
}

// CategoryProposal is a name the model wanted and could not have. It is stored
// for the weekly curator, never applied.
type CategoryProposal struct {
	Name        string `json:"name"`
	Description string `json:"description"`
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

// Categorizer files content against the active taxonomy. The taxonomy is an
// argument, pinned by the caller for the whole run, so categorization stays
// deterministic against one taxonomy version even while the curator works.
type Categorizer interface {
	Categorize(ctx context.Context, extraction Extraction, taxonomy []TaxonomyOption) (Category, error)
}
