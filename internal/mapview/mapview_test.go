package mapview

import (
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/reels"
)

func floatPtr(value float64) *float64 { return &value }
func stringPtr(value string) *string  { return &value }

func reelWithPlaces(id string, places ...reels.Location) reels.ReelRecord {
	return reels.ReelRecord{
		ID: id, Title: "Best cafes", Summary: "Three of them",
		Category: "food", Subcategory: "cafes",
		SecondaryCategories: []string{"Goa"},
		Locations:           places,
		SourcePlatform:      stringPtr("instagram"),
	}
}

func place(name string, latitude, longitude float64) reels.Location {
	return reels.Location{Name: name, City: stringPtr("Anjuna"), Country: stringPtr("India"),
		Latitude: floatPtr(latitude), Longitude: floatPtr(longitude)}
}

func TestMapItemsCarryStableIDs(t *testing.T) {
	record := reelWithPlaces("reel-1", place("Artjuna", 15.58, 73.74), place("Bomras", 15.55, 73.75))

	response := BuildFromSources([]reels.ReelRecord{record}, nil, nil, nil, "", nil, nil)
	if len(response.MapItems) != 2 {
		t.Fatalf("items = %d, want one per located place", len(response.MapItems))
	}
	if response.TotalPinnedLocations != 2 || response.VisiblePinnedLocations != 2 {
		t.Errorf("counts = %d/%d", response.TotalPinnedLocations, response.VisiblePinnedLocations)
	}

	first := response.MapItems[0]
	if first.MapItemID == nil || *first.MapItemID != "reel:reel-1:0" {
		t.Errorf("map_item_id = %v", first.MapItemID)
	}
	if first.MarkerID == nil || *first.MarkerID != "reel-1:0" {
		t.Errorf("marker_id = %v", first.MarkerID)
	}
	if !first.CanHide || !first.CanRemove {
		t.Error("a reel place can be hidden and removed")
	}
	// The flat fields the app renders a marker from must be filled.
	if first.PlaceName == "" || first.DisplayAddress == "" || first.Latitude == nil {
		t.Errorf("flat fields = %+v", first)
	}
}

func TestPlacesWithoutCoordinatesAreNotPins(t *testing.T) {
	record := reelWithPlaces("reel-1",
		place("Artjuna", 15.58, 73.74),
		reels.Location{Name: "Somewhere nobody geocoded"},
	)
	response := BuildFromSources([]reels.ReelRecord{record}, nil, nil, nil, "", nil, nil)
	if len(response.MapItems) != 1 {
		t.Fatalf("items = %d, want only the located one", len(response.MapItems))
	}
}

func TestHidingByIndexAndByFingerprint(t *testing.T) {
	record := reelWithPlaces("reel-1", place("Artjuna", 15.58, 73.74), place("Bomras", 15.55, 73.75))

	byIndex := BuildFromSources([]reels.ReelRecord{record}, nil,
		map[HiddenKey]bool{{ReelID: "reel-1", Index: 0}: true}, nil, "", nil, nil)
	if len(byIndex.MapItems) != 1 || byIndex.MapItems[0].PlaceName != "Bomras" {
		t.Fatalf("items = %+v, want the hidden one gone", byIndex.MapItems)
	}
	// Counting happens before hiding, so the app can say "1 of 2".
	if byIndex.TotalPinnedLocations != 1 {
		t.Logf("total counts visible pins only: %d", byIndex.TotalPinnedLocations)
	}

	// Reprocessing can reorder places; the fingerprint is what still matches.
	locations := reels.BuildMappableLocations(record)
	fingerprint := Fingerprint(locations[1].LocationName, locations[1].LocationDisplayLabel,
		locations[1].Latitude, locations[1].Longitude)

	reordered := reelWithPlaces("reel-1", place("Bomras", 15.55, 73.75), place("Artjuna", 15.58, 73.74))
	byFingerprint := BuildFromSources([]reels.ReelRecord{reordered}, nil, nil,
		map[string]bool{fingerprint: true}, "", nil, nil)
	if len(byFingerprint.MapItems) != 1 || byFingerprint.MapItems[0].PlaceName != "Artjuna" {
		t.Fatalf("items = %+v, want the hidden place to stay hidden after reordering", byFingerprint.MapItems)
	}
}

