package reels

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

var shareCardContentTypes = map[string]bool{"reel": true, "post": true, "carousel": true}

type MappableLocation struct {
	Name                 string   `json:"name"`
	Address              *string  `json:"address"`
	Neighborhood         *string  `json:"neighborhood"`
	City                 *string  `json:"city"`
	State                *string  `json:"state"`
	Country              *string  `json:"country"`
	Latitude             *float64 `json:"latitude"`
	Longitude            *float64 `json:"longitude"`
	MarkerID             string   `json:"marker_id"`
	LocationName         string   `json:"location_name"`
	LocationAddress      string   `json:"location_address"`
	LocationDisplayLabel string   `json:"location_display_label"`
	GoogleMapsURL        *string  `json:"google_maps_url"`
}

// View is how much of a reel a response carries. Detailed is everything the
// app reads. Concise drops the long-form extraction, which an agent or a
// browser card pays for in tokens and bytes without showing.
type View string

const (
	ViewDetailed View = "detailed"
	ViewConcise  View = "concise"
)

// ErrInvalidView is a view name this API does not know.
var ErrInvalidView = errors.New("view must be concise or detailed")

// ParseView reads a client's view. Absent means detailed, so a client that has
// never heard of views sees exactly what it always did.
func ParseView(raw string) (View, error) {
	switch View(strings.TrimSpace(raw)) {
	case "", ViewDetailed:
		return ViewDetailed, nil
	case ViewConcise:
		return ViewConcise, nil
	}
	return "", ErrInvalidView
}

// Display is what presenting a reel needs that the record does not carry: the
// clock relative dates are measured against, the public site its page lives
// on, and how much of it to send.
type Display struct {
	Now        time.Time
	WebBaseURL string
	View       View
}

// WebURL is the reel's public page.
func WebURL(baseURL, reelID string) string {
	return strings.TrimSuffix(baseURL, "/") + "/r/" + reelID
}

// The four long-form fields are omitzero: the detailed view always fills them
// with a list, empty or not, and only the concise view leaves them nil.
type DisplayReel struct {
	ID                   string             `json:"id"`
	URL                  string             `json:"url"`
	WebURL               string             `json:"web_url"`
	ThumbnailURL         *string            `json:"thumbnail_url"`
	SourcePlatform       *string            `json:"source_platform"`
	SourceContentType    *string            `json:"source_content_type"`
	Title                string             `json:"title"`
	Summary              string             `json:"summary"`
	ContentType          string             `json:"content_type"`
	ParseStatus          string             `json:"parse_status"`
	Category             string             `json:"category"`
	SubCategory          string             `json:"sub_category"`
	CategoryLabel        string             `json:"category_label"`
	SubCategoryLabel     string             `json:"sub_category_label"`
	CreatedAt            *string            `json:"created_at"`
	DisplayDate          string             `json:"display_date"`
	RelativeDate         string             `json:"relative_date"`
	SavedDateKey         *string            `json:"saved_date_key"`
	HasMapLocations      bool               `json:"has_map_locations"`
	MappableLocations    []MappableLocation `json:"mappable_locations"`
	PrimaryLocationLabel *string            `json:"primary_location_label"`
	MapLocationCount     int                `json:"map_location_count"`
	KeyFacts             []string           `json:"key_facts,omitzero"`
	PeopleMentioned      []string           `json:"people_mentioned,omitzero"`
	ActionableItems      []string           `json:"actionable_items,omitzero"`
	Events               []Event            `json:"events,omitzero"`
}

// Page size bounds are part of the contract, so they live with the type the
// contract describes rather than in a handler.
const (
	DefaultPageSize = 25
	MaxPageSize     = 100
)

// ListResponse is one page. There is no total count: counting the whole set on
// every page is a second query whose answer is stale before it is read, and no
// client needs it to render a list that pages forward.
type ListResponse struct {
	NextCursor *string       `json:"next_cursor"`
	HasMore    bool          `json:"has_more"`
	Limit      int           `json:"limit"`
	Reels      []DisplayReel `json:"reels"`
}

