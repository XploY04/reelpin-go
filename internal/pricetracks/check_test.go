package pricetracks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/notify"
)

var (
	errFake = errors.New("fake failure")
	clock   = time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)
)

const userID = "11111111-1111-4111-8111-111111111111"

// fakeStore keeps tracks in memory and applies a check the way the SQL does.
type fakeStore struct {
	mu       sync.Mutex
	tracks   map[string]Record
	checks   map[string]Check
	saves    map[string]string
	dueCalls int
}

func newFakeStore(tracks ...Record) *fakeStore {
	store := &fakeStore{tracks: map[string]Record{}, checks: map[string]Check{}, saves: map[string]string{}}
	for _, track := range tracks {
		store.tracks[track.ID] = track
	}
	return store
}

func (f *fakeStore) List(_ context.Context, userID string, _ int) ([]Record, error) {
	list := []Record{}
	for _, track := range f.tracks {
		if track.UserID == userID {
			list = append(list, track)
		}
	}
	return list, nil
}

func (f *fakeStore) Create(_ context.Context, userID string, track New) (Record, error) {
	if track.SaveID != nil && f.saves[*track.SaveID] != userID {
		return Record{}, ErrUnknownSave
	}
	record := Record{
		ID: fmt.Sprintf("t%d", len(f.tracks)), UserID: userID, SaveID: track.SaveID, URL: track.URL,
		Title: track.Title, TargetPrice: track.TargetPrice, Active: true, CreatedAt: clock,
	}
	f.tracks[record.ID] = record
	return record, nil
}

func (f *fakeStore) Delete(_ context.Context, userID, trackID string) error {
	track, ok := f.tracks[trackID]
	if !ok || track.UserID != userID {
		return ErrNotFound
	}
	delete(f.tracks, trackID)
	return nil
}

func (f *fakeStore) Due(_ context.Context, checkedBefore time.Time, afterID string, limit int) ([]Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dueCalls++
	list := []Record{}
	for _, track := range f.tracks {
		stale := track.LastCheckedAt == nil || !track.LastCheckedAt.After(checkedBefore)
		if track.Active && stale && track.ID > afterID {
			list = append(list, track)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	if len(list) > limit {
		list = list[:limit]
	}
	return list, nil
}

func (f *fakeStore) RecordCheck(_ context.Context, trackID string, check Check) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks[trackID] = check
	track := f.tracks[trackID]
	track.LastCheckedAt, track.LastPrice = &check.CheckedAt, check.LastPrice
	if check.Notified {
		track.NotifiedAt = &check.CheckedAt
	}
	f.tracks[trackID] = track
	return nil
}

type fakeFetcher map[string]Quote

func (f fakeFetcher) FetchPrice(_ context.Context, url string) (Quote, error) {
	quote, ok := f[url]
	if !ok {
		return Quote{}, errFake
	}
	return quote, nil
}

type fakeNotifier struct {
	mu   sync.Mutex
	sent []notify.Notification
}

func (f *fakeNotifier) SendToUser(_ context.Context, notification notify.Notification) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, notification)
	return "id", nil
}

func price(value float64) *float64 { return &value }

func track(id, url string, target float64) Record {
	return Record{ID: id, UserID: userID, URL: url, TargetPrice: target, Active: true}
}

func checker(store *fakeStore, fetcher Fetcher, notifier *fakeNotifier) *Checker {
	return NewChecker(store, fetcher, notifier, slog.New(slog.NewJSONHandler(io.Discard, nil)),
		func() time.Time { return clock }, 3)
}

func TestTheDailyRunNotifiesOnceAtOrBelowTarget(t *testing.T) {
	notifiedBefore := clock.Add(-48 * time.Hour)
	already := track("d", "https://shop/d", 100)
	already.NotifiedAt = &notifiedBefore

	store := newFakeStore(
		track("a", "https://shop/a", 100), // at target
		track("b", "https://shop/b", 100), // above target
		track("c", "https://shop/c", 100), // page says nothing
		already,
	)
	fetcher := fakeFetcher{
		"https://shop/a": {Price: price(100), Currency: "INR"},
		"https://shop/b": {Price: price(150)},
		"https://shop/c": {},
		"https://shop/d": {Price: price(50)},
	}
	notifier := &fakeNotifier{}

	report, err := checker(store, fetcher, notifier).CheckDue(context.Background())
	if err != nil {
		t.Fatalf("CheckDue: %v", err)
	}
	if report.Checked != 4 || report.Notified != 1 {
		t.Fatalf("report = %+v", report)
	}
	if len(notifier.sent) != 1 || notifier.sent[0].EventKey != "price_drop:a" {
		t.Fatalf("sent = %+v, want one push for a", notifier.sent)
	}
	if !store.checks["a"].Notified || store.checks["d"].Notified {
		t.Error("only a should be recorded as notified; d was told already")
	}
	if store.checks["c"].LastPrice != nil || store.tracks["c"].LastCheckedAt == nil {
		t.Error("a page with no price is still recorded as checked")
	}

	// A second run the same day reads nothing: every track is fresh.
	second, err := checker(store, fetcher, notifier).CheckDue(context.Background())
	if err != nil || second.Checked != 0 || len(notifier.sent) != 1 {
		t.Fatalf("second run = %+v, %v, sent %d", second, err, len(notifier.sent))
	}
}

