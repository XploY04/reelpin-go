//go:build integration

package enqueue

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/XploY04/reelpin-go/internal/migrations"
	"github.com/XploY04/reelpin-go/internal/queue"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
	"github.com/jackc/pgx/v5/pgxpool"
)

// legacySchema is the slice of the existing database this service writes to.
const legacySchema = `
CREATE TABLE public.reels (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id TEXT NOT NULL,
    url TEXT NOT NULL,
    title TEXT NOT NULL DEFAULT 'Untitled',
    created_at TIMESTAMPTZ DEFAULT now()
);
CREATE TABLE public.processing_jobs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id TEXT NOT NULL,
    environment TEXT NOT NULL DEFAULT 'test',
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
CREATE TABLE public.collections (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id TEXT NOT NULL,
    name TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE public.collection_members (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    collection_id UUID NOT NULL REFERENCES public.collections(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL,
    role TEXT NOT NULL DEFAULT 'viewer',
    UNIQUE (collection_id, user_id)
);
`

const (
	userA = "11111111-1111-4111-8111-111111111111"
	userB = "22222222-2222-4222-8222-222222222222"
)

func testService(t *testing.T) (*Service, *pgxpool.Pool) {
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

	name := "reelpin_enqueue_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
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

	if _, err := pool.Exec(ctx, legacySchema); err != nil {
		t.Fatalf("creating legacy tables: %v", err)
	}
	if _, err := migrations.Up(ctx, parsed.String()); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	return New(pool, &sourceidentity.Resolver{}, DefaultLimits, testEnvironment), pool
}

const sharedReel = "https://www.instagram.com/reel/SHARED1/"

func TestTwoUsersSharingOneReelProduceOneRun(t *testing.T) {
	service, pool := testService(t)
	ctx := context.Background()

	first, err := service.Enqueue(ctx, Request{UserID: userA, URL: sharedReel})
	if err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	// The same post, shared with different tracking parameters.
	second, err := service.Enqueue(ctx, Request{UserID: userB, URL: sharedReel + "?igsh=abc"})
	if err != nil {
		t.Fatalf("second enqueue: %v", err)
	}

	if first.Job.ID == second.Job.ID {
		t.Fatal("two users were given the same private job id")
	}

	var contents, runs int
	pool.QueryRow(ctx, `SELECT count(*) FROM reelpin.contents`).Scan(&contents)
	pool.QueryRow(ctx, `SELECT count(*) FROM reelpin.processing_runs`).Scan(&runs)
	if contents != 1 || runs != 1 {
		t.Fatalf("contents=%d runs=%d, want one of each: the download is shared", contents, runs)
	}

	var firstRun, secondRun string
	pool.QueryRow(ctx, `SELECT processing_run_id::text FROM public.processing_jobs WHERE id = $1`, first.Job.ID).Scan(&firstRun)
	pool.QueryRow(ctx, `SELECT processing_run_id::text FROM public.processing_jobs WHERE id = $1`, second.Job.ID).Scan(&secondRun)
	if firstRun != secondRun {
		t.Fatalf("jobs point at different runs: %s and %s", firstRun, secondRun)
	}

	// One event per private job: each user's save is still their own work.
	var events int
	pool.QueryRow(ctx, `SELECT count(*) FROM reelpin.outbox_events`).Scan(&events)
	if events != 2 {
		t.Fatalf("outbox events = %d, want one per private job", events)
	}
}

func TestConcurrentSharesOfOneReel(t *testing.T) {
	service, pool := testService(t)
	ctx := context.Background()

	const users = 8
	var wait sync.WaitGroup
	errs := make(chan error, users)
	start := make(chan struct{})

	for i := 0; i < users; i++ {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			<-start
			_, err := service.Enqueue(ctx, Request{
				UserID: userA[:len(userA)-1] + string(rune('0'+i)),
				URL:    sharedReel,
			})
			errs <- err
		}(i)
	}
	close(start)
	wait.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent enqueue: %v", err)
		}
	}

	var contents, runs, jobs int
	pool.QueryRow(ctx, `SELECT count(*) FROM reelpin.contents`).Scan(&contents)
	pool.QueryRow(ctx, `SELECT count(*) FROM reelpin.processing_runs`).Scan(&runs)
	pool.QueryRow(ctx, `SELECT count(*) FROM public.processing_jobs`).Scan(&jobs)

	if contents != 1 || runs != 1 {
		t.Fatalf("contents=%d runs=%d under concurrency, want one of each", contents, runs)
	}
	if jobs != users {
		t.Fatalf("jobs = %d, want one private job per user", jobs)
	}
}

