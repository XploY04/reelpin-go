//go:build integration

package enqueue_test

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/XploY04/reelpin-go/internal/enqueue"
	"github.com/XploY04/reelpin-go/internal/migrations"
	"github.com/XploY04/reelpin-go/internal/postgres"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	userA = "11111111-1111-4111-8111-111111111111"
	userB = "22222222-2222-4222-8222-222222222222"

	publicReel = "https://www.instagram.com/reel/SHARED1/"
)

func testService(t *testing.T) (*enqueue.Service, *pgxpool.Pool) {
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

	return enqueue.New(postgres.NewEnqueue(pool), &sourceidentity.Resolver{}), pool
}

func submit(t *testing.T, service *enqueue.Service, userID, link string) enqueue.Result {
	t.Helper()
	result, err := service.Submit(context.Background(), enqueue.Request{
		UserID:         userID,
		URL:            link,
		IdempotencyKey: uuid.NewString(),
		Endpoint:       "processing-jobs/reels",
	})
	if err != nil {
		t.Fatalf("submit(%s, %s): %v", userID, link, err)
	}
	return result
}

func counts(t *testing.T, pool *pgxpool.Pool) (contents, runs, jobs, saves, events int) {
	t.Helper()
	ctx := context.Background()
	for query, target := range map[string]*int{
		`SELECT count(*) FROM reelpin.contents`:        &contents,
		`SELECT count(*) FROM reelpin.processing_runs`: &runs,
		`SELECT count(*) FROM reelpin.processing_jobs`: &jobs,
		`SELECT count(*) FROM reelpin.user_saves`:      &saves,
		`SELECT count(*) FROM reelpin.outbox_events`:   &events,
	} {
		if err := pool.QueryRow(ctx, query).Scan(target); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	return
}

func TestTwoUsersSharingPublicContentShareOneRun(t *testing.T) {
	service, pool := testService(t)

	first := submit(t, service, userA, publicReel)
	second := submit(t, service, userB, publicReel)

	if first.Kind != enqueue.Accepted || second.Kind != enqueue.Accepted {
		t.Fatalf("kinds = %v, %v; want both accepted", first.Kind, second.Kind)
	}
	if first.Job.ID == second.Job.ID {
		t.Fatal("two users share one private job")
	}

	contents, runs, jobs, _, events := counts(t, pool)
	if contents != 1 || runs != 1 || jobs != 2 || events != 1 {
		t.Fatalf("contents=%d runs=%d jobs=%d events=%d; want one global run and one event for two private jobs",
			contents, runs, jobs, events)
	}
}

func TestConcurrentSubmissionsFromOneUserProduceOneOfEverything(t *testing.T) {
	service, pool := testService(t)

	var wait sync.WaitGroup
	results := make([]enqueue.Result, 8)
	errs := make([]error, 8)
	for i := 0; i < 8; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			results[index], errs[index] = service.Submit(context.Background(), enqueue.Request{
				UserID:         userA,
				URL:            publicReel,
				IdempotencyKey: uuid.NewString(),
				Endpoint:       "processing-jobs/reels",
			})
		}(i)
	}
	wait.Wait()

	jobIDs := map[string]bool{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("submission %d: %v", i, err)
		}
		jobIDs[results[i].Job.ID] = true
	}
	if len(jobIDs) != 1 {
		t.Fatalf("one user produced %d distinct jobs for one link", len(jobIDs))
	}

	contents, runs, jobs, _, events := counts(t, pool)
	if contents != 1 || runs != 1 || jobs != 1 || events != 1 {
		t.Fatalf("contents=%d runs=%d jobs=%d events=%d; want exactly one of each",
			contents, runs, jobs, events)
	}
}

