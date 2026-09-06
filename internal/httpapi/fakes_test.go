package httpapi

import (
	"context"
	"errors"
	"flag"
	"github.com/XploY04/reelpin-go/internal/enqueue"
	"github.com/XploY04/reelpin-go/internal/geo"
	"github.com/XploY04/reelpin-go/internal/mapview"
	"github.com/XploY04/reelpin-go/internal/ratelimit"
	"github.com/XploY04/reelpin-go/internal/sharetoken"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
	"io"
	"log/slog"
	"time"

	"github.com/XploY04/reelpin-go/internal/jobs"
	"github.com/XploY04/reelpin-go/internal/lifecycle"
	"github.com/XploY04/reelpin-go/internal/reels"
	"github.com/XploY04/reelpin-go/internal/search"
	"github.com/google/uuid"
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

func testDeps(pinger Pinger) Deps {
	return Deps{
		DB:            pinger,
		Auth:          fakeAuth{userID: testUserID},
		Reels:         &fakeReels{},
		Jobs:          &fakeJobs{},
		Enqueue:       &fakeSubmitter{},
		ShareTokens:   &fakeShareTokens{},
		Resolver:      &sourceidentity.Resolver{},
		Collections:   newFakeCollections(),
		Notifications: &fakeNotifications{},
		Lifecycle:     &fakeLifecycle{},
		Map:           &fakeMap{},
		Search:        &fakeSearch{},
		Logger:        slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Version:       "test",
		Now:           func() time.Time { return testNow },
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

// update rewrites generated artifacts instead of comparing against them:
// `go test ./internal/httpapi -update`.
var update = flag.Bool("update", false, "rewrite generated contract artifacts")

// allowAllLimiter opens the metered path in tests that are not about limits.
type allowAllLimiter struct{}

func (allowAllLimiter) Allow(context.Context, ratelimit.Policy, string) (ratelimit.Decision, error) {
	return ratelimit.Decision{Allowed: true, Remaining: 1}, nil
}

// fakeSubmitter answers deterministically: the key "conflict" reproduces the
// idempotency conflict, anything else is accepted with a fixed job.
type fakeSubmitter struct {
	lastRequest enqueue.Request
	result      enqueue.Result
	err         error
}

func (f *fakeSubmitter) Submit(_ context.Context, request enqueue.Request) (enqueue.Result, error) {
	f.lastRequest = request
	if request.IdempotencyKey == "conflict" {
		return enqueue.Result{}, enqueue.ErrIdempotencyMismatch
	}
	if f.err != nil {
		return enqueue.Result{}, f.err
	}
	if f.result.Job != nil || f.result.Reel != nil {
		return f.result, nil
	}
	step := "queued"
	created := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	return enqueue.Result{Kind: enqueue.Accepted, Job: &enqueue.Job{
		ID:            "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		Status:        "queued",
		URL:           request.URL,
		CurrentStep:   &step,
		CollectionIDs: []string{},
		CreatedAt:     &created,
		UpdatedAt:     &created,
	}}, nil
}

// fakeShareTokens is deterministic so fixtures stay byte-stable.
type fakeShareTokens struct {
	revoked int
	minted  int
	userID  string
}

func (f *fakeShareTokens) Mint(_ context.Context, userID string) (string, time.Time, error) {
	f.minted++
	f.userID = userID
	return "fixture-share-token", time.Date(2027, 9, 6, 0, 0, 0, 0, time.UTC), nil
}

func (f *fakeShareTokens) Authenticate(_ context.Context, raw string) (string, error) {
	if raw == "device.token" {
		return testUserID, nil
	}
	return "", sharetoken.ErrUnknownToken
}

func (f *fakeShareTokens) RevokeAll(_ context.Context, userID string) (int, error) {
	f.revoked++
	f.userID = userID
	return 2, nil
}

// fakeNotifications records what the handlers asked for. It never keeps a
// token beyond the call, mirroring the rule that a token is a credential.
type fakeNotifications struct {
	err          error
	lastUserID   string
	lastPlatform string
	registered   int
	deleted      int
	opened       int
}

func (f *fakeNotifications) RegisterToken(_ context.Context, userID, _, platform string) error {
	f.lastUserID, f.lastPlatform = userID, platform
	f.registered++
	return f.err
}

func (f *fakeNotifications) DeleteToken(_ context.Context, userID, _ string) error {
	f.lastUserID = userID
	f.deleted++
	return f.err
}

func (f *fakeNotifications) MarkOpened(_ context.Context, userID, _ string) error {
	f.lastUserID = userID
	f.opened++
	return f.err
}

// fakeLifecycle records what deletion was asked for and answers with whatever
// the test set up.
type fakeLifecycle struct {
	deleteErr  error
	report     lifecycle.Report
	lastUserID string
	lastReelID string
	calls      int
}

func (f *fakeLifecycle) DeleteReel(_ context.Context, userID, reelID string) error {
	f.calls++
	f.lastUserID, f.lastReelID = userID, reelID
	return f.deleteErr
}

func (f *fakeLifecycle) DeleteAccount(_ context.Context, userID string) (lifecycle.Report, error) {
	f.calls++
	f.lastUserID = userID
	if f.deleteErr != nil {
		return lifecycle.Report{}, f.deleteErr
	}
	report := f.report
	report.UserID = userID
	return report, nil
}

// fakeMap answers with whatever the test set up and records the bounds it was
// asked for, so a test can prove the viewport reached the service unchanged.
type fakeMap struct {
	pins       []mapview.Pin
	err        error
	lastUserID string
	lastBounds geo.Bounds
	lastCentre geo.Point
	hidden     *bool
}

func (f *fakeMap) Pins(_ context.Context, userID string, bounds geo.Bounds) ([]mapview.Pin, error) {
	f.lastUserID, f.lastBounds = userID, bounds
	return f.pins, f.err
}

func (f *fakeMap) Nearby(_ context.Context, userID string, centre geo.Point, _ float64, _ int) ([]mapview.Pin, error) {
	f.lastUserID, f.lastCentre = userID, centre
	return f.pins, f.err
}

func (f *fakeMap) CreateManualPin(_ context.Context, userID, name string, address *string, point geo.Point) (mapview.Pin, error) {
	f.lastUserID = userID
	if f.err != nil {
		return mapview.Pin{}, f.err
	}
	return mapview.Pin{
		ID: "55555555-5555-4555-8555-555555555555", Kind: "manual",
		Name: name, Address: address, Latitude: point.Latitude, Longitude: point.Longitude,
	}, nil
}

func (f *fakeMap) DeleteManualPin(_ context.Context, userID, _ string) error {
	f.lastUserID = userID
	return f.err
}

func (f *fakeMap) HidePin(_ context.Context, userID, _ string, hidden bool) error {
	f.lastUserID, f.hidden = userID, &hidden
	return f.err
}

// fakeSearch records what a query asked for and answers with whatever the test
// set up.
type fakeSearch struct {
	err         error
	response    search.Response
	lastUserID  string
	lastQuery   string
	lastFilters search.Filters
	lastLimit   int
}

func (f *fakeSearch) Search(_ context.Context, userID, query string, filters search.Filters, limit int) (search.Response, error) {
	f.lastUserID, f.lastQuery, f.lastFilters, f.lastLimit = userID, query, filters, limit
	if f.err != nil {
		return search.Response{}, f.err
	}
	if f.response.Results == nil {
		f.response.Results = []search.Result{}
	}
	return f.response, nil
}