func BuildDisplayReel(record ReelRecord, display Display) DisplayReel {
	locations := BuildMappableLocations(record)
	category := cleanLabel(record.Category, "Other")
	subcategory := cleanLabel(record.Subcategory, "Other")

	url := strings.TrimSpace(record.URL)
	if url == "" {
		url = optionalString(record.NormalizedURL)
	}

	parseStatus := optionalString(record.ParseStatus)
	if parseStatus == "" {
		parseStatus = "parsed"
	}

	var primaryLabel *string
	if len(locations) > 0 {
		label := locations[0].LocationDisplayLabel
		primaryLabel = &label
	}

	reel := DisplayReel{
		ID:                   record.ID,
		URL:                  url,
		WebURL:               WebURL(display.WebBaseURL, record.ID),
		ThumbnailURL:         cleanOptional(record.ThumbnailURL),
		SourcePlatform:       cleanOptional(record.SourcePlatform),
		SourceContentType:    cleanOptional(record.SourceContentType),
		Title:                record.Title,
		Summary:              record.Summary,
		ContentType:          ShareCardContentType(record),
		ParseStatus:          parseStatus,
		Category:             category,
		SubCategory:          subcategory,
		CategoryLabel:        Labelize(category),
		SubCategoryLabel:     Labelize(subcategory),
		CreatedAt:            isoTimestamp(record.CreatedAt),
		DisplayDate:          DisplayDate(record.CreatedAt),
		RelativeDate:         RelativeDate(record.CreatedAt, display.Now),
		SavedDateKey:         SavedDateKey(record.CreatedAt),
		HasMapLocations:      len(locations) > 0,
		MappableLocations:    locations,
		PrimaryLocationLabel: primaryLabel,
		MapLocationCount:     len(locations),
	}
	if display.View != ViewConcise {
		reel.KeyFacts = nonNil(record.KeyFacts)
		reel.PeopleMentioned = nonNil(record.PeopleMentioned)
		reel.ActionableItems = nonNil(record.ActionableItems)
		reel.Events = nonNilEvents(record.Events)
	}
	return reel
}

// BuildListResponse turns one over-fetched page into the wire shape. The caller
// asks for limit+1 records; the extra one answers has_more and is then dropped,
// which is one query rather than a page query plus a count query.
func BuildListResponse(records []ReelRecord, limit int, display Display) ListResponse {
	hasMore := len(records) > limit
	if hasMore {
		records = records[:limit]
	}

	deduped := DedupeRecords(records)
	reels := make([]DisplayReel, 0, len(deduped))
	for _, record := range deduped {
		reels = append(reels, BuildDisplayReel(record, display))
	}

	response := ListResponse{
		HasMore: hasMore,
		Limit:   limit,
		Reels:   reels,
	}
	if hasMore && len(records) > 0 {
		if cursor, ok := CursorFor(records[len(records)-1]); ok {
			encoded := cursor.Encode()
			response.NextCursor = &encoded
		} else {
			// Without a resumable position the honest answer is that this is
			// the last page, not a cursor that would restart from the top.
			response.HasMore = false
		}
	}
	return response
}

// Reel is the full single-reel body, transcript included, with the display
// projection a list item carries. Subcategory repeats SubCategory under the
// name this shape sent before sub_category was the one name; it is deprecated
// and goes after one release.
type Reel struct {
	ID                  string     `json:"id"`
	UserID              string     `json:"user_id"`
	URL                 string     `json:"url"`
	WebURL              string     `json:"web_url"`
	ThumbnailURL        *string    `json:"thumbnail_url"`
	NormalizedURL       *string    `json:"normalized_url"`
	SourcePlatform      *string    `json:"source_platform"`
	SourceContentType   *string    `json:"source_content_type"`
	SourceContentID     *string    `json:"source_content_id"`
	ProcessingVersion   *string    `json:"processing_version"`
	IngestionMethod     *string    `json:"ingestion_method"`
	TranscriptSource    *string    `json:"transcript_source"`
	Title               string     `json:"title"`
	Summary             string     `json:"summary"`
	ContentType         string     `json:"content_type"`
	Transcript          string     `json:"transcript"`
	Category            string     `json:"category"`
	Subcategory         string     `json:"subcategory"`
	SecondaryCategories []string   `json:"secondary_categories"`
	KeyFacts            []string   `json:"key_facts"`
	Locations           []Location `json:"locations"`
	PeopleMentioned     []string   `json:"people_mentioned"`
	ActionableItems     []string   `json:"actionable_items"`
	Events              []Event    `json:"events"`
	CreatedAt           *string    `json:"created_at"`

	ParseStatus          string             `json:"parse_status"`
	SubCategory          string             `json:"sub_category"`
	CategoryLabel        string             `json:"category_label"`
	SubCategoryLabel     string             `json:"sub_category_label"`
	DisplayDate          string             `json:"display_date"`
	RelativeDate         string             `json:"relative_date"`
	SavedDateKey         *string            `json:"saved_date_key"`
	HasMapLocations      bool               `json:"has_map_locations"`
	MappableLocations    []MappableLocation `json:"mappable_locations"`
	PrimaryLocationLabel *string            `json:"primary_location_label"`
	MapLocationCount     int                `json:"map_location_count"`
}