func TestPrivateScopesNeverCrossUsers(t *testing.T) {
	service, pool := testService(t)

	// A generic page resolves to a user-scoped identity: unknown sources start
	// private until a worker proves them public.
	private := "https://example.com/some/private/page"
	submit(t, service, userA, private)
	submit(t, service, userB, private)

	contents, runs, _, _, _ := counts(t, pool)
	if contents != 2 || runs != 2 {
		t.Fatalf("contents=%d runs=%d; the same private URL must be two identities for two users",
			contents, runs)
	}

	// And one user cannot read the other's extracted result: complete A's
	// content with a version, then have B submit again — B still has no reel.
	var contentA string
	if err := pool.QueryRow(context.Background(), `
		SELECT c.id::text FROM reelpin.contents c
		JOIN reelpin.processing_runs r ON r.content_id = c.id
		JOIN reelpin.processing_jobs j ON j.run_id = r.id
		WHERE j.user_id = $1`, userA).Scan(&contentA); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `
		WITH v AS (
			INSERT INTO reelpin.content_versions
				(content_id, processor_version, prompt_version, schema_version, model_version, title)
			VALUES ($1, 'go-v1', 'p1', 's1', 'm1', 'A private page')
			RETURNING id, content_id
		)
		UPDATE reelpin.contents SET current_version_id = v.id FROM v WHERE contents.id = v.content_id`,
		contentA); err != nil {
		t.Fatal(err)
	}

	result := submit(t, service, userB, private)
	if result.Kind == enqueue.AlreadySaved {
		t.Fatal("user B was answered with content extracted under user A's private scope")
	}
}

func TestCompletedPublicContentIsReusedWithoutAProviderRun(t *testing.T) {
	service, pool := testService(t)
	ctx := context.Background()

	submit(t, service, userA, publicReel)

	// The pipeline finishes user A's run: version, save, completed job.
	var contentID, runID string
	if err := pool.QueryRow(ctx,
		`SELECT c.id::text, r.id::text FROM reelpin.contents c
		 JOIN reelpin.processing_runs r ON r.content_id = c.id`).Scan(&contentID, &runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		WITH v AS (
			INSERT INTO reelpin.content_versions
				(content_id, processor_version, prompt_version, schema_version, model_version, title, summary)
			VALUES ($1, 'go-v1', 'p1', 's1', 'm1', 'Shared reel', 'What everyone shared')
			RETURNING id
		)
		UPDATE reelpin.contents SET current_version_id = (SELECT id FROM v) WHERE id = $1`,
		contentID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE reelpin.processing_runs SET status = 'completed' WHERE id = $1`, runID); err != nil {
		t.Fatal(err)
	}

	before, _, _, _, eventsBefore := counts(t, pool)

	// User B submits the same public link: a save and a completed job appear,
	// no new content, no new run, no new event, no provider spend.
	result := submit(t, service, userB, publicReel)
	if result.Kind != enqueue.AlreadySaved {
		t.Fatalf("kind = %v, want the reel straight back", result.Kind)
	}
	if result.Reel.Title != "Shared reel" {
		t.Fatalf("reel title = %q", result.Reel.Title)
	}

	contents, runs, _, saves, events := counts(t, pool)
	if contents != before || runs != 1 || events != eventsBefore {
		t.Fatalf("contents=%d runs=%d events=%d; reuse must add no global work", contents, runs, events)
	}
	if saves != 1 {
		t.Fatalf("saves = %d, want user B's one (user A's run has not persisted saves yet)", saves)
	}
}

