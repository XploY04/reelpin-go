package reels

import (
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

type DisplayReel struct {
	ID                   string             `json:"id"`
	URL                  string             `json:"url"`
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
	KeyFacts             []string           `json:"key_facts"`
	PeopleMentioned      []string           `json:"people_mentioned"`
	ActionableItems      []string           `json:"actionable_items"`
	Events               []Event            `json:"events"`
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

func BuildDisplayReel(record ReelRecord, now time.Time) DisplayReel {
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

	return DisplayReel{
		ID:                   record.ID,
		URL:                  url,
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
		RelativeDate:         RelativeDate(record.CreatedAt, now),
		SavedDateKey:         SavedDateKey(record.CreatedAt),
		HasMapLocations:      len(locations) > 0,
		MappableLocations:    locations,
		PrimaryLocationLabel: primaryLabel,
		MapLocationCount:     len(locations),
		KeyFacts:             nonNil(record.KeyFacts),
		PeopleMentioned:      nonNil(record.PeopleMentioned),
		ActionableItems:      nonNil(record.ActionableItems),
		Events:               nonNilEvents(record.Events),
	}
}

// BuildListResponse mirrors the Python pagination contract: the caller fetches
// one extra row, trims it, and passes the estimated total.
// BuildListResponse turns one over-fetched page into the wire shape. The caller
// asks for limit+1 records; the extra one answers has_more and is then dropped,
// which is one query rather than a page query plus a count query.
func BuildListResponse(records []ReelRecord, limit int, now time.Time) ListResponse {
	hasMore := len(records) > limit
	if hasMore {
		records = records[:limit]
	}

	deduped := DedupeRecords(records)
	display := make([]DisplayReel, 0, len(deduped))
	for _, record := range deduped {
		display = append(display, BuildDisplayReel(record, now))
	}

	response := ListResponse{
		HasMore: hasMore,
		Limit:   limit,
		Reels:   display,
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
