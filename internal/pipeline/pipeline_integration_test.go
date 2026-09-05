//go:build integration

package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/XploY04/reelpin-go/internal/ai"
	"github.com/XploY04/reelpin-go/internal/enqueue"
	"github.com/XploY04/reelpin-go/internal/geo"
	"github.com/XploY04/reelpin-go/internal/migrations"
	"github.com/XploY04/reelpin-go/internal/platform"
	"github.com/XploY04/reelpin-go/internal/queue"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
	"github.com/jackc/pgx/v5/pgxpool"
)

const legacySchema = `
CREATE TABLE public.reels (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id TEXT NOT NULL,
    url TEXT NOT NULL,
    normalized_url TEXT,
    source_platform TEXT,
    source_content_type TEXT,
    source_content_id TEXT,
    processing_version TEXT,
    ingestion_method TEXT,
    transcript_source TEXT,
    thumbnail_url TEXT,
    title TEXT NOT NULL DEFAULT 'Untitled',
    summary TEXT DEFAULT '',
    transcript TEXT DEFAULT '',
    category TEXT DEFAULT 'Other',
    subcategory TEXT DEFAULT 'Other',
    secondary_categories JSONB DEFAULT '[]'::jsonb,
    key_facts JSONB DEFAULT '[]'::jsonb,
    locations JSONB DEFAULT '[]'::jsonb,
    people_mentioned JSONB DEFAULT '[]'::jsonb,
    actionable_items JSONB DEFAULT '[]'::jsonb,
    events JSONB NOT NULL DEFAULT '[]'::jsonb,
    parse_status TEXT NOT NULL DEFAULT 'parsed',
    created_at TIMESTAMPTZ DEFAULT now()
);
CREATE TABLE public.processing_jobs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id TEXT NOT NULL,
    url TEXT NOT NULL,
    normalized_url TEXT,
    source_platform TEXT,
    source_content_type TEXT,
    source_content_id TEXT,
    processing_version TEXT,
    ingestion_method TEXT,
    transcript_source TEXT,
    status TEXT NOT NULL DEFAULT 'queued',
    current_step TEXT DEFAULT 'queued',
    progress_percent INTEGER NOT NULL DEFAULT 0,
    failure_code TEXT,
    error_message TEXT,
    result_reel_id UUID,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 3,
    next_retry_at TIMESTAMPTZ,
    claimed_by TEXT,
    step_durations JSONB NOT NULL DEFAULT '{}'::jsonb,
    collection_ids JSONB NOT NULL DEFAULT '[]'::jsonb,
    created_at TIMESTAMPTZ DEFAULT now(),
    updated_at TIMESTAMPTZ DEFAULT now(),
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ
);
CREATE TABLE public.geocode_cache (
    query_key TEXT PRIMARY KEY,
    query_text TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'unknown',
    latitude DOUBLE PRECISION,
    longitude DOUBLE PRECISION,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
`

// fakeHandler stands in for a platform. It writes a file so the pipeline's temp
// handling is exercised for real.
type fakeHandler struct {
	prepared platform.Prepared
	err      error
	calls    atomic.Int64
}

func (f *fakeHandler) Name() string                             { return "fake" }
func (f *fakeHandler) Match(sourceidentity.SourceIdentity) bool { return true }
func (f *fakeHandler) Capabilities(sourceidentity.SourceIdentity) platform.Capabilities {
	return platform.Capabilities{Video: true, Audio: true, Caption: true}
}
func (f *fakeHandler) Normalize(_ context.Context, identity sourceidentity.SourceIdentity) (sourceidentity.SourceIdentity, error) {
	return identity, nil
}
func (f *fakeHandler) Prepare(_ context.Context, _ sourceidentity.SourceIdentity, workDir string) (platform.Prepared, error) {
	f.calls.Add(1)
	if f.err != nil {
		return platform.Prepared{}, f.err
	}
	prepared := f.prepared
	if prepared.AudioPath == "" && prepared.Transcript == "" {
		path := workDir + "/audio.mp3"
		if err := os.WriteFile(path, []byte("audio"), 0o600); err != nil {
			return platform.Prepared{}, err
		}
		prepared.AudioPath = path
	}
	return prepared, nil
}

type fakeTranscriber struct {
	text  string
	err   error
	calls atomic.Int64
}

func (f *fakeTranscriber) Transcribe(context.Context, ai.Media) (string, error) {
	f.calls.Add(1)
	return f.text, f.err
}

type fakeImageReader struct{ text string }