func TestProcessedContentTakesThePersonalizeQueue(t *testing.T) {
	service, pool := testService(t)
	ctx := context.Background()

	if _, err := service.Enqueue(ctx, Request{UserID: userA, URL: sharedReel}); err != nil {
		t.Fatalf("first enqueue: %v", err)
	}

	// The pipeline finished: the content now has a version, and the run is done.
	if _, err := pool.Exec(ctx, `
		WITH version AS (
			INSERT INTO reelpin.content_versions (content_id, processor_version, extraction_schema_version)
			SELECT id, $1, 'v1' FROM reelpin.contents
			RETURNING id, content_id
		)
		UPDATE reelpin.contents c SET current_content_version_id = version.id
		FROM version WHERE c.id = version.content_id`, ProcessorVersion); err != nil {
		t.Fatalf("finishing the content: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE reelpin.processing_runs SET status = 'completed'`); err != nil {
		t.Fatalf("completing the run: %v", err)
	}

	if _, err := service.Enqueue(ctx, Request{UserID: userB, URL: sharedReel}); err != nil {
		t.Fatalf("second enqueue: %v", err)
	}

	var routingKey, eventType string
	if err := pool.QueryRow(ctx, `
		SELECT routing_key, event_type FROM reelpin.outbox_events
		ORDER BY created_at DESC LIMIT 1`).Scan(&routingKey, &eventType); err != nil {
		t.Fatalf("reading the event: %v", err)
	}
	if routingKey != queue.QueuePersonalize {
		t.Errorf("routing key = %q, want the personalize queue: nothing needs downloading", routingKey)
	}
	if eventType != "content.personalize" {
		t.Errorf("event type = %q, want content.personalize", eventType)
	}
}

func TestResharingReusesTheJobAndMergesCollections(t *testing.T) {
	service, pool := testService(t)
	ctx := context.Background()

	firstCollection := seedCollection(t, pool, userA, "Trips")
	secondCollection := seedCollection(t, pool, userA, "Food")

	first, err := service.Enqueue(ctx, Request{UserID: userA, URL: sharedReel, CollectionIDs: []string{firstCollection}})
	if err != nil {
		t.Fatalf("first enqueue: %v", err)
	}

	second, err := service.Enqueue(ctx, Request{UserID: userA, URL: sharedReel, CollectionIDs: []string{secondCollection}})
	if err != nil {
		t.Fatalf("second enqueue: %v", err)
	}
	if !second.Reused || second.Job.ID != first.Job.ID {
		t.Fatalf("re-sharing created a second job: %+v", second)
	}

	var raw []byte
	if err := pool.QueryRow(ctx,
		`SELECT collection_ids FROM public.processing_jobs WHERE id = $1`, first.Job.ID,
	).Scan(&raw); err != nil {
		t.Fatalf("reading collection ids: %v", err)
	}
	var merged []string
	if err := json.Unmarshal(raw, &merged); err != nil {
		t.Fatalf("decoding collection ids: %v", err)
	}
	if len(merged) != 2 {
		t.Fatalf("collection ids = %v, want the union of both shares", merged)
	}

	var runs int
	pool.QueryRow(ctx, `SELECT count(*) FROM reelpin.processing_runs`).Scan(&runs)
	if runs != 1 {
		t.Errorf("runs = %d, want re-sharing to create no work", runs)
	}
}

func TestOnlyEditableCollectionsSurvive(t *testing.T) {
	service, pool := testService(t)
	ctx := context.Background()

	owned := seedCollection(t, pool, userA, "Owned")
	editable := seedCollection(t, pool, userB, "Shared with me")
	seedMember(t, pool, editable, userA, "editor")
	viewOnly := seedCollection(t, pool, userB, "Read only")
	seedMember(t, pool, viewOnly, userA, "viewer")
	foreign := seedCollection(t, pool, userB, "Not mine")

	result, err := service.Enqueue(ctx, Request{
		UserID: userA,
		URL:    sharedReel,
		// A deleted target and a malformed one must not cost the user the save.
		CollectionIDs: []string{owned, editable, viewOnly, foreign,
			"99999999-9999-4999-8999-999999999999", "not-a-uuid"},
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	var raw []byte
	pool.QueryRow(ctx, `SELECT collection_ids FROM public.processing_jobs WHERE id = $1`, result.Job.ID).Scan(&raw)
	var filed []string
	json.Unmarshal(raw, &filed)

	if len(filed) != 2 {
		t.Fatalf("filed into %v, want only the owned and editable collections", filed)
	}
	if filed[0] != owned || filed[1] != editable {
		t.Errorf("filed into %v, and in the caller's order", filed)
	}
}

func TestSubmissionLimits(t *testing.T) {
	service, pool := testService(t)
	ctx := context.Background()

	for i := 0; i < DefaultLimits.ActiveJobs; i++ {
		if _, err := pool.Exec(ctx, `
			INSERT INTO public.processing_jobs (user_id, url, status) VALUES ($1, $2, 'queued')`,
			userA, "https://example.com/"+string(rune('a'+i)),
		); err != nil {
			t.Fatalf("seeding jobs: %v", err)
		}
	}

	_, err := service.Enqueue(ctx, Request{UserID: userA, URL: sharedReel})
	var limit *LimitError
	if !errors.As(err, &limit) || limit.Code != "too_many_active_jobs" {
		t.Fatalf("err = %v, want too_many_active_jobs", err)
	}

	// Finishing them frees the user, and the hourly limit takes over.
	if _, err := pool.Exec(ctx, `UPDATE public.processing_jobs SET status='completed', result_reel_id=gen_random_uuid()`); err != nil {
		t.Fatalf("completing jobs: %v", err)
	}
	for i := DefaultLimits.ActiveJobs; i < DefaultLimits.SubmissionsPerHour; i++ {
		if _, err := pool.Exec(ctx, `
			INSERT INTO public.processing_jobs (user_id, url, status, result_reel_id)
			VALUES ($1, $2, 'completed', gen_random_uuid())`,
			userA, "https://example.com/old"+string(rune('a'+i)),
		); err != nil {
			t.Fatalf("seeding history: %v", err)
		}
	}

	_, err = service.Enqueue(ctx, Request{UserID: userA, URL: sharedReel})
	if !errors.As(err, &limit) || limit.Code != "submission_rate_limited" {
		t.Fatalf("err = %v, want submission_rate_limited", err)
	}
}

func TestUnsupportedShareIsRejectedWithoutWriting(t *testing.T) {
	service, pool := testService(t)
	ctx := context.Background()

	if _, err := service.Enqueue(ctx, Request{UserID: userA, RawPayloadText: "no link here"}); !errors.Is(err, ErrNoURL) {
		t.Fatalf("err = %v, want ErrNoURL", err)
	}

	for _, table := range []string{"reelpin.contents", "reelpin.processing_runs", "reelpin.outbox_events", "public.processing_jobs"} {
		var count int
		pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&count)
		if count != 0 {
			t.Errorf("%s has %d rows after a rejected share", table, count)
		}
	}
}

func TestRawPayloadIsUsedWhenNoURLIsGiven(t *testing.T) {
	service, _ := testService(t)

	result, err := service.Enqueue(context.Background(), Request{
		UserID:         userA,
		RawPayloadText: "check this out " + sharedReel + " amazing",
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if result.Job.NormalizedURL == nil || *result.Job.NormalizedURL != sharedReel {
		t.Fatalf("normalized url = %v, want the link from the payload", result.Job.NormalizedURL)
	}
}

func seedCollection(t *testing.T, pool *pgxpool.Pool, ownerID, name string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO public.collections (owner_id, name) VALUES ($1, $2) RETURNING id::text`,
		ownerID, name,
	).Scan(&id); err != nil {
		t.Fatalf("seeding a collection: %v", err)
	}
	return id
}

func seedMember(t *testing.T, pool *pgxpool.Pool, collectionID, userID, role string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO public.collection_members (collection_id, user_id, role) VALUES ($1, $2, $3)`,
		collectionID, userID, role,
	); err != nil {
		t.Fatalf("seeding a member: %v", err)
	}
}

func TestTheOtherEnvironmentsRunIsNotReused(t *testing.T) {
	service, pool := testService(t)
	ctx := context.Background()

	first, err := service.Enqueue(ctx, Request{UserID: userA, URL: sharedReel})
	if err != nil {
		t.Fatalf("first share: %v", err)
	}

	// The same link, shared into the other deployment. Dev and production share
	// this table, so without an environment the second share would attach to
	// the first run and its result would land in the wrong environment.
	other := New(pool, &sourceidentity.Resolver{}, DefaultLimits, "production")
	second, err := other.Enqueue(ctx, Request{UserID: userB, URL: sharedReel})
	if err != nil {
		t.Fatalf("second share: %v", err)
	}

	if second.Job.ID == first.Job.ID {
		t.Fatal("the other environment reused this environment's job")
	}

	var runs int
	if err := pool.QueryRow(ctx,
		`SELECT count(DISTINCT environment) FROM reelpin.processing_runs`).Scan(&runs); err != nil {
		t.Fatalf("counting runs: %v", err)
	}
	if runs != 2 {
		t.Fatalf("distinct environments with runs = %d, want one each", runs)
	}

	// Within one environment the dedup still holds: this is the behaviour the
	// scoping must not break.
	again, err := service.Enqueue(ctx, Request{UserID: userB, URL: sharedReel})
	if err != nil {
		t.Fatalf("third share: %v", err)
	}
	var sameRun bool
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT processing_run_id FROM public.processing_jobs WHERE id = $1)
		     = (SELECT processing_run_id FROM public.processing_jobs WHERE id = $2)`,
		again.Job.ID, first.Job.ID).Scan(&sameRun); err != nil {
		t.Fatalf("comparing runs: %v", err)
	}
	if !sameRun {
		t.Error("two users in the same environment did not share one run")
	}
}
