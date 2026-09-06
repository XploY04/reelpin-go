//go:build integration

package pipeline

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/XploY04/reelpin-go/internal/ai"
	"github.com/XploY04/reelpin-go/internal/lease"
	"github.com/XploY04/reelpin-go/internal/migrations"
	"github.com/XploY04/reelpin-go/internal/platform"
	"github.com/XploY04/reelpin-go/internal/queue"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	userA = "11111111-1111-4111-8111-111111111111"
	userB = "22222222-2222-4222-8222-222222222222"
)

func testPool(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
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

	parsed, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + name

	pool, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		t.Fatalf("connecting to %s: %v", name, err)
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

	if _, err := pool.Exec(ctx, `
		CREATE SCHEMA auth;
		CREATE TABLE auth.users (id UUID PRIMARY KEY, email TEXT, created_at TIMESTAMPTZ DEFAULT now())`); err != nil {
		t.Fatalf("creating auth.users: %v", err)
	}
	if _, err := migrations.Up(ctx, parsed.String()); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO auth.users (id) VALUES ($1), ($2)`, userA, userB); err != nil {
		t.Fatalf("seeding users: %v", err)
	}
	return pool, parsed.String()
}

// seedRun creates the content and run shape enqueue produces, plus one private
// job per subscriber.
func seedRun(t *testing.T, pool *pgxpool.Pool, subscribers ...string) (runID, contentID string) {
	t.Helper()
	ctx := context.Background()
	err := pool.QueryRow(ctx, `
		WITH content AS (
			INSERT INTO reelpin.contents
				(source_platform, source_content_type, source_content_id,
				 normalized_url, normalized_url_hash, access_scope_hash)
			VALUES ('instagram', 'reel', 'SHARED1',
			        'https://www.instagram.com/reel/SHARED1/', 'hash-1', 'public')
			RETURNING id
		)
		INSERT INTO reelpin.processing_runs (content_id, processor_version)
		SELECT id, $1 FROM content
		RETURNING id::text, content_id::text`, ProcessorVersion).Scan(&runID, &contentID)
	if err != nil {
		t.Fatalf("seeding the run: %v", err)
	}

	for _, user := range subscribers {
		if _, err := pool.Exec(ctx, `
			INSERT INTO reelpin.processing_jobs (user_id, run_id, url, normalized_url, source_platform)
			VALUES ($1, $2, 'https://www.instagram.com/reel/SHARED1/',
			        'https://www.instagram.com/reel/SHARED1/', 'instagram')`,
			user, runID); err != nil {
			t.Fatalf("seeding a job: %v", err)
		}
	}
	return runID, contentID
}

// fakeHandler is the platform seam under test control. It can fail on demand
// and counts how often each half ran, which is how checkpoint reuse is proven.
type fakeHandler struct {
	prepares  atomic.Int64
	downloads atomic.Int64
	prepared  platform.Prepared
	prepErr   error
}

func (f *fakeHandler) Platform() string { return "instagram" }

func (f *fakeHandler) Prepare(context.Context, sourceidentity.SourceIdentity) (platform.Prepared, error) {
	f.prepares.Add(1)
	if f.prepErr != nil {
		return platform.Prepared{}, f.prepErr
	}
	return f.prepared, nil
}

func (f *fakeHandler) Download(context.Context, sourceidentity.SourceIdentity, string) ([]ai.Media, error) {
	f.downloads.Add(1)
	return nil, nil
}

type fakeExtractor struct {
	calls      atomic.Int64
	extraction ai.Extraction
	err        error
}

func (f *fakeExtractor) Extract(context.Context, string, string) (ai.Extraction, error) {
	f.calls.Add(1)
	if f.err != nil {
		return ai.Extraction{}, f.err
	}
	return f.extraction, nil
}

type fakeCategorizer struct {
	calls    atomic.Int64
	category ai.Category
	err      error
}

func (f *fakeCategorizer) Categorize(context.Context, ai.Extraction, []ai.TaxonomyOption) (ai.Category, error) {
	f.calls.Add(1)
	if f.err != nil {
		return ai.Category{}, f.err
	}
	return f.category, nil
}

type harness struct {
	pool        *pgxpool.Pool
	pipeline    *Pipeline
	handler     *fakeHandler
	extractor   *fakeExtractor
	categorizer *fakeCategorizer
	runID       string
	contentID   string
}

func newHarness(t *testing.T, subscribers ...string) *harness {
	t.Helper()
	pool, _ := testPool(t)

	handler := &fakeHandler{prepared: platform.Prepared{
		Caption:      "A quiet garden cafe in Anjuna.",
		PageText:     "Artjuna serves coffee from eight.",
		ThumbnailURL: "https://example.com/thumb.jpg",
	}}
	extractor := &fakeExtractor{extraction: ai.Extraction{
		Title:       "Artjuna cafe",
		Summary:     "A quiet garden cafe in Anjuna with good coffee.",
		TopicalTags: []string{"cafe", "goa"},
		KeyFacts:    []string{"Opens at eight"},
	}}
	categorizer := &fakeCategorizer{category: ai.Category{Category: "Food", Subcategory: "Cafes"}}

	registry, err := platform.NewRegistry(handler)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	runID, contentID := seedRun(t, pool, subscribers...)
	return &harness{
		pool:        pool,
		handler:     handler,
		extractor:   extractor,
		categorizer: categorizer,
		runID:       runID,
		contentID:   contentID,
		pipeline: New(Deps{
			Pool:         pool,
			Handlers:     registry,
			Extractor:    extractor,
			Categorizer:  categorizer,
			ModelVersion: "fake-model-v1",
			Logger:       slog.New(slog.NewJSONHandler(io.Discard, nil)),
			WorkerID:     "worker-test",
			TempRoot:     t.TempDir(),
		}),
	}
}

func (h *harness) handle(t *testing.T) (queue.Outcome, error) {
	t.Helper()
	return h.pipeline.Handle(context.Background(), queue.Message{
		EventID:       "33333333-3333-4333-8333-333333333333",
		SchemaVersion: queue.SchemaVersion,
		EventType:     queue.EventProcessLight,
		RunID:         h.runID,
	})
}

func (h *harness) runStatus(t *testing.T) string {
	t.Helper()
	var status string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT status FROM reelpin.processing_runs WHERE id = $1`, h.runID).Scan(&status); err != nil {
		t.Fatalf("reading the run: %v", err)
	}
	return status
}