func (f fakeImageReader) ReadText(context.Context, []ai.Media) (string, error) {
	return f.text, nil
}

type fakeExtractor struct {
	extraction ai.Extraction
	err        error
	calls      atomic.Int64
}

func (f *fakeExtractor) Extract(context.Context, string, string) (ai.Extraction, error) {
	f.calls.Add(1)
	return f.extraction, f.err
}

type fakeCategorizer struct {
	category ai.Category
	calls    atomic.Int64
	seen     []string
}

func (f *fakeCategorizer) Categorize(_ context.Context, _ ai.Extraction, existing []string) (ai.Category, error) {
	f.calls.Add(1)
	f.seen = existing
	return f.category, nil
}

type fakeGeocoder struct {
	point geo.Point
	err   error
	calls atomic.Int64
}

func (f *fakeGeocoder) Geocode(context.Context, string) (geo.Point, error) {
	f.calls.Add(1)
	if f.err != nil {
		return geo.Point{}, f.err
	}
	return f.point, nil
}

type harness struct {
	pipeline    *Pipeline
	pool        *pgxpool.Pool
	handler     *fakeHandler
	transcriber *fakeTranscriber
	extractor   *fakeExtractor
	categorizer *fakeCategorizer
	geocoder    *fakeGeocoder
	runID       string
	jobIDs      []string
}

func newHarness(t *testing.T, users ...string) *harness {
	t.Helper()
	if len(users) == 0 {
		users = []string{"11111111-1111-4111-8111-111111111111"}
	}

	adminURL := os.Getenv("TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer admin.Close()

	name := "reelpin_pipeline_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
	if len(name) > 60 {
		name = name[:60]
	}
	for _, statement := range []string{
		`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`,
		`CREATE DATABASE ` + name,
	} {
		if _, err := admin.Exec(ctx, statement); err != nil {
			t.Fatalf("preparing %s: %v", name, err)
		}
	}

	parsed, _ := url.Parse(adminURL)
	parsed.Path = "/" + name
	pool, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		cleanup, err := pgxpool.New(context.Background(), adminURL)
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
	})

	if _, err := pool.Exec(ctx, legacySchema); err != nil {
		t.Fatalf("legacy schema: %v", err)
	}
	if _, err := migrations.Up(ctx, parsed.String()); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	h := &harness{
		pool:        pool,
		handler:     &fakeHandler{prepared: platform.Prepared{Caption: "a caption", ThumbnailURL: "https://cdn.example.com/t.jpg", IngestionMethod: "url_share"}},
		transcriber: &fakeTranscriber{text: "spoken words about a cafe"},
		extractor: &fakeExtractor{extraction: ai.Extraction{
			Title: "Best cafes in Goa", Summary: "Three cafes worth the ride.",
			TopicalTags: []string{"Cafes"}, KeyFacts: []string{"Opens at 8am"},
			Locations: []ai.Location{{Name: "Artjuna Cafe", City: "Anjuna", Country: "India"}},
		}},
		categorizer: &fakeCategorizer{category: ai.Category{Category: "Food", Subcategory: "Cafes"}},
		geocoder:    &fakeGeocoder{point: geo.Point{Latitude: 15.58, Longitude: 73.74}},
	}

	h.pipeline = New(Deps{
		Pool:        pool,
		Handlers:    platform.NewRegistry(h.handler),
		Transcriber: h.transcriber,
		ImageReader: fakeImageReader{text: "image words"},
		Extractor:   h.extractor,
		Categorizer: h.categorizer,
		Geocoder:    h.geocoder,
		Logger:      slog.New(slog.NewJSONHandler(io.Discard, nil)),
		TempRoot:    t.TempDir(),
	})

	// One content, one run, one private job per user: the shape enqueue creates.
	var contentID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO reelpin.contents
			(source_platform, source_content_type, source_content_id, normalized_url, normalized_url_hash)
		VALUES ('instagram','reel','SHARED1','https://www.instagram.com/reel/SHARED1/','hash-1')
		RETURNING id::text`).Scan(&contentID); err != nil {
		t.Fatalf("seeding content: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO reelpin.processing_runs (content_id, processor_version, platform, status)
		VALUES ($1, $2, 'instagram', 'processing')
		RETURNING id::text`, contentID, enqueue.ProcessorVersion).Scan(&h.runID); err != nil {
		t.Fatalf("seeding run: %v", err)
	}
	for _, user := range users {
		var jobID string
		if err := pool.QueryRow(ctx, `
			INSERT INTO public.processing_jobs (user_id, url, normalized_url, processing_run_id, status)
			VALUES ($1, 'https://www.instagram.com/reel/SHARED1/', 'https://www.instagram.com/reel/SHARED1/', $2, 'queued')
			RETURNING id::text`, user, h.runID).Scan(&jobID); err != nil {
			t.Fatalf("seeding job: %v", err)
		}
		h.jobIDs = append(h.jobIDs, jobID)
	}
	return h
}