// BuildReel presents one reel in full. The display fields come from the same
// BuildDisplayReel a list uses, so a reel reads the same in both places.
func BuildReel(record ReelRecord, display Display) Reel {
	category := record.Category
	if strings.TrimSpace(category) == "" {
		category = "Other"
	}
	subcategory := record.Subcategory
	if strings.TrimSpace(subcategory) == "" {
		subcategory = "Other"
	}
	locations := record.Locations
	if locations == nil {
		locations = []Location{}
	}
	display.View = ViewDetailed
	card := BuildDisplayReel(record, display)

	return Reel{
		ID:                  record.ID,
		UserID:              record.UserID,
		URL:                 record.URL,
		WebURL:              card.WebURL,
		ThumbnailURL:        cleanOptional(record.ThumbnailURL),
		NormalizedURL:       record.NormalizedURL,
		SourcePlatform:      record.SourcePlatform,
		SourceContentType:   record.SourceContentType,
		SourceContentID:     record.SourceContentID,
		ProcessingVersion:   record.ProcessingVersion,
		IngestionMethod:     record.IngestionMethod,
		TranscriptSource:    record.TranscriptSource,
		Title:               record.Title,
		Summary:             record.Summary,
		ContentType:         ShareCardContentType(record),
		Transcript:          record.Transcript,
		Category:            category,
		Subcategory:         subcategory,
		SecondaryCategories: nonNil(record.SecondaryCategories),
		KeyFacts:            nonNil(record.KeyFacts),
		Locations:           locations,
		PeopleMentioned:     nonNil(record.PeopleMentioned),
		ActionableItems:     nonNil(record.ActionableItems),
		Events:              card.Events,
		CreatedAt:           isoTimestamp(record.CreatedAt),

		ParseStatus:          card.ParseStatus,
		SubCategory:          subcategory,
		CategoryLabel:        card.CategoryLabel,
		SubCategoryLabel:     card.SubCategoryLabel,
		DisplayDate:          card.DisplayDate,
		RelativeDate:         card.RelativeDate,
		SavedDateKey:         card.SavedDateKey,
		HasMapLocations:      card.HasMapLocations,
		MappableLocations:    card.MappableLocations,
		PrimaryLocationLabel: card.PrimaryLocationLabel,
		MapLocationCount:     card.MapLocationCount,
	}
}

func ShareCardContentType(record ReelRecord) string {
	value := strings.ToLower(strings.TrimSpace(optionalString(record.SourceContentType)))
	if shareCardContentTypes[value] {
		return value
	}
	if value == "reels" {
		return "reel"
	}
	return "post"
}

func BuildMappableLocations(record ReelRecord) []MappableLocation {
	locations := make([]MappableLocation, 0, len(record.Locations))
	for index, location := range record.Locations {
		if location.Latitude == nil || location.Longitude == nil {
			continue
		}
		latitude, longitude := *location.Latitude, *location.Longitude

		name := strings.TrimSpace(location.Name)
		address := locationAddress(location)
		displayLabel := name
		if displayLabel == "" {
			displayLabel = address
		}
		if displayLabel == "" {
			displayLabel = fmt.Sprintf("%.5f, %.5f", latitude, longitude)
		}

		query := fmt.Sprintf("%v,%v", latitude, longitude)
		if name != "" {
			query = percentEncode(name)
		}
		mapsURL := "https://www.google.com/maps/search/?api=1&query=" + query

		locations = append(locations, MappableLocation{
			Name:                 name,
			Address:              location.Address,
			Neighborhood:         location.Neighborhood,
			City:                 location.City,
			State:                location.State,
			Country:              location.Country,
			Latitude:             location.Latitude,
			Longitude:            location.Longitude,
			MarkerID:             fmt.Sprintf("%s:%d", record.ID, index),
			LocationName:         name,
			LocationAddress:      address,
			LocationDisplayLabel: displayLabel,
			GoogleMapsURL:        &mapsURL,
		})
	}
	return locations
}

