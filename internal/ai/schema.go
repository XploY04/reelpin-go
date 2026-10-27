// Package ai holds the model-facing work: transcription, image text, structured
// extraction and taxonomy categorization. Every call is behind a narrow
// interface, so the pipeline can be tested without a provider and a provider
// can be replaced without touching the pipeline.
package ai

import (
	"errors"
	"regexp"
	"strings"
	"time"
)

// ErrInvalidExtraction means the model's structured output cannot become a
// content version. Provider schema enforcement reduces malformed output; this
// is the domain validation it does not replace.
var ErrInvalidExtraction = errors.New("extraction is not usable")

// Validate is the gate between model output and the database. It runs on the
// normalized value: an extraction with neither a title nor a summary describes
// nothing, and persisting it would complete a job with an empty reel.
func (e Extraction) Validate() error {
	if strings.TrimSpace(e.Title) == "" && strings.TrimSpace(e.Summary) == "" {
		return ErrInvalidExtraction
	}
	return nil
}

// SchemaVersion is the shape of Extraction. It is stored with every content
// version, so a later change re-processes on purpose rather than by accident.
const SchemaVersion = "extraction-v1"

// Limits keep a model that ignores its instructions from filling the database.
const (
	MaxTitleRunes   = 200
	MaxSummaryRunes = 2_000
	MaxLabelRunes   = 120
	MaxListItems    = 50
	MaxLocations    = 25
	MaxEvents       = 25
	MaxFactRunes    = 500
)

type Location struct {
	Name         string   `json:"name"`
	Neighborhood string   `json:"neighborhood"`
	City         string   `json:"city"`
	State        string   `json:"state"`
	Country      string   `json:"country"`
	Latitude     *float64 `json:"latitude,omitempty"`
	Longitude    *float64 `json:"longitude,omitempty"`
}

// Address rebuilds the flat address string the app already displays.
func (l Location) Address() string {
	parts := []string{}
	for _, part := range []string{l.Neighborhood, l.City, l.State, l.Country} {
		if cleaned := strings.TrimSpace(part); cleaned != "" {
			parts = append(parts, cleaned)
		}
	}
	return strings.Join(parts, ", ")
}

func (l Location) hasText() bool {
	return strings.TrimSpace(l.Name+l.Neighborhood+l.City+l.State+l.Country) != ""
}

type Event struct {
	Name string `json:"name"`
	Date string `json:"date"`
	Time string `json:"time"`
}

// Extraction is the content-neutral half of a reel: what it is about, not how
// one user filed it.
type Extraction struct {
	Title           string     `json:"title"`
	Summary         string     `json:"summary"`
	ContentDomain   string     `json:"content_domain"`
	ContentFormat   string     `json:"content_format"`
	TopicalTags     []string   `json:"topical_tags"`
	KeyFacts        []string   `json:"key_facts"`
	Locations       []Location `json:"locations"`
	PeopleMentioned []string   `json:"people_mentioned"`
	ActionableItems []string   `json:"actionable_items"`
	Events          []Event    `json:"events"`
}

var (
	isoDate  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	isoClock = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)
)

// Normalize repairs what can be repaired and drops what cannot. A model is not
// a validator: this is where its output stops being text and becomes data.
func (e Extraction) Normalize() Extraction {
	normalized := Extraction{
		Title:         truncate(strings.TrimSpace(e.Title), MaxTitleRunes),
		Summary:       truncate(strings.TrimSpace(e.Summary), MaxSummaryRunes),
		ContentDomain: truncate(strings.TrimSpace(e.ContentDomain), MaxLabelRunes),
		ContentFormat: truncate(strings.TrimSpace(e.ContentFormat), MaxLabelRunes),

		TopicalTags:     cleanList(e.TopicalTags, MaxLabelRunes),
		KeyFacts:        cleanList(e.KeyFacts, MaxFactRunes),
		PeopleMentioned: cleanList(e.PeopleMentioned, MaxLabelRunes),
		ActionableItems: cleanList(e.ActionableItems, MaxFactRunes),
		Locations:       []Location{},
		Events:          []Event{},
	}

	for _, location := range e.Locations {
		if len(normalized.Locations) >= MaxLocations {
			break
		}
		cleaned := Location{
			Name:         truncate(strings.TrimSpace(location.Name), MaxLabelRunes),
			Neighborhood: truncate(strings.TrimSpace(location.Neighborhood), MaxLabelRunes),
			City:         truncate(strings.TrimSpace(location.City), MaxLabelRunes),
			State:        truncate(strings.TrimSpace(location.State), MaxLabelRunes),
			Country:      truncate(strings.TrimSpace(location.Country), MaxLabelRunes),
		}
		if !cleaned.hasText() {
			// A location with no place text is not a place.
			continue
		}
		if cleaned.Name == "" {
			// The model gave a hierarchy without a name: use the most specific
			// part rather than dropping a usable pin.
			cleaned.Name = firstNonEmpty(cleaned.Neighborhood, cleaned.City, cleaned.State, cleaned.Country)
		}
		cleaned.Latitude, cleaned.Longitude = validCoordinates(location.Latitude, location.Longitude)
		normalized.Locations = append(normalized.Locations, cleaned)
	}

	for _, event := range e.Events {
		if len(normalized.Events) >= MaxEvents {
			break
		}
		name := truncate(strings.TrimSpace(event.Name), MaxLabelRunes)
		if name == "" {
			continue
		}
		normalized.Events = append(normalized.Events, Event{
			Name: name,
			Date: validDate(event.Date),
			Time: validClock(event.Time),
		})
	}

	return normalized
}

// validCoordinates keeps a pair only when both halves are real and in range. A
// half-set or out-of-range pin is worse than no pin: it lands in the ocean.
func validCoordinates(latitude, longitude *float64) (*float64, *float64) {
	if latitude == nil || longitude == nil {
		return nil, nil
	}
	if *latitude < -90 || *latitude > 90 || *longitude < -180 || *longitude > 180 {
		return nil, nil
	}
	if *latitude == 0 && *longitude == 0 {
		// Null island is what a model returns when it has nothing.
		return nil, nil
	}
	return latitude, longitude
}

// validDate accepts only a complete, real calendar date. A model that guesses
// a year would put a reminder on the wrong day.
func validDate(value string) string {
	cleaned := strings.TrimSpace(value)
	if !isoDate.MatchString(cleaned) {
		return ""
	}
	parsed, err := time.Parse("2006-01-02", cleaned)
	if err != nil {
		return ""
	}
	// time.Parse accepts 2026-02-31 and rolls it over, which is not a date the
	// model meant.
	if parsed.Format("2006-01-02") != cleaned {
		return ""
	}
	return cleaned
}

func validClock(value string) string {
	cleaned := strings.TrimSpace(value)
	if !isoClock.MatchString(cleaned) {
		return ""
	}
	return cleaned
}

func cleanList(values []string, limit int) []string {
	cleaned := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		item := truncate(strings.TrimSpace(value), limit)
		if item == "" || seen[strings.ToLower(item)] {
			continue
		}
		seen[strings.ToLower(item)] = true
		cleaned = append(cleaned, item)
		if len(cleaned) >= MaxListItems {
			break
		}
	}
	return cleaned
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return strings.TrimSpace(string(runes[:limit]))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return "Unknown place"
}