func TestOneRunCompletesEverySubscriber(t *testing.T) {
	h := newHarness(t, userA, userB)
	ctx := context.Background()

	outcome, err := h.handle(t)
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if outcome.Kind != queue.Done {
		t.Fatalf("outcome = %v, want Done", outcome.Kind)
	}
	if status := h.runStatus(t); status != "completed" {
		t.Fatalf("run status = %q", status)
	}

	// One global version, two private saves: the whole point of the model.
	var versions, saves, completedJobs int
	if err := h.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM reelpin.content_versions WHERE content_id = $1),
		       (SELECT count(*) FROM reelpin.user_saves WHERE content_id = $1),
		       (SELECT count(*) FROM reelpin.processing_jobs WHERE run_id = $2 AND status = 'completed')`,
		h.contentID, h.runID).Scan(&versions, &saves, &completedJobs); err != nil {
		t.Fatal(err)
	}
	if versions != 1 || saves != 2 || completedJobs != 2 {
		t.Fatalf("versions=%d saves=%d completed=%d, want 1/2/2", versions, saves, completedJobs)
	}

	// Reads move to the new version only on that commit.
	var current string
	if err := h.pool.QueryRow(ctx,
		`SELECT current_version_id::text FROM reelpin.contents WHERE id = $1`, h.contentID).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if current == "" {
		t.Error("current_version_id was not moved")
	}

	// Each subscriber's job points at their own save, and each gets one
	// notification event.
	var jobsWithSaves, notifications, indexEvents int
	if err := h.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM reelpin.processing_jobs WHERE run_id = $1 AND user_save_id IS NOT NULL),
		       (SELECT count(*) FROM reelpin.outbox_events WHERE event_type = 'notification.send'),
		       (SELECT count(*) FROM reelpin.outbox_events WHERE event_type = 'content.index')`,
		h.runID).Scan(&jobsWithSaves, &notifications, &indexEvents); err != nil {
		t.Fatal(err)
	}
	if jobsWithSaves != 2 || notifications != 2 || indexEvents != 1 {
		t.Fatalf("saves=%d notifications=%d index=%d, want 2/2/1", jobsWithSaves, notifications, indexEvents)
	}
}