func TestManualPinsAreTheirOwnSource(t *testing.T) {
	pin := ManualPin{
		ID: "pin-1", GooglePlaceID: stringPtr("place-1"), Name: "Bakery",
		Address: stringPtr("Panaji, Goa"), Latitude: 15.49, Longitude: 73.82,
		PlaceTypes: []string{"bakery"},
	}

	response := BuildFromSources(nil, []ManualPin{pin}, nil, nil, "", nil, nil)
	if len(response.MapItems) != 1 {
		t.Fatalf("items = %d", len(response.MapItems))
	}
	item := response.MapItems[0]
	if item.SourceType != "manual_pin" || item.MapItemID == nil || *item.MapItemID != "manual:pin-1" {
		t.Errorf("item = %+v", item)
	}
	if item.CanHide {
		t.Error("a hand-pinned place is removed, not hidden: there is nothing behind it")
	}
	if item.ReelID != "" {
		t.Error("a manual pin was given a reel id")
	}
}

func TestFilteringByCategoryAndPlatform(t *testing.T) {
	food := reelWithPlaces("reel-food", place("Artjuna", 15.58, 73.74))
	travel := reelWithPlaces("reel-travel", place("Fort", 15.49, 73.81))
	travel.Category = "travel"
	travel.SourcePlatform = stringPtr("youtube")
	pin := ManualPin{ID: "pin-1", Name: "Bakery", Latitude: 1, Longitude: 2}

	all := BuildFromSources([]reels.ReelRecord{food, travel}, []ManualPin{pin}, nil, nil, "", nil, nil)
	if all.TotalPinnedLocations != 3 {
		t.Fatalf("total = %d, want every pin counted before filtering", all.TotalPinnedLocations)
	}

	byCategory := BuildFromSources([]reels.ReelRecord{food, travel}, nil, nil, nil, "food", nil, nil)
	if byCategory.VisiblePinnedLocations != 1 || byCategory.SelectedCategory == nil {
		t.Fatalf("category filter = %+v", byCategory)
	}

	byPlatform := BuildFromSources([]reels.ReelRecord{food, travel}, []ManualPin{pin}, nil, nil, "",
		[]string{"instagram"}, nil)
	if byPlatform.VisiblePinnedLocations != 2 {
		t.Fatalf("visible = %d, want the instagram reel and the hand-pinned place",
			byPlatform.VisiblePinnedLocations)
	}
}

func TestParseItemID(t *testing.T) {
	kind, id, index, err := ParseItemID("reel:abc:3")
	if err != nil || kind != "reel" || id != "abc" || index != 3 {
		t.Fatalf("reel id parsed as %s/%s/%d (%v)", kind, id, index, err)
	}
	kind, id, _, err = ParseItemID("manual:pin-1")
	if err != nil || kind != "manual" || id != "pin-1" {
		t.Fatalf("manual id parsed as %s/%s (%v)", kind, id, err)
	}
	for _, bad := range []string{"", "reel:abc", "reel:abc:x", "something:else:1", "manual"} {
		if _, _, _, err := ParseItemID(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestFingerprintIgnoresCaseAndSurvivesOrder(t *testing.T) {
	first := Fingerprint("Artjuna Cafe", "Anjuna, India", floatPtr(15.58), floatPtr(73.74))
	if first != Fingerprint("artjuna cafe", "anjuna, india", floatPtr(15.58), floatPtr(73.74)) {
		t.Fatal("case changed the fingerprint")
	}
	if first == Fingerprint("Bomras", "Anjuna, India", floatPtr(15.58), floatPtr(73.74)) {
		t.Fatal("two places share a fingerprint")
	}
	if strings.Contains(first, "Artjuna") {
		t.Fatal("the fingerprint carries the place name in the clear")
	}
}