func TestAlreadySavedAnswersTheReelAndNothingChanges(t *testing.T) {
	service, pool := testService(t)
	ctx := context.Background()

	submit(t, service, userA, publicReel)
	var contentID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM reelpin.contents`).Scan(&contentID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		WITH v AS (
			INSERT INTO reelpin.content_versions
				(content_id, processor_version, prompt_version, schema_version, model_version, title)
			VALUES ($1, 'go-v1', 'p1', 's1', 'm1', 'Mine already')
			RETURNING id
		)
		UPDATE reelpin.contents SET current_version_id = (SELECT id FROM v) WHERE id = $1`,
		contentID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO reelpin.user_saves (user_id, content_id) VALUES ($1, $2)`,
		userA, contentID); err != nil {
		t.Fatal(err)
	}

	_, _, jobsBefore, savesBefore, eventsBefore := counts(t, pool)
	result := submit(t, service, userA, publicReel)
	if result.Kind != enqueue.AlreadySaved || result.Reel.Title != "Mine already" {
		t.Fatalf("result = %+v", result)
	}
	_, _, jobs, saves, events := counts(t, pool)
	if jobs != jobsBefore || saves != savesBefore || events != eventsBefore {
		t.Fatal("an already-saved submission changed state")
	}
}

func TestTheActiveJobCapRefusesTheThird(t *testing.T) {
	service, _ := testService(t)
	ctx := context.Background()

	submit(t, service, userA, "https://www.instagram.com/reel/FIRST11/")
	submit(t, service, userA, "https://www.instagram.com/reel/SECOND2/")

	_, err := service.Submit(ctx, enqueue.Request{
		UserID:         userA,
		URL:            "https://www.instagram.com/reel/THIRD33/",
		IdempotencyKey: uuid.NewString(),
		Endpoint:       "processing-jobs/reels",
	})
	if !errors.Is(err, enqueue.ErrActiveJobLimit) {
		t.Fatalf("third submission err = %v, want the cap", err)
	}

	// Another user is not affected by A's cap.
	submit(t, service, userB, "https://www.instagram.com/reel/FOURTH4/")
}

func TestIdempotencyReplaysAndConflicts(t *testing.T) {
	service, pool := testService(t)
	ctx := context.Background()
	key := uuid.NewString()

	first, err := service.Submit(ctx, enqueue.Request{
		UserID: userA, URL: publicReel, IdempotencyKey: key, Endpoint: "processing-jobs/reels",
	})
	if err != nil {
		t.Fatal(err)
	}

	// The same attempt retried: the same job back, nothing new created.
	_, _, jobsBefore, _, eventsBefore := counts(t, pool)
	again, err := service.Submit(ctx, enqueue.Request{
		UserID: userA, URL: publicReel, IdempotencyKey: key, Endpoint: "processing-jobs/reels",
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.Job.ID != first.Job.ID {
		t.Fatalf("replay returned job %s, want %s", again.Job.ID, first.Job.ID)
	}
	_, _, jobs, _, events := counts(t, pool)
	if jobs != jobsBefore || events != eventsBefore {
		t.Fatal("a replayed attempt created new state")
	}

	// The same key with a different body is a client bug, answered 409.
	_, err = service.Submit(ctx, enqueue.Request{
		UserID: userA, URL: "https://www.instagram.com/reel/OTHER77/",
		IdempotencyKey: key, Endpoint: "processing-jobs/reels",
	})
	if !errors.Is(err, enqueue.ErrIdempotencyMismatch) {
		t.Fatalf("err = %v, want the idempotency conflict", err)
	}

	// The same key on the other endpoint is a different attempt.
	if _, err := service.Submit(ctx, enqueue.Request{
		UserID: userA, RawPayloadText: "look " + publicReel,
		IdempotencyKey: key, Endpoint: "native-shares/reels",
	}); err != nil {
		t.Fatalf("the same key on another endpoint was rejected: %v", err)
	}
}

func TestRollbackLeavesNoFragments(t *testing.T) {
	service, pool := testService(t)
	ctx := context.Background()

	// A cancelled context aborts the transaction partway.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err := service.Submit(cancelled, enqueue.Request{
		UserID: userA, URL: publicReel,
		IdempotencyKey: uuid.NewString(), Endpoint: "processing-jobs/reels",
	})
	if err == nil {
		t.Fatal("a cancelled submission succeeded")
	}

	contents, runs, jobs, saves, events := counts(t, pool)
	if contents+runs+jobs+saves+events != 0 {
		t.Fatalf("fragments survived a rollback: contents=%d runs=%d jobs=%d saves=%d events=%d",
			contents, runs, jobs, saves, events)
	}
}

func TestNativeShareTextResolvesAndEnqueues(t *testing.T) {
	service, pool := testService(t)
	ctx := context.Background()

	result, err := service.Submit(ctx, enqueue.Request{
		UserID:         userA,
		RawPayloadText: "check this out! " + publicReel + " so good",
		IdempotencyKey: uuid.NewString(),
		Endpoint:       "native-shares/reels",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Kind != enqueue.Accepted {
		t.Fatalf("kind = %v", result.Kind)
	}

	// The identity is the reel's, not the text's: a URL submission of the same
	// reel joins the same content and run.
	direct := submit(t, service, userB, publicReel)
	if direct.Kind != enqueue.Accepted {
		t.Fatal("the direct submission did not join")
	}
	contents, runs, _, _, _ := counts(t, pool)
	if contents != 1 || runs != 1 {
		t.Fatalf("contents=%d runs=%d; shared text and its URL must be one identity", contents, runs)
	}
}