func (h *harness) message() queue.Message {
	return queue.Message{
		EventID: "33333333-3333-4333-8333-333333333333", RunID: h.runID,
		Platform: "instagram", SchemaVersion: queue.SchemaVersion, Type: "content.process",
	}
}

func TestPipelineSavesOneReelPerSubscriber(t *testing.T) {
	h := newHarness(t, "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222")
	ctx := context.Background()

	if outcome, err := h.pipeline.Handle(ctx, h.message()); err != nil || outcome != queue.Done {
		t.Fatalf("handle = %v (%v)", outcome, err)
	}

	var reels, versions, locations, chunks int
	h.pool.QueryRow(ctx, `SELECT count(*) FROM public.reels`).Scan(&reels)
	h.pool.QueryRow(ctx, `SELECT count(*) FROM reelpin.content_versions`).Scan(&versions)
	h.pool.QueryRow(ctx, `SELECT count(*) FROM reelpin.content_locations`).Scan(&locations)
	h.pool.QueryRow(ctx, `SELECT count(*) FROM reelpin.content_chunks`).Scan(&chunks)

	if reels != 2 {
		t.Errorf("reels = %d, want one per subscriber", reels)
	}
	if versions != 1 {
		t.Errorf("content versions = %d, want the shared half written once", versions)
	}
	if locations != 1 || chunks != 1 {
		t.Errorf("locations=%d chunks=%d", locations, chunks)
	}

	// The expensive stages ran once for both users.
	if h.handler.calls.Load() != 1 || h.transcriber.calls.Load() != 1 || h.extractor.calls.Load() != 1 {
		t.Errorf("prepare=%d transcribe=%d extract=%d, want one each",
			h.handler.calls.Load(), h.transcriber.calls.Load(), h.extractor.calls.Load())
	}
	// Categories are per user, so that call is not shared.
	if h.categorizer.calls.Load() != 2 {
		t.Errorf("categorize ran %d times, want one per user", h.categorizer.calls.Load())
	}

	var status string
	var progress int
	var resultReel *string
	h.pool.QueryRow(ctx, `SELECT status, progress_percent, result_reel_id::text FROM public.processing_jobs WHERE id = $1`,
		h.jobIDs[0]).Scan(&status, &progress, &resultReel)
	if status != "completed" || progress != 100 || resultReel == nil {
		t.Errorf("job status=%s progress=%d reel=%v", status, progress, resultReel)
	}

	var runStatus string
	h.pool.QueryRow(ctx, `SELECT status FROM reelpin.processing_runs`).Scan(&runStatus)
	if runStatus != "completed" {
		t.Errorf("run status = %s, want completed", runStatus)
	}

	var events int
	h.pool.QueryRow(ctx, `SELECT count(*) FROM reelpin.outbox_events`).Scan(&events)
	if events != 4 {
		// two saves, one index, one notification
		t.Errorf("outbox events = %d, want 4", events)
	}

	// The geocoded coordinates reach the user's reel row.
	var raw []byte
	h.pool.QueryRow(ctx, `SELECT locations FROM public.reels LIMIT 1`).Scan(&raw)
	var saved []map[string]any
	json.Unmarshal(raw, &saved)
	if len(saved) != 1 || saved[0]["latitude"] == nil {
		t.Errorf("saved locations = %v", saved)
	}
}

func TestRedeliveryRepeatsNothingExpensive(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, err := h.pipeline.Handle(ctx, h.message()); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	// The broker delivers the same message again after the ack was lost.
	if outcome, err := h.pipeline.Handle(ctx, h.message()); err != nil || outcome != queue.Done {
		t.Fatalf("second delivery = %v (%v)", outcome, err)
	}

	if h.handler.calls.Load() != 1 {
		t.Errorf("prepare ran %d times: a duplicate message re-downloaded", h.handler.calls.Load())
	}
	if h.transcriber.calls.Load() != 1 || h.extractor.calls.Load() != 1 {
		t.Errorf("a duplicate message paid for the model again: transcribe=%d extract=%d",
			h.transcriber.calls.Load(), h.extractor.calls.Load())
	}
	if h.geocoder.calls.Load() != 1 {
		t.Errorf("geocoding ran %d times", h.geocoder.calls.Load())
	}

	var reels, versions, events int
	h.pool.QueryRow(ctx, `SELECT count(*) FROM public.reels`).Scan(&reels)
	h.pool.QueryRow(ctx, `SELECT count(*) FROM reelpin.content_versions`).Scan(&versions)
	h.pool.QueryRow(ctx, `SELECT count(*) FROM reelpin.outbox_events`).Scan(&events)
	if reels != 1 || versions != 1 || events != 3 {
		t.Fatalf("redelivery duplicated data: reels=%d versions=%d events=%d", reels, versions, events)
	}
}

