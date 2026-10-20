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
	if got := BuildReel(record, testWebBase).WebURL; got != want {
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