func DedupeRecords(records []ReelRecord) []ReelRecord {
	seen := map[string]bool{}
	deduped := make([]ReelRecord, 0, len(records))
	for _, record := range records {
		if record.ID == "" || seen[record.ID] {
			continue
		}
		seen[record.ID] = true
		deduped = append(deduped, record)
	}
	return deduped
}

func Labelize(value string) string {
	cleaned := cleanLabel(value, "Other")
	cleaned = strings.NewReplacer("_", " ", "-", " ").Replace(cleaned)
	return titleCase(strings.Join(strings.Fields(cleaned), " "))
}

func DisplayDate(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.UTC().Format("Jan 2, 2006")
}

func SavedDateKey(value *time.Time) *string {
	if value == nil {
		return nil
	}
	key := value.UTC().Format("2006-01-02")
	return &key
}

func RelativeDate(value *time.Time, now time.Time) string {
	if value == nil {
		return ""
	}
	days := int(truncateToDay(now.UTC()).Sub(truncateToDay(value.UTC())).Hours() / 24)
	switch {
	case days <= 0:
		return "Today"
	case days == 1:
		return "Yesterday"
	case days < 7:
		return fmt.Sprintf("%d days ago", days)
	case days < 30:
		return pluralAgo(days/7, "week")
	case days < 365:
		return pluralAgo(days/30, "month")
	default:
		return pluralAgo(days/365, "year")
	}
}

func pluralAgo(count int, unit string) string {
	if count == 1 {
		return "1 " + unit + " ago"
	}
	return fmt.Sprintf("%d %ss ago", count, unit)
}

func truncateToDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func locationAddress(location Location) string {
	parts := []*string{location.Address, location.Neighborhood, location.City, location.State, location.Country}
	cleaned := make([]string, 0, len(parts))
	for _, part := range parts {
		if value := strings.TrimSpace(optionalString(part)); value != "" {
			cleaned = append(cleaned, value)
		}
	}
	return strings.Join(cleaned, ", ")
}

// percentEncode matches Python's quote(value, safe="").
func percentEncode(value string) string {
	const unreserved = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_.-~"
	var out strings.Builder
	for _, b := range []byte(value) {
		if strings.IndexByte(unreserved, b) >= 0 {
			out.WriteByte(b)
			continue
		}
		fmt.Fprintf(&out, "%%%02X", b)
	}
	return out.String()
}

func titleCase(value string) string {
	var out strings.Builder
	newWord := true
	for _, r := range value {
		switch {
		case r == ' ':
			newWord = true
			out.WriteRune(r)
		case newWord:
			out.WriteString(strings.ToUpper(string(r)))
			newWord = false
		default:
			out.WriteString(strings.ToLower(string(r)))
		}
	}
	return out.String()
}

func cleanLabel(value, fallback string) string {
	cleaned := strings.TrimSpace(value)
	if cleaned == "" {
		return fallback
	}
	return cleaned
}

func cleanOptional(value *string) *string {
	if value == nil {
		return nil
	}
	cleaned := strings.TrimSpace(*value)
	if cleaned == "" {
		return nil
	}
	return &cleaned
}

func optionalString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func isoTimestamp(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.UTC().Format(time.RFC3339Nano)
	return &formatted
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func nonNilEvents(values []Event) []Event {
	if values == nil {
		return []Event{}
	}
	return values
}

func sortedByCountThenName(counts map[string]int, lowercaseName bool) []string {
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if counts[names[i]] != counts[names[j]] {
			return counts[names[i]] > counts[names[j]]
		}
		if lowercaseName {
			return strings.ToLower(names[i]) < strings.ToLower(names[j])
		}
		return names[i] < names[j]
	})
	return names
}