func TestAPageThatCannotBeReadIsRecordedAsChecked(t *testing.T) {
	store := newFakeStore(track("a", "https://shop/unreachable", 100))
	report, err := checker(store, fakeFetcher{}, &fakeNotifier{}).CheckDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Checked != 1 || store.tracks["a"].LastCheckedAt == nil {
		t.Fatalf("report = %+v; an unreadable page must not be retried in a loop", report)
	}
}

func TestTheRunPagesThroughEveryDueTrack(t *testing.T) {
	tracks := []Record{}
	fetcher := fakeFetcher{}
	for i := range checkPage*2 + 5 {
		url := fmt.Sprintf("https://shop/%03d", i)
		tracks = append(tracks, track(fmt.Sprintf("%03d", i), url, 1))
		fetcher[url] = Quote{Price: price(5)}
	}
	store := newFakeStore(tracks...)

	report, err := checker(store, fetcher, &fakeNotifier{}).CheckDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Checked != len(tracks) || report.Priced != len(tracks) {
		t.Fatalf("report = %+v, want all %d", report, len(tracks))
	}
	if store.dueCalls != 3 {
		t.Errorf("%d page reads, want 3", store.dueCalls)
	}
}

func TestAPriceDropReadsLikePython(t *testing.T) {
	save := "22222222-2222-4222-8222-222222222222"
	drop := PriceDrop(Record{ID: "t1", UserID: userID, SaveID: &save, Title: "Shoes", TargetPrice: 1500}, 1299, "₹")
	if drop.Body != "Shoes is now ₹1299 (your target: ₹1500)." {
		t.Errorf("body = %q", drop.Body)
	}
	if drop.Title != "Price drop" || drop.Type != "price_drop" || drop.Target != "reel" || drop.TargetID != save {
		t.Errorf("drop = %+v", drop)
	}
	if drop.Data["price_track_id"] != "t1" || drop.Data["reel_id"] != save {
		t.Errorf("data = %v", drop.Data)
	}

	untitled := PriceDrop(Record{ID: "t2", UserID: userID, TargetPrice: 20, Currency: "$"}, 19.99, "")
	if untitled.Body != "A product you're tracking is now $19.99 (your target: $20)." {
		t.Errorf("body = %q", untitled.Body)
	}
	if untitled.Target != "price_track" || untitled.TargetID != "t2" {
		t.Errorf("untitled = %+v", untitled)
	}
}

func TestCreatingAPriceTrackIsValidated(t *testing.T) {
	tests := []struct {
		name   string
		params CreateParams
		field  string
	}{
		{"blank url", CreateParams{URL: "   ", TargetPrice: price(100)}, "url"},
		{"not http", CreateParams{URL: "ftp://example.com/x", TargetPrice: price(100)}, "url"},
		{"no target", CreateParams{URL: "https://shop/a"}, "target_price"},
		{"zero target", CreateParams{URL: "https://shop/a", TargetPrice: price(0)}, "target_price"},
		{"negative target", CreateParams{URL: "https://shop/a", TargetPrice: price(-5)}, "target_price"},
		{"reel_id not an id", CreateParams{URL: "https://shop/a", TargetPrice: price(5), ReelID: stringPtr("x")}, "reel_id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewService(newFakeStore()).Create(context.Background(), userID, tt.params)
			var invalid *ValidationError
			if !errors.As(err, &invalid) || invalid.Field != tt.field {
				t.Fatalf("err = %v, want a validation error on %s", err, tt.field)
			}
		})
	}
}

func TestCreatingAPriceTrackTrimsAndStores(t *testing.T) {
	store := newFakeStore()
	created, err := NewService(store).Create(context.Background(), userID, CreateParams{
		URL: "  https://shop.example/p/1  ", TargetPrice: price(999), Title: "  Shoes ",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.URL != "https://shop.example/p/1" || created.Title != "Shoes" || !created.Active {
		t.Errorf("created = %+v", created)
	}
	if created.LastPrice != nil || created.LastCheckedAt != nil || created.NotifiedAt != nil || created.ReelID != nil {
		t.Errorf("a new track has never been checked: %+v", created)
	}
}

func stringPtr(value string) *string { return &value }