func TestRestartResumesAtEveryStageBoundary(t *testing.T) {
	for _, stopAfter := range Stages[:len(Stages)-1] {
		t.Run(stopAfter, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()

			// Run up to and including one stage, then throw the process away.
			state, err := h.pipeline.load(ctx, h.runID)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			workDir, err := os.MkdirTemp(h.pipeline.deps.TempRoot, "run-partial-")
			if err != nil {
				t.Fatal(err)
			}
			state.WorkDir = workDir

			for _, stage := range Stages {
				if err := h.pipeline.checkpoints.Progress(ctx, state.ID, stage); err != nil {
					t.Fatalf("progress: %v", err)
				}
				if err := h.pipeline.runStage(ctx, stage, state); err != nil {
					t.Fatalf("stage %s: %v", stage, err)
				}
				if stage == stopAfter {
					break
				}
			}
			os.RemoveAll(workDir)

			// A fresh delivery finishes the run.
			if outcome, err := h.pipeline.Handle(ctx, h.message()); err != nil || outcome != queue.Done {
				t.Fatalf("resumed delivery = %v (%v)", outcome, err)
			}

			var reels, versions int
			h.pool.QueryRow(ctx, `SELECT count(*) FROM public.reels`).Scan(&reels)
			h.pool.QueryRow(ctx, `SELECT count(*) FROM reelpin.content_versions`).Scan(&versions)
			if reels != 1 || versions != 1 {
				t.Fatalf("after resuming from %s: reels=%d versions=%d", stopAfter, reels, versions)
			}
		})
	}
}

func TestTransientFailureSchedulesARetry(t *testing.T) {
	h := newHarness(t)
	h.handler.err = errors.New("read timeout after 30s")
	ctx := context.Background()

	outcome, err := h.pipeline.Handle(ctx, h.message())
	if outcome != queue.Retry {
		t.Fatalf("outcome = %v, want Retry (%v)", outcome, err)
	}

	var runStatus, jobStep string
	var failureCode *string
	h.pool.QueryRow(ctx, `SELECT status, failure_code FROM reelpin.processing_runs`).Scan(&runStatus, &failureCode)
	h.pool.QueryRow(ctx, `SELECT current_step FROM public.processing_jobs`).Scan(&jobStep)

	if runStatus != "retry_scheduled" || failureCode == nil || *failureCode != "provider_timeout" {
		t.Errorf("run = %s/%v", runStatus, failureCode)
	}
	if jobStep != "retry_scheduled" {
		t.Errorf("job step = %s, want the user to see it is still working", jobStep)
	}

	var reels int
	h.pool.QueryRow(ctx, `SELECT count(*) FROM public.reels`).Scan(&reels)
	if reels != 0 {
		t.Error("a transient failure left a placeholder that would block a future share")
	}
}

func TestContentTerminalFailureSavesAnUnparsedRecord(t *testing.T) {
	h := newHarness(t)
	h.handler.err = EmptyPostContent(errors.New("nothing in this post"))
	ctx := context.Background()

	outcome, _ := h.pipeline.Handle(ctx, h.message())
	if outcome != queue.Done {
		t.Fatalf("outcome = %v, want Done: retrying will not change the content", outcome)
	}

	var runStatus string
	h.pool.QueryRow(ctx, `SELECT status FROM reelpin.processing_runs`).Scan(&runStatus)
	if runStatus != "failed" {
		t.Errorf("run status = %s, want failed", runStatus)
	}

	var parseStatus, summary string
	if err := h.pool.QueryRow(ctx, `SELECT parse_status, summary FROM public.reels`).Scan(&parseStatus, &summary); err != nil {
		t.Fatalf("the user's share vanished: %v", err)
	}
	if parseStatus != "unparsed" {
		t.Errorf("parse_status = %s, want unparsed", parseStatus)
	}
	if !strings.Contains(summary, "no readable content") {
		t.Errorf("summary = %q, want the user-facing reason", summary)
	}

	var jobStatus string
	var jobFailure *string
	h.pool.QueryRow(ctx, `SELECT status, failure_code FROM public.processing_jobs`).Scan(&jobStatus, &jobFailure)
	if jobStatus != "failed" || jobFailure == nil || *jobFailure != "empty_post_content" {
		t.Errorf("job = %s/%v", jobStatus, jobFailure)
	}
}

