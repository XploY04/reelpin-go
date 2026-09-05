package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/XploY04/reelpin-go/internal/enqueue"
	"github.com/XploY04/reelpin-go/internal/jobs"
	"github.com/XploY04/reelpin-go/internal/reels"
	"github.com/XploY04/reelpin-go/internal/sharetoken"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
	"github.com/XploY04/reelpin-go/internal/uuid"
)

// testNow is the fixed clock every handler test runs against.
var testNow = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

type fakePinger struct {
	err   error
	calls int
}

func (f *fakePinger) Ping(context.Context) error {
	f.calls++
	return f.err
}

type fakeAuth struct {
	userID string
	err    error
}

func (f fakeAuth) Authenticate(context.Context, string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.userID, nil
}

type fakeReels struct {
	records []reels.ReelRecord
	facets  []reels.FacetRow
	stats   reels.LibraryStats
	byID    map[string]reels.ReelRecord
	err     error

	lastUserID  string
	lastOptions reels.ListOptions
	lastGetID   uuid.UUID
}

func (f *fakeReels) List(_ context.Context, userID string, options reels.ListOptions) ([]reels.ReelRecord, error) {
	f.lastUserID = userID
	f.lastOptions = options
	if f.err != nil {
		return nil, f.err
	}
	if options.Limit > 0 && len(f.records) > options.Limit {
		return f.records[:options.Limit], nil
	}
	return f.records, nil
}

func (f *fakeReels) Get(_ context.Context, userID string, id uuid.UUID) (reels.ReelRecord, error) {
	f.lastUserID = userID
	f.lastGetID = id
	if f.err != nil {
		return reels.ReelRecord{}, f.err
	}
	record, ok := f.byID[id.String()]
	if !ok || record.UserID != userID {
		return reels.ReelRecord{}, reels.ErrNotFound
	}
	return record, nil
}

func (f *fakeReels) Facets(_ context.Context, userID string) ([]reels.FacetRow, error) {
	f.lastUserID = userID
	if f.err != nil {
		return nil, f.err
	}
	return f.facets, nil
}

func (f *fakeReels) Stats(_ context.Context, userID string) (reels.LibraryStats, error) {
	f.lastUserID = userID
	if f.err != nil {
		return reels.LibraryStats{}, f.err
	}
	return f.stats, nil
}

type fakeJobs struct {
	records []jobs.JobRecord
	err     error

	lastUserID     string
	lastActiveOnly bool
	lastLimit      int
}

func (f *fakeJobs) List(_ context.Context, userID string, activeOnly bool, limit int) ([]jobs.JobRecord, error) {
	f.lastUserID, f.lastActiveOnly, f.lastLimit = userID, activeOnly, limit
	if f.err != nil {
		return nil, f.err
	}
	return f.records, nil
}

func (f *fakeJobs) Get(_ context.Context, userID string, id uuid.UUID) (jobs.JobRecord, error) {
	f.lastUserID = userID
	if f.err != nil {
		return jobs.JobRecord{}, f.err
	}
	for _, record := range f.records {
		if record.ID == id.String() && record.UserID == userID {
			return record, nil
		}
	}
	return jobs.JobRecord{}, jobs.ErrNotFound
}

func testDeps(pinger DatabasePinger) Deps {
	return Deps{
		DB:      pinger,
		Auth:    fakeAuth{userID: testUserID},
		Reels:   &fakeReels{},
		Jobs:    &fakeJobs{},
		Share:   &sourceidentity.Resolver{},
		Enqueue: &fakeEnqueuer{},
		// Paid routes fail closed without a limiter, so tests that are not
		// about limiting get a permissive one.
		Limiter:     &fakeLimiter{allow: true},
		ShareTokens: &fakeShareTokens{},
		Collections: newFakeCollections(),
		Logger:      slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Version:     "test",
		Now:         func() time.Time { return testNow },
	}
}

const testUserID = "11111111-1111-4111-8111-111111111111"

var errFake = errors.New("fake failure")

const (
	testReelID  = "22222222-2222-4222-8222-222222222222"
	testJobID   = "33333333-3333-4333-8333-333333333333"
	otherUserID = "44444444-4444-4444-8444-444444444444"
)

func stringPtr(value string) *string     { return &value }
func floatPtr(value float64) *float64    { return &value }
func timePtr(value time.Time) *time.Time { return &value }

// sampleReel is one saved reel with a mappable location, saved two days before
// the fixed test clock.
func sampleReel(id, userID string) reels.ReelRecord {
	return reels.ReelRecord{
		ID:                id,
		UserID:            userID,
		URL:               "https://www.instagram.com/reel/abc/",
		SourcePlatform:    stringPtr("instagram"),
		SourceContentType: stringPtr("reels"),
		Title:             "Best cafes in Goa",
		Summary:           "Three cafes worth the ride.",
		Transcript:        "spoken words",
		Category:          "food_and_drink",
		Subcategory:       "cafes",
		KeyFacts:          []string{"Opens at 8am"},
		Locations: []reels.Location{
			{Name: "Artjuna Cafe", City: stringPtr("Anjuna"), Latitude: floatPtr(15.5834), Longitude: floatPtr(73.7407)},
			{Name: "No coordinates"},
		},
		CreatedAt: timePtr(testNow.AddDate(0, 0, -2)),
	}
}

type fakeEnqueuer struct {
	result enqueue.Result
	err    error
	last   enqueue.Request
	calls  int
}

func (f *fakeEnqueuer) Enqueue(_ context.Context, request enqueue.Request) (enqueue.Result, error) {
	f.calls++
	f.last = request
	if f.err != nil {
		return enqueue.Result{}, f.err
	}
	result := f.result
	if result.Job.ID == "" {
		result.Job = jobs.JobRecord{
			ID:     testJobID,
			UserID: request.UserID,
			URL:    "https://www.instagram.com/reel/C8abc123/",
			Status: jobs.StatusQueued,
		}
	}
	return result, nil
}

type fakeShareTokens struct {
	token   string
	userID  string
	err     error
	revoked int
	lastRaw string
}

func (f *fakeShareTokens) Mint(context.Context, string, string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	if f.token == "" {
		return "minted-token", nil
	}
	return f.token, nil
}

func (f *fakeShareTokens) UserID(_ context.Context, raw string) (string, error) {
	f.lastRaw = raw
	if f.userID == "" {
		return "", sharetoken.ErrUnknownToken
	}
	return f.userID, nil
}

func (f *fakeShareTokens) RevokeAll(context.Context, string) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.revoked, nil
}
