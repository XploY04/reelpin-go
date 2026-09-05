package reels

import (
	"errors"
	"testing"
	"time"
)

var now = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

func ptr[T any](value T) *T { return &value }

func TestRelativeDate(t *testing.T) {
	tests := []struct {
		name  string
		saved *time.Time
		want  string
	}{
		{name: "missing", saved: nil, want: ""},
		{name: "today", saved: ptr(now.Add(-2 * time.Hour)), want: "Today"},
		{name: "in the future", saved: ptr(now.Add(48 * time.Hour)), want: "Today"},
		{name: "yesterday", saved: ptr(now.AddDate(0, 0, -1)), want: "Yesterday"},
		{name: "days", saved: ptr(now.AddDate(0, 0, -3)), want: "3 days ago"},
		{name: "one week", saved: ptr(now.AddDate(0, 0, -7)), want: "1 week ago"},
		{name: "weeks", saved: ptr(now.AddDate(0, 0, -21)), want: "3 weeks ago"},
		{name: "one month", saved: ptr(now.AddDate(0, 0, -30)), want: "1 month ago"},
		{name: "months", saved: ptr(now.AddDate(0, 0, -90)), want: "3 months ago"},
		{name: "one year", saved: ptr(now.AddDate(0, 0, -400)), want: "1 year ago"},
		{name: "years", saved: ptr(now.AddDate(0, 0, -800)), want: "2 years ago"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RelativeDate(tt.saved, now); got != tt.want {
				t.Errorf("RelativeDate = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLabelize(t *testing.T) {
	tests := []struct{ in, want string }{
		{"food_and_drink", "Food And Drink"},
		{"travel-tips", "Travel Tips"},
		{"  spaced   out ", "Spaced Out"},
		{"", "Other"},
		{"HOWTO", "Howto"},
	}
	for _, tt := range tests {
		if got := Labelize(tt.in); got != tt.want {
			t.Errorf("Labelize(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestShareCardContentType(t *testing.T) {
	tests := []struct {
		stored string
		want   string
	}{
		{"reel", "reel"},
		{"reels", "reel"},
		{"POST", "post"},
		{"carousel", "carousel"},
		{"video", "post"},
		{"", "post"},
	}
	for _, tt := range tests {
		got := ShareCardContentType(ReelRecord{SourceContentType: ptr(tt.stored)})
		if got != tt.want {
			t.Errorf("ShareCardContentType(%q) = %q, want %q", tt.stored, got, tt.want)
		}
	}
}

func TestBuildMappableLocations(t *testing.T) {
	record := ReelRecord{
		ID: "reel-1",
		Locations: []Location{
			{Name: "", City: ptr("Panaji"), Country: ptr("India"), Latitude: ptr(15.5), Longitude: ptr(73.8)},
			{Name: "No coordinates"},
			{Latitude: ptr(1.25), Longitude: ptr(2.5)},
		},
	}

	locations := BuildMappableLocations(record)
	if len(locations) != 2 {
		t.Fatalf("locations = %d, want 2 (the one without coordinates is dropped)", len(locations))
	}
	if locations[0].LocationDisplayLabel != "Panaji, India" {
		t.Errorf("display label = %q, want the address", locations[0].LocationDisplayLabel)
	}
	if locations[0].MarkerID != "reel-1:0" {
		t.Errorf("marker_id = %q", locations[0].MarkerID)
	}
	// The second surviving location keeps its original index.
	if locations[1].MarkerID != "reel-1:2" {
		t.Errorf("marker_id = %q, want reel-1:2", locations[1].MarkerID)
	}
	if locations[1].LocationDisplayLabel != "1.25000, 2.50000" {
		t.Errorf("coordinate label = %q", locations[1].LocationDisplayLabel)
	}
	if got := *locations[1].GoogleMapsURL; got != "https://www.google.com/maps/search/?api=1&query=1.25,2.5" {
		t.Errorf("google_maps_url = %q", got)
	}
}

func TestBuildListResponseDedupes(t *testing.T) {
	records := []ReelRecord{
		{ID: "a", Title: "first"},
		{ID: "a", Title: "duplicate"},
		{ID: "", Title: "no id"},
		{ID: "b", Title: "second"},
	}

	response := BuildListResponse(records, 5, 10, 0, now)
	if len(response.Reels) != 2 {
		t.Fatalf("reels = %d, want 2", len(response.Reels))
	}
	if response.Reels[0].Title != "first" {
		t.Errorf("kept the wrong duplicate: %q", response.Reels[0].Title)
	}
	// next_offset counts the rows the reader returned, not the deduped ones.
	if response.NextOffset == nil || *response.NextOffset != 4 {
		t.Errorf("next_offset = %v, want 4", response.NextOffset)
	}
}

func TestParsePlatformFilter(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    []string
		wantErr bool
	}{
		{name: "blank", in: "  "},
		{name: "aliases fold", in: "twitter, x.com, yt", want: []string{"x", "youtube"}},
		{name: "other is allowed", in: "other", want: []string{"other"}},
		{name: "unknown", in: "myspace", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParsePlatformFilter(tt.in)
			if tt.wantErr {
				var invalid *InvalidPlatformError
				if !errors.As(err, &invalid) {
					t.Fatalf("err = %v, want an InvalidPlatformError", err)
				}
				if len(invalid.Allowed) == 0 {
					t.Error("the error must carry the allowed values")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("platforms = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("platforms = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestRecordPlatformBuckets(t *testing.T) {
	tests := []struct {
		stored *string
		want   string
	}{
		{nil, "other"},
		{ptr(""), "other"},
		{ptr("twitter"), "x"},
		{ptr("Instagram"), "instagram"},
		{ptr("myspace"), "other"},
	}
	for _, tt := range tests {
		if got := RecordPlatform(tt.stored); got != tt.want {
			t.Errorf("RecordPlatform(%v) = %q, want %q", tt.stored, got, tt.want)
		}
	}
}
