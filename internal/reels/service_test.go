package reels

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

const testWebBase = "https://reelpin.in"

type stubReader struct {
	records []ReelRecord
	err     error
	lastOpt ListOptions
}

func (s *stubReader) List(_ context.Context, _ string, options ListOptions) ([]ReelRecord, error) {
	s.lastOpt = options
	return s.records, s.err
}

func (s *stubReader) Get(_ context.Context, _ string, id uuid.UUID) (ReelRecord, error) {
	for _, record := range s.records {
		if record.ID == id.String() {
			return record, nil
		}
	}
	return ReelRecord{}, ErrNotFound
}

func (s *stubReader) Facets(context.Context, string) ([]FacetRow, error) { return nil, s.err }
func (s *stubReader) Library(context.Context, string, int) ([]ReelRecord, error) {
	return s.records, s.err
}
func (s *stubReader) Stats(context.Context, string) (LibraryStats, error) {
	return LibraryStats{}, s.err
}

func longFormRecord() ReelRecord {
	return ReelRecord{
		ID:              "22222222-2222-4222-8222-222222222222",
		Title:           "Best cafes in Goa",
		Transcript:      "spoken words",
		KeyFacts:        []string{"Opens at 8am"},
		PeopleMentioned: []string{"Rhea"},
		ActionableItems: []string{"Go early"},
		Events:          []Event{{Name: "Market", Date: "2026-09-05"}},
		CreatedAt:       ptr(now),
	}
}

func encodedKeys(t *testing.T, value any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]any
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	return keys
}

func TestParseView(t *testing.T) {
	for raw, want := range map[string]View{"": ViewDetailed, "detailed": ViewDetailed, "concise": ViewConcise} {
		if got, err := ParseView(raw); err != nil || got != want {
			t.Errorf("ParseView(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	if _, err := ParseView("verbose"); !errors.Is(err, ErrInvalidView) {
		t.Errorf("ParseView(verbose) error = %v, want ErrInvalidView", err)
	}
}

func TestConciseDropsTheLongFormFields(t *testing.T) {
	longForm := []string{"transcript", "key_facts", "people_mentioned", "actionable_items", "events"}

	concise := encodedKeys(t, BuildDisplayReel(longFormRecord(), Display{Now: now, View: ViewConcise}))
	for _, key := range longForm {
		if _, present := concise[key]; present {
			t.Errorf("concise carries %s", key)
		}
	}
	if concise["title"] != "Best cafes in Goa" {
		t.Errorf("concise lost the title: %v", concise["title"])
	}

	detailed := encodedKeys(t, BuildDisplayReel(longFormRecord(), Display{Now: now, View: ViewDetailed}))
	for _, key := range longForm[1:] {
		if _, present := detailed[key]; !present {
			t.Errorf("detailed is missing %s", key)
		}
	}

	// An empty list is still a list in the detailed view: the app decodes these
	// into non-nullable fields.
	empty := encodedKeys(t, BuildDisplayReel(ReelRecord{ID: "a"}, Display{Now: now}))
	if facts, ok := empty["key_facts"].([]any); !ok || len(facts) != 0 {
		t.Errorf("key_facts = %#v, want []", empty["key_facts"])
	}
}

func TestEveryReelShapeLinksItsPublicPage(t *testing.T) {
	record := longFormRecord()
	want := "https://reelpin.in/r/" + record.ID

	if got := BuildDisplayReel(record, Display{WebBaseURL: testWebBase + "/"}).WebURL; got != want {
		t.Errorf("display web_url = %q, want %q", got, want)
	}
	if got := BuildReel(record, Display{WebBaseURL: testWebBase}).WebURL; got != want {
		t.Errorf("full reel web_url = %q, want %q", got, want)
	}
}

func TestServiceListOverFetchesAndProjects(t *testing.T) {
	reader := &stubReader{records: []ReelRecord{longFormRecord()}}
	service := NewService(reader, testWebBase, func() time.Time { return now })

	page, err := service.List(context.Background(), "user", ListParams{Limit: 10, View: ViewConcise})
	if err != nil {
		t.Fatal(err)
	}
	if reader.lastOpt.Limit != 11 {
		t.Errorf("reader limit = %d, want one over the page", reader.lastOpt.Limit)
	}
	if len(page.Reels) != 1 || page.Reels[0].KeyFacts != nil {
		t.Errorf("reels = %+v, want one concise reel", page.Reels)
	}
}

// TestServiceGetCarriesTheListProjection pins that one reel reads the same in
// the detail view as it does in a list: every display field the list item
// carries is on the detail body with the same value, and the subcategory has
// one name, with the old one kept beside it for a release.
func TestServiceGetCarriesTheListProjection(t *testing.T) {
	record := longFormRecord()
	record.Category = "food_and_drink"
	record.Subcategory = "cafes"
	record.CreatedAt = ptr(now.AddDate(0, 0, -2))
	record.Locations = []Location{
		{Name: "Artjuna Cafe", Latitude: ptr(15.5834), Longitude: ptr(73.7407)},
		{Name: "No coordinates"},
	}
	service := NewService(&stubReader{records: []ReelRecord{record}}, testWebBase, func() time.Time { return now })

	reel, err := service.Get(context.Background(), "user", uuid.MustParse(record.ID))
	if err != nil {
		t.Fatal(err)
	}
	page, err := service.List(context.Background(), "user", ListParams{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}

	detail := encodedKeys(t, reel)
	card := encodedKeys(t, page.Reels[0])
	for _, key := range []string{
		"category_label", "sub_category", "sub_category_label", "relative_date",
		"display_date", "saved_date_key", "content_type", "parse_status", "has_map_locations",
		"mappable_locations", "map_location_count", "primary_location_label", "events",
	} {
		if _, present := detail[key]; !present {
			t.Errorf("the detail body has no %s", key)
			continue
		}
		if got, want := mustMarshal(t, detail[key]), mustMarshal(t, card[key]); got != want {
			t.Errorf("%s = %s on the detail, %s on the list", key, got, want)
		}
	}
	if detail["sub_category"] != "cafes" || detail["subcategory"] != "cafes" {
		t.Errorf("sub_category = %v, subcategory = %v, want cafes for both", detail["sub_category"], detail["subcategory"])
	}
	if detail["relative_date"] != "2 days ago" || detail["map_location_count"] != float64(1) {
		t.Errorf("relative_date = %v, map_location_count = %v", detail["relative_date"], detail["map_location_count"])
	}
	if detail["transcript"] != "spoken words" {
		t.Errorf("transcript = %v, the detail keeps every record field", detail["transcript"])
	}
}

func mustMarshal(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestServiceGetKeepsNotFound(t *testing.T) {
	service := NewService(&stubReader{}, testWebBase, nil)
	if _, err := service.Get(context.Background(), "user", uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestServiceDiscoverNamesTheReadThatFailed(t *testing.T) {
	service := NewService(&stubReader{err: errors.New("boom")}, testWebBase, nil)
	_, err := service.Discover(context.Background(), "user", DiscoverParams{Limit: 5})
	if err == nil || err.Error() != "reading recent saves: boom" {
		t.Errorf("error = %v", err)
	}
}