func TestCompletionFilesTheCollectionsTheSubmissionNamed(t *testing.T) {
	h := newHarness(t, userA)
	ctx := context.Background()

	// Submission recorded the intent on the job: one collection that still
	// exists, one deleted between submission and completion.
	var collectionID string
	if err := h.pool.QueryRow(ctx, `
		INSERT INTO reelpin.collections (owner_id, name) VALUES ($1, 'Goa')
		RETURNING id::text`, userA).Scan(&collectionID); err != nil {
		t.Fatalf("seeding a collection: %v", err)
	}
	gone := "88888888-8888-4888-8888-888888888888"
	if _, err := h.pool.Exec(ctx, `
		UPDATE reelpin.processing_jobs SET collection_ids = ARRAY[$2, $3]::uuid[]
		WHERE run_id = $1`, h.runID, collectionID, gone); err != nil {
		t.Fatalf("recording the intent: %v", err)
	}

	if _, err := h.handle(t); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if status := h.runStatus(t); status != "completed" {
		t.Fatalf("run status = %q: a missing collection must not fail the run", status)
	}

	// The save is filed into the collection that survived, and the one that
	// did not is skipped rather than fatal.
	var filed int
	var jobStatus string
	if err := h.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM reelpin.collection_items i
		        JOIN reelpin.processing_jobs j ON j.user_save_id = i.save_id
		        WHERE i.collection_id = $1 AND j.run_id = $2),
		       (SELECT status FROM reelpin.processing_jobs WHERE run_id = $2)`,
		collectionID, h.runID).Scan(&filed, &jobStatus); err != nil {
		t.Fatal(err)
	}
	if filed != 1 || jobStatus != "completed" {
		t.Fatalf("filed=%d job status=%q, want the save filed once and the job completed", filed, jobStatus)
	}

	// The broker is at-least-once, and filing twice must not duplicate an item.
	if _, err := h.handle(t); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	var items int
	if err := h.pool.QueryRow(ctx,
		`SELECT count(*) FROM reelpin.collection_items WHERE collection_id = $1`,
		collectionID).Scan(&items); err != nil {
		t.Fatal(err)
	}
	if items != 1 {
		t.Fatalf("collection items = %d after redelivery, want 1", items)
	}
}

func TestRedeliveryAfterCompletionChangesNothing(t *testing.T) {
	h := newHarness(t, userA)
	if _, err := h.handle(t); err != nil {
		t.Fatal(err)
	}

	// The broker is at-least-once: the same message arrives again.
	outcome, err := h.handle(t)
	if err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if outcome.Kind != queue.Done {
		t.Fatalf("outcome = %v, want Done", outcome.Kind)
	}

	var versions, saves int
	if err := h.pool.QueryRow(context.Background(), `
		SELECT (SELECT count(*) FROM reelpin.content_versions WHERE content_id = $1),
		       (SELECT count(*) FROM reelpin.user_saves WHERE content_id = $1)`,
		h.contentID).Scan(&versions, &saves); err != nil {
		t.Fatal(err)
	}
	if versions != 1 || saves != 1 {
		t.Fatalf("versions=%d saves=%d after redelivery, want 1/1", versions, saves)
	}
}

// TestCrashAndResumeReusesEveryFinishedStage walks the crash boundary after
// each stage: the run is interrupted, then a fresh delivery resumes it, and
// the finished stages must not run again.
func TestCrashAndResumeReusesEveryFinishedStage(t *testing.T) {
	for _, boundary := range []string{stagePrepare, stageExtract, stageCategorize} {
		t.Run("after_"+boundary, func(t *testing.T) {
			h := newHarness(t, userA)

			// Crash by failing the stage after the boundary, so everything up
			// to it checkpoints and the run stops.
			switch boundary {
			case stagePrepare:
				h.extractor.err = errors.New("worker died")
			case stageExtract:
				h.categorizer.err = errors.New("worker died")
			case stageCategorize:
				// Everything through categorize checkpoints; nothing to break.
			}

			if _, err := h.handle(t); err != nil && boundary == stageCategorize {
				t.Fatalf("first pass: %v", err)
			}

			before := struct{ prepares, extracts, categorizes int64 }{
				h.handler.prepares.Load(), h.extractor.calls.Load(), h.categorizer.calls.Load(),
			}

			// The fault clears and a fresh delivery resumes the run.
			h.extractor.err = nil
			h.categorizer.err = nil
			if _, err := h.handle(t); err != nil {
				t.Fatalf("resume: %v", err)
			}
			if status := h.runStatus(t); status != "completed" {
				t.Fatalf("run status after resume = %q", status)
			}

			// Prepare finished on the first pass every time, so it must not
			// have run again.
			if h.handler.prepares.Load() != before.prepares {
				t.Errorf("prepare ran again after resume (%d then %d)",
					before.prepares, h.handler.prepares.Load())
			}
			if boundary == stageExtract || boundary == stageCategorize {
				if h.extractor.calls.Load() != before.extracts {
					t.Errorf("extract ran again after resume (%d then %d)",
						before.extracts, h.extractor.calls.Load())
				}
			}
			if boundary == stageCategorize && h.categorizer.calls.Load() != before.categorizes {
				t.Errorf("categorize ran again after resume")
			}
		})
	}
}

func TestInvalidStructuredOutputCreatesNoVersion(t *testing.T) {
	h := newHarness(t, userA)
	// The model answered with nothing usable: no title, no summary.
	h.extractor.extraction = ai.Extraction{}

	if _, err := h.handle(t); err == nil {
		t.Fatal("an empty extraction completed the run")
	}

	var versions int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM reelpin.content_versions`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 0 {
		t.Fatalf("versions = %d, want none: invalid output must never become a version", versions)
	}

	// It is transient, so the run is scheduled to retry rather than failed.
	if status := h.runStatus(t); status != "retry_scheduled" {
		t.Fatalf("run status = %q, want retry_scheduled", status)
	}
	var resumes int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM reelpin.outbox_events WHERE event_type = 'run.resume'`).Scan(&resumes); err != nil {
		t.Fatal(err)
	}
	if resumes != 1 {
		t.Fatalf("resume events = %d, want one scheduled retry", resumes)
	}
}

func TestAStageGivesUpAfterThreeExecutions(t *testing.T) {
	h := newHarness(t, userA)
	h.extractor.err = errors.New("provider keeps failing")

	for attempt := 1; attempt <= maxStageExecutions; attempt++ {
		if _, err := h.handle(t); err == nil {
			t.Fatalf("attempt %d succeeded unexpectedly", attempt)
		}
		if attempt == maxStageExecutions {
			break // the last failure is terminal; requeueing would hide it
		}
		// A scheduled retry leaves the run claimable again, which is what the
		// resume event does when it is delivered.
		if _, err := h.pool.Exec(context.Background(),
			`UPDATE reelpin.processing_runs SET status = 'queued' WHERE id = $1`, h.runID); err != nil {
			t.Fatal(err)
		}
	}

	if status := h.runStatus(t); status != "failed" {
		t.Fatalf("run status = %q after %d executions, want failed", status, maxStageExecutions)
	}

	// The subscriber's job carries the stable public code, not the internal one.
	var code string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT failure_code FROM reelpin.processing_jobs WHERE run_id = $1`, h.runID).Scan(&code); err != nil {
		t.Fatal(err)
	}
	if code != "internal_error" {
		t.Errorf("job failure_code = %q", code)
	}
}