func TestProviderExhaustionCoolsDownThePlatform(t *testing.T) {
	h := newHarness(t)
	h.handler.err = RateLimited(errors.New("429 from the provider"))
	ctx := context.Background()

	if outcome, _ := h.pipeline.Handle(ctx, h.message()); outcome != queue.Retry {
		t.Fatalf("outcome = %v, want Retry", outcome)
	}

	var platformName string
	var until time.Time
	if err := h.pool.QueryRow(ctx,
		`SELECT platform, cooldown_until FROM reelpin.provider_cooldowns`,
	).Scan(&platformName, &until); err != nil {
		t.Fatalf("no cooldown was recorded: %v", err)
	}
	if platformName != "instagram" || !until.After(time.Now()) {
		t.Errorf("cooldown = %s until %s", platformName, until)
	}
}

func TestExhaustedAttemptsDeadLetter(t *testing.T) {
	h := newHarness(t)
	h.handler.err = errors.New("something broke inside")
	ctx := context.Background()

	message := h.message()
	message.Attempt = 3 // max_attempts on the seeded run

	if outcome, _ := h.pipeline.Handle(ctx, message); outcome != queue.DeadLetter {
		t.Fatalf("outcome = %v, want DeadLetter", outcome)
	}

	var runStatus string
	h.pool.QueryRow(ctx, `SELECT status FROM reelpin.processing_runs`).Scan(&runStatus)
	if runStatus != "dead_lettered" {
		t.Errorf("run status = %s, want dead_lettered", runStatus)
	}

	// An internal failure is not the content's fault, so no placeholder is
	// written: a later share should get a clean run.
	var reels int
	h.pool.QueryRow(ctx, `SELECT count(*) FROM public.reels`).Scan(&reels)
	if reels != 0 {
		t.Error("a dead-lettered internal failure wrote a placeholder")
	}
}

func TestPostWithNoTextAtAllIsTerminal(t *testing.T) {
	h := newHarness(t)
	h.handler.prepared = platform.Prepared{}
	h.transcriber.text = ""
	ctx := context.Background()

	if outcome, err := h.pipeline.Handle(ctx, h.message()); outcome != queue.Done {
		t.Fatalf("outcome = %v (%v), want Done", outcome, err)
	}

	var failureCode *string
	h.pool.QueryRow(ctx, `SELECT failure_code FROM reelpin.processing_runs`).Scan(&failureCode)
	if failureCode == nil || *failureCode != "empty_post_content" {
		t.Fatalf("failure code = %v, want empty_post_content", failureCode)
	}
}

func TestTheRunDirectoryIsAlwaysRemoved(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, err := h.pipeline.Handle(ctx, h.message()); err != nil {
		t.Fatalf("handle: %v", err)
	}

	entries, err := os.ReadDir(h.pipeline.deps.TempRoot)
	if err != nil {
		t.Fatalf("reading the temp root: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "run-") {
			t.Fatalf("%s survived a successful run", entry.Name())
		}
	}

	// And after a failure.
	h.handler.err = errors.New("boom")
	h.pipeline.Handle(ctx, h.message())
	entries, _ = os.ReadDir(h.pipeline.deps.TempRoot)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "run-") {
			t.Fatalf("%s survived a failed run", entry.Name())
		}
	}
}

func TestCategoriesReuseTheUsersExistingTree(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, err := h.pool.Exec(ctx, `
		INSERT INTO public.reels (user_id, url, title, category, subcategory)
		VALUES ($1, 'https://example.com/old', 'Old', 'Travel', 'Beaches')`,
		"11111111-1111-4111-8111-111111111111"); err != nil {
		t.Fatalf("seeding history: %v", err)
	}

	if _, err := h.pipeline.Handle(ctx, h.message()); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(h.categorizer.seen) == 0 || !strings.Contains(strings.Join(h.categorizer.seen, "|"), "Travel > Beaches") {
		t.Fatalf("the categorizer was given %v, want the user's existing tree", h.categorizer.seen)
	}
}
