package ai

import "testing"

func floatPtr(value float64) *float64 { return &value }

func TestNormalizeRepairsLocations(t *testing.T) {
	extraction := Extraction{
		Locations: []Location{
			{Name: "  Artjuna Cafe  ", City: "Anjuna", Country: "India",
				Latitude: floatPtr(15.58), Longitude: floatPtr(73.74)},
			// No name, but a usable hierarchy: keep it with the most specific part.
			{City: "Panaji", Country: "India"},
			// Nothing at all: not a place.
			{},
			// Coordinates outside the world, and null island.
			{Name: "Broken", Latitude: floatPtr(120), Longitude: floatPtr(45)},
			{Name: "Null island", Latitude: floatPtr(0), Longitude: floatPtr(0)},
			// Half a pair is not a pin.
			{Name: "Half", Latitude: floatPtr(15.5)},
		},
	}

	got := extraction.Normalize()
	if len(got.Locations) != 5 {
		t.Fatalf("locations = %d, want the one with no text dropped: %+v", len(got.Locations), got.Locations)
	}
	if got.Locations[0].Name != "Artjuna Cafe" {
		t.Errorf("name = %q, want it trimmed", got.Locations[0].Name)
	}
	if got.Locations[0].Address() != "Anjuna, India" {
		t.Errorf("address = %q", got.Locations[0].Address())
	}
	if got.Locations[1].Name != "Panaji" {
		t.Errorf("name = %q, want the most specific part of the hierarchy", got.Locations[1].Name)
	}
	for _, location := range got.Locations[2:] {
		if location.Latitude != nil || location.Longitude != nil {
			t.Errorf("%q kept unusable coordinates: %+v", location.Name, location)
		}
	}
}

func TestNormalizeRepairsEvents(t *testing.T) {
	extraction := Extraction{
		Events: []Event{
			{Name: "Concert", Date: "2026-11-20", Time: "19:30"},
			// A date that does not exist must not become a reminder.
			{Name: "Impossible", Date: "2026-02-31", Time: "10:00"},
			{Name: "Partial", Date: "2026-11", Time: "25:00"},
			{Name: "  ", Date: "2026-11-20"},
		},
	}

	got := extraction.Normalize()
	if len(got.Events) != 3 {
		t.Fatalf("events = %d, want the unnamed one dropped: %+v", len(got.Events), got.Events)
	}
	if got.Events[0].Date != "2026-11-20" || got.Events[0].Time != "19:30" {
		t.Errorf("a valid event was changed: %+v", got.Events[0])
	}
	if got.Events[1].Date != "" {
		t.Errorf("31 February survived as %q", got.Events[1].Date)
	}
	if got.Events[2].Date != "" || got.Events[2].Time != "" {
		t.Errorf("a partial date or impossible clock survived: %+v", got.Events[2])
	}
}

func TestNormalizeBoundsAndDeduplicates(t *testing.T) {
	long := make([]string, 200)
	for i := range long {
		long[i] = "tag"
	}
	extraction := Extraction{
		Title:       string(make([]rune, MaxTitleRunes+50)),
		TopicalTags: append([]string{"Food", "food", " Food "}, long...),
	}

	got := extraction.Normalize()
	if len([]rune(got.Title)) > MaxTitleRunes {
		t.Errorf("title is %d runes, want at most %d", len([]rune(got.Title)), MaxTitleRunes)
	}
	// "Food", "food" and " Food " are one tag; "tag" repeated 200 times is one more.
	if len(got.TopicalTags) != 2 {
		t.Fatalf("tags = %v, want case-insensitive deduplication", got.TopicalTags)
	}
	if got.TopicalTags[0] != "Food" {
		t.Errorf("tags = %v, want the first spelling kept", got.TopicalTags)
	}
}

func TestNormalizeAlwaysReturnsSlices(t *testing.T) {
	got := Extraction{}.Normalize()
	if got.Locations == nil || got.Events == nil || got.TopicalTags == nil {
		t.Fatal("an empty extraction produced nil slices, which serialize as null")
	}
}

func TestValidateIsTheGateToAContentVersion(t *testing.T) {
	// An extraction with neither a title nor a summary describes nothing, and
	// persisting it would complete a job with an empty reel.
	if err := (Extraction{Title: " ", Summary: "\n"}).Validate(); err == nil {
		t.Fatal("an empty extraction validated")
	}
	if err := (Extraction{Title: "A cafe in Anjuna"}).Validate(); err != nil {
		t.Fatalf("a usable extraction failed validation: %v", err)
	}
	if err := (Extraction{Summary: "Three cafes worth saving."}).Validate(); err != nil {
		t.Fatalf("a summary-only extraction failed validation: %v", err)
	}
}

func TestStripFencesToleratesAModelThatIgnoresTheMIMEType(t *testing.T) {
	tests := []struct{ in, want string }{
		{"{\"a\":1}", "{\"a\":1}"},
		{"```json\n{\"a\":1}\n```", "{\"a\":1}"},
		{"```\n{\"a\":1}\n```", "{\"a\":1}"},
		{"  {\"a\":1}  ", "{\"a\":1}"},
	}
	for _, tt := range tests {
		if got := stripFences(tt.in); got != tt.want {
			t.Errorf("stripFences(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
