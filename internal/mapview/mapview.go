// Package mapview builds the map and Discover screens. Both read the same
// library from three sources: places extracted from reels, places the user
// pinned by hand, and the reel places they chose to hide.
package mapview

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/XploY04/reelpin-go/internal/reels"
)

// MapItem is one pin. The flat display fields exist because the app renders a
// marker without walking into the location object.
type MapItem struct {
	ReelID               string                   `json:"reel_id"`
	Title                string                   `json:"title"`
	Summary              string                   `json:"summary"`
	Category             string                   `json:"category"`
	SubCategory          string                   `json:"sub_category"`
	CategoryLabel        string                   `json:"category_label"`
	SubCategoryLabel     string                   `json:"sub_category_label"`
	SecondaryCategories  []string                 `json:"secondary_categories"`
	Locations            []reels.MappableLocation `json:"locations"`
	MapItemID            *string                  `json:"map_item_id"`
	SourceType           string                   `json:"source_type"`
	SourceID             *string                  `json:"source_id"`
	DisplayTitle         string                   `json:"display_title"`
	ShortDetail          string                   `json:"short_detail"`
	MarkerID             *string                  `json:"marker_id"`
	Latitude             *float64                 `json:"latitude"`
	Longitude            *float64                 `json:"longitude"`
	PlaceName            string                   `json:"place_name"`
	DisplayAddress       string                   `json:"display_address"`
	LocationName         string                   `json:"location_name"`
	LocationAddress      string                   `json:"location_address"`
	LocationDisplayLabel string                   `json:"location_display_label"`
	GoogleMapsURL        *string                  `json:"google_maps_url"`
	GooglePlaceID        *string                  `json:"google_place_id"`
	PlaceTypes           []string                 `json:"place_types"`
	CanHide              bool                     `json:"can_hide"`
	CanRemove            bool                     `json:"can_remove"`
}

type MapResponse struct {
	TotalPinnedLocations   int       `json:"total_pinned_locations"`
	VisiblePinnedLocations int       `json:"visible_pinned_locations"`
	SelectedCategory       *string   `json:"selected_category"`
	MapItems               []MapItem `json:"map_items"`
}

// HiddenKey identifies one hidden place on one reel.
type HiddenKey struct {
	ReelID string
	Index  int
}

// ManualPin is a place the user pinned themselves.
type ManualPin struct {
	ID                  string
	GooglePlaceID       *string
	Name                string
	Address             *string
	Latitude            float64
	Longitude           float64
	GoogleMapsURL       *string
	Category            string
	Subcategory         string
	SecondaryCategories []string
	PlaceTypes          []string
}

// itemID is what the client sends back to hide or remove a pin. It names the
// source and the exact place, so it stays stable across reprocessing.
func reelItemID(reelID string, index int) string {
	return fmt.Sprintf("reel:%s:%d", reelID, index)
}

func manualItemID(pinID string) string { return "manual:" + pinID }

// ParseItemID splits an id back into its source and identifier.
func ParseItemID(itemID string) (kind, id string, index int, err error) {
	parts := strings.Split(itemID, ":")
	switch {
	case len(parts) == 2 && parts[0] == "manual":
		return "manual", parts[1], 0, nil
	case len(parts) == 3 && parts[0] == "reel":
		if _, scanErr := fmt.Sscanf(parts[2], "%d", &index); scanErr != nil {
			return "", "", 0, fmt.Errorf("map item id has no place index")
		}
		return "reel", parts[1], index, nil
	default:
		return "", "", 0, fmt.Errorf("map item id is not recognised")
	}
}

// Fingerprint identifies a place by what it is rather than where it came from,
// so hiding survives a reel being reprocessed and its places reordered.
func Fingerprint(placeName, displayAddress string, latitude, longitude *float64) string {
	parts := []string{placeName, displayAddress, formatCoordinate(latitude), formatCoordinate(longitude)}
	sum := sha256.Sum256([]byte(strings.ToLower(strings.Join(parts, "|"))))
	return hex.EncodeToString(sum[:])
}

func formatCoordinate(value *float64) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(*value)
}