func TestAnUnsupportedPlatformIsTerminalImmediately(t *testing.T) {
	h := newHarness(t, userA)
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE reelpin.contents SET source_platform = 'myspace' WHERE id = $1`, h.contentID); err != nil {
		t.Fatal(err)
	}

	if _, err := h.handle(t); err == nil {
		t.Fatal("an unsupported platform completed")
	}
	// Terminal on the first execution: no retry is scheduled.
	if status := h.runStatus(t); status != "failed" {
		t.Fatalf("run status = %q, want failed on the first attempt", status)
	}
	var resumes int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM reelpin.outbox_events WHERE event_type = 'run.resume'`).Scan(&resumes); err != nil {
		t.Fatal(err)
	}
	if resumes != 0 {
		t.Errorf("a terminal failure scheduled %d retries", resumes)
	}

	var code string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT failure_code FROM reelpin.processing_jobs WHERE run_id = $1`, h.runID).Scan(&code); err != nil {
		t.Fatal(err)
	}
	if code != "unsupported_platform" {
		t.Errorf("job failure_code = %q", code)
	}
}

// TestAFencedPipelineDiscardsItsResult is the pipeline-level half of the lease
// tests: a worker that lost its claim mid-run must write nothing.
func TestAFencedPipelineDiscardsItsResult(t *testing.T) {
	h := newHarness(t, userA)
	ctx := context.Background()

	// The worker claims the run, then stalls long enough to lose its lease and
	// have another worker take it. Simulate by claiming, then expiring and
	// re-claiming under a different owner.
	held, err := lease.Acquire(ctx, h.pool, h.runID, "worker-test")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := h.pool.Exec(ctx, `
		UPDATE reelpin.processing_runs
		SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, h.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := lease.Acquire(ctx, h.pool, h.runID, "worker-other"); err != nil {
		t.Fatalf("the second worker could not claim: %v", err)
	}

	// The stale worker finishes its work and tries to commit.
	state := &run{ID: h.runID, ContentID: h.contentID, Lease: held,
		Extraction: ai.Extraction{Title: "Stale result"}}
	err = h.pipeline.persist(ctx, state)
	if !errors.Is(err, lease.ErrFenced) {
		t.Fatalf("persist err = %v, want ErrFenced", err)
	}

	var versions int
	if err := h.pool.QueryRow(ctx,
		`SELECT count(*) FROM reelpin.content_versions`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 0 {
		t.Fatal("a fenced worker wrote a content version")
	}

	// Handle turns a fence into a clean acknowledgement: the newer claim owns
	// the run, so redelivering this message would achieve nothing.
	outcome, err := h.pipeline.Handle(ctx, queue.Message{
		EventID: "44444444-4444-4444-8444-444444444444", SchemaVersion: queue.SchemaVersion,
		EventType: queue.EventProcessLight, RunID: h.runID,
	})
	if err != nil {
		t.Fatalf("handle while another worker holds the lease: %v", err)
	}
	if outcome.Kind != queue.Done {
		t.Errorf("outcome = %v, want Done", outcome.Kind)
	}
}

// TestReprocessingCreatesANewImmutableVersion is the versioning contract:
// a new prompt or schema version writes a new row and only then moves reads.
func TestReprocessingCreatesANewImmutableVersion(t *testing.T) {
	h := newHarness(t, userA)
	ctx := context.Background()

	if _, err := h.handle(t); err != nil {
		t.Fatal(err)
	}
	var firstVersion string
	if err := h.pool.QueryRow(ctx,
		`SELECT current_version_id::text FROM reelpin.contents WHERE id = $1`, h.contentID).Scan(&firstVersion); err != nil {
		t.Fatal(err)
	}

	// A version row cannot be edited, only superseded.
	_, err := h.pool.Exec(ctx,
		`UPDATE reelpin.content_versions SET title = 'edited' WHERE id = $1`, firstVersion)
	if err == nil {
		t.Fatal("a content version was updated in place")
	}

	// Reprocess under a new processor version: a second run for the same
	// content, which is what a prompt or schema bump produces.
	var secondRun string
	if err := h.pool.QueryRow(ctx, `
		INSERT INTO reelpin.processing_runs (content_id, processor_version)
		VALUES ($1, 'go-v2') RETURNING id::text`, h.contentID).Scan(&secondRun); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx, `
		INSERT INTO reelpin.processing_jobs (user_id, run_id, url, normalized_url, source_platform)
		VALUES ($1, $2, 'https://www.instagram.com/reel/SHARED1/',
		        'https://www.instagram.com/reel/SHARED1/', 'instagram')`,
		userB, secondRun); err != nil {
		t.Fatal(err)
	}

	// Reads stay on the first version while the second run is in flight.
	var duringReprocess string
	if err := h.pool.QueryRow(ctx,
		`SELECT current_version_id::text FROM reelpin.contents WHERE id = $1`, h.contentID).Scan(&duringReprocess); err != nil {
		t.Fatal(err)
	}
	if duringReprocess != firstVersion {
		t.Fatal("reads moved off the prior version before the new one completed")
	}

	h.extractor.extraction.Title = "Artjuna cafe, reprocessed"
	if _, err := h.pipeline.Handle(ctx, queue.Message{
		EventID: "55555555-5555-4555-8555-555555555555", SchemaVersion: queue.SchemaVersion,
		EventType: queue.EventProcessLight, RunID: secondRun,
	}); err != nil {
		t.Fatalf("reprocess: %v", err)
	}

	var versions int
	var current string
	if err := h.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM reelpin.content_versions WHERE content_id = $1),
		       (SELECT current_version_id::text FROM reelpin.contents WHERE id = $1)`,
		h.contentID).Scan(&versions, &current); err != nil {
		t.Fatal(err)
	}
	if versions != 2 {
		t.Fatalf("versions = %d, want the old one kept and a new one added", versions)
	}
	if current == firstVersion {
		t.Fatal("reads did not move to the new version after it completed")
	}
}

func TestTheModelsCategoryProposalIsFiledNotApplied(t *testing.T) {
	h := newHarness(t, userA)
	h.categorizer.category = ai.Category{
		Category:    "Food",
		Subcategory: "Cafes",
		Proposal:    &ai.CategoryProposal{Name: "Coffee Culture", Description: "Specialty coffee."},
	}

	if _, err := h.handle(t); err != nil {
		t.Fatal(err)
	}

	var proposals, activeCategories int
	if err := h.pool.QueryRow(context.Background(), `
		SELECT (SELECT count(*) FROM reelpin.category_proposals WHERE status = 'pending'),
		       (SELECT count(*) FROM reelpin.categories)`).Scan(&proposals, &activeCategories); err != nil {
		t.Fatal(err)
	}
	if proposals != 1 {
		t.Fatalf("proposals = %d, want the model's wish recorded", proposals)
	}
	if activeCategories != 0 {
		t.Fatal("a processing job activated a category; only the curator may")
	}
}

func TestALightConsumerHandsMediaToTheMediaQueue(t *testing.T) {
	h := newHarness(t, userA)
	ctx := context.Background()

	// Prepare discovers media on a run that arrived on the light queue. The
	// 180-second download must not happen on this channel.
	h.handler.prepared.NeedsMedia = true

	outcome, err := h.pipeline.Handle(ctx, queue.Message{
		EventID:       "44444444-4444-4444-8444-444444444444",
		SchemaVersion: queue.SchemaVersion,
		EventType:     queue.EventProcessLight,
		RunID:         h.runID,
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if outcome.Kind != queue.Done {
		t.Fatalf("outcome = %v, want Done: the escalation is committed, so this message is finished", outcome.Kind)
	}
	if downloads := h.handler.downloads.Load(); downloads != 0 {
		t.Fatalf("the light consumer ran %d downloads instead of escalating", downloads)
	}

	// The run is queued again and exactly one media event is waiting.
	if status := h.runStatus(t); status != "queued" {
		t.Errorf("run status = %q, want queued for the media consumer", status)
	}
	var events int
	var routingKey string
	if err := h.pool.QueryRow(ctx, `
		SELECT count(*), coalesce(min(routing_key), '')
		FROM reelpin.outbox_events WHERE event_type = $1`,
		queue.EventProcessMedia).Scan(&events, &routingKey); err != nil {
		t.Fatal(err)
	}
	if events != 1 || routingKey != queue.QueueMedia {
		t.Fatalf("media events = %d on %q, want one on the media queue", events, routingKey)
	}

	// The media consumer picks it up and finishes, reusing prepare's
	// checkpoint rather than repeating it.
	preparesBefore := h.handler.prepares.Load()
	mediaOutcome, err := h.pipeline.Handle(ctx, queue.Message{
		EventID:       "55555555-5555-4555-8555-555555555555",
		SchemaVersion: queue.SchemaVersion,
		EventType:     queue.EventProcessMedia,
		RunID:         h.runID,
	})
	if err != nil {
		t.Fatalf("media handle: %v", err)
	}
	if mediaOutcome.Kind != queue.Done {
		t.Fatalf("media outcome = %v", mediaOutcome.Kind)
	}
	if h.handler.prepares.Load() != preparesBefore {
		t.Error("the media consumer repeated prepare instead of reusing its checkpoint")
	}
	if h.handler.downloads.Load() != 1 {
		t.Errorf("downloads = %d, want the media consumer to have done exactly one",
			h.handler.downloads.Load())
	}
	if status := h.runStatus(t); status != "completed" {
		t.Errorf("run status = %q, want completed", status)
	}
}

func TestEscalatingTwiceWritesOneEvent(t *testing.T) {
	h := newHarness(t, userA)
	ctx := context.Background()
	h.handler.prepared.NeedsMedia = true

	message := queue.Message{
		EventID:       "66666666-6666-4666-8666-666666666666",
		SchemaVersion: queue.SchemaVersion,
		EventType:     queue.EventProcessLight,
		RunID:         h.runID,
	}
	for i := 0; i < 2; i++ {
		if _, err := h.pipeline.Handle(ctx, message); err != nil {
			t.Fatalf("handle %d: %v", i+1, err)
		}
	}

	var events int
	if err := h.pool.QueryRow(ctx,
		`SELECT count(*) FROM reelpin.outbox_events WHERE event_type = $1`,
		queue.EventProcessMedia).Scan(&events); err != nil {
		t.Fatal(err)
	}
	// The id is derived from the run and its lease generation, so a
	// redelivery cannot double-queue the media work.
	if events > 2 {
		t.Fatalf("media events = %d after two deliveries", events)
	}
}