// BuildFromSources merges the three sources into one map. Counting happens
// before filtering, so the app can say "12 of 40" honestly.
func BuildFromSources(
	records []reels.ReelRecord,
	manualPins []ManualPin,
	hidden map[HiddenKey]bool,
	hiddenFingerprints map[string]bool,
	selectedCategory string,
	selectedPlatforms []string,
	now func() string,
) MapResponse {
	items := []MapItem{}

	for _, record := range reels.DedupeRecords(records) {
		category := labelOr(record.Category, "Other")
		subcategory := labelOr(record.Subcategory, "Other")

		for index, location := range reels.BuildMappableLocations(record) {
			fingerprint := Fingerprint(location.LocationName, location.LocationDisplayLabel,
				location.Latitude, location.Longitude)
			if hidden[HiddenKey{ReelID: record.ID, Index: index}] || hiddenFingerprints[fingerprint] {
				continue
			}

			itemID := reelItemID(record.ID, index)
			markerID := location.MarkerID
			sourceID := record.ID
			items = append(items, MapItem{
				ReelID:               record.ID,
				Title:                record.Title,
				Summary:              record.Summary,
				Category:             category,
				SubCategory:          subcategory,
				CategoryLabel:        reels.Labelize(category),
				SubCategoryLabel:     reels.Labelize(subcategory),
				SecondaryCategories:  cleanStrings(record.SecondaryCategories),
				Locations:            []reels.MappableLocation{location},
				MapItemID:            &itemID,
				SourceType:           "reel",
				SourceID:             &sourceID,
				DisplayTitle:         record.Title,
				ShortDetail:          record.Summary,
				MarkerID:             &markerID,
				Latitude:             location.Latitude,
				Longitude:            location.Longitude,
				PlaceName:            location.LocationName,
				DisplayAddress:       location.LocationDisplayLabel,
				LocationName:         location.LocationName,
				LocationAddress:      location.LocationAddress,
				LocationDisplayLabel: location.LocationDisplayLabel,
				GoogleMapsURL:        location.GoogleMapsURL,
				PlaceTypes:           []string{},
				CanHide:              true,
				CanRemove:            true,
			})
		}
	}

	for _, pin := range manualPins {
		items = append(items, manualPinItem(pin))
	}

	// Platform filtering applies to reel pins only: a hand-pinned place has no
	// platform, and dropping it would be surprising.
	allowedReels := map[string]bool{}
	if len(selectedPlatforms) > 0 {
		for _, record := range records {
			if containsString(selectedPlatforms, reels.RecordPlatform(record.SourcePlatform)) {
				allowedReels[record.ID] = true
			}
		}
	}

	visible := make([]MapItem, 0, len(items))
	for _, item := range items {
		if selectedCategory != "" && !strings.EqualFold(item.Category, selectedCategory) {
			continue
		}
		if len(selectedPlatforms) > 0 && item.SourceType == "reel" && !allowedReels[item.ReelID] {
			continue
		}
		visible = append(visible, item)
	}

	response := MapResponse{
		TotalPinnedLocations:   len(items),
		VisiblePinnedLocations: len(visible),
		MapItems:               visible,
	}
	if selectedCategory != "" {
		response.SelectedCategory = &selectedCategory
	}
	return response
}

func manualPinItem(pin ManualPin) MapItem {
	itemID := manualItemID(pin.ID)
	address := ""
	if pin.Address != nil {
		address = *pin.Address
	}
	latitude, longitude := pin.Latitude, pin.Longitude

	location := reels.MappableLocation{
		Name:                 pin.Name,
		Address:              pin.Address,
		Latitude:             &latitude,
		Longitude:            &longitude,
		MarkerID:             itemID,
		LocationName:         pin.Name,
		LocationAddress:      address,
		LocationDisplayLabel: firstNonEmpty(pin.Name, address),
		GoogleMapsURL:        pin.GoogleMapsURL,
	}

	category := labelOr(pin.Category, "Places")
	subcategory := labelOr(pin.Subcategory, "Saved")

	return MapItem{
		Title:                pin.Name,
		Category:             category,
		SubCategory:          subcategory,
		CategoryLabel:        reels.Labelize(category),
		SubCategoryLabel:     reels.Labelize(subcategory),
		SecondaryCategories:  cleanStrings(pin.SecondaryCategories),
		Locations:            []reels.MappableLocation{location},
		MapItemID:            &itemID,
		SourceType:           "manual_pin",
		SourceID:             &pin.ID,
		DisplayTitle:         pin.Name,
		ShortDetail:          address,
		MarkerID:             &itemID,
		Latitude:             &latitude,
		Longitude:            &longitude,
		PlaceName:            pin.Name,
		DisplayAddress:       address,
		LocationName:         pin.Name,
		LocationAddress:      address,
		LocationDisplayLabel: firstNonEmpty(pin.Name, address),
		GoogleMapsURL:        pin.GoogleMapsURL,
		GooglePlaceID:        pin.GooglePlaceID,
		PlaceTypes:           cleanStrings(pin.PlaceTypes),
		// A hand-pinned place is removed, not hidden: there is nothing behind it.
		CanHide:   false,
		CanRemove: true,
	}
}

func labelOr(value, fallback string) string {
	if cleaned := strings.TrimSpace(value); cleaned != "" {
		return cleaned
	}
	return fallback
}

func cleanStrings(values []string) []string {
	cleaned := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			cleaned = append(cleaned, trimmed)
		}
	}
	return cleaned
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if cleaned := strings.TrimSpace(value); cleaned != "" {
			return cleaned
		}
	}
	return ""
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
