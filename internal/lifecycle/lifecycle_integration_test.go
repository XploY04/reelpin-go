//go:build integration

package lifecycle

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	userA = "11111111-1111-4111-8111-111111111111"
	userB = "22222222-2222-4222-8222-222222222222"
)

func quiet() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func testPool(t *testing.T) *pgxpool.Pool {
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

	name := "reelpin_lifecycle_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
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
	return pool
}

// seedContent creates one piece of content with a version, and returns its id.
// scope is the access scope hash: the literal "public" for content that
// deduplicates globally, anything else for a user-scoped identity.
func seedContent(t *testing.T, pool *pgxpool.Pool, sourceID, scope string) string {
	t.Helper()
	ctx := context.Background()

	// Three statements, not one: data-modifying CTEs share a snapshot, so an
	// UPDATE in the same statement cannot see the row an earlier CTE inserted.
	var contentID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO reelpin.contents
			(source_platform, source_content_type, source_content_id,
			 normalized_url, normalized_url_hash, access_scope_hash)
		VALUES ('instagram', 'reel', $1,
		        'https://www.instagram.com/reel/' || $1 || '/', $1, $2)
		RETURNING id::text`, sourceID, scope).Scan(&contentID); err != nil {
		t.Fatalf("seeding content %s: %v", sourceID, err)
	}

	var versionID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO reelpin.content_versions
			(content_id, processor_version, prompt_version, schema_version, model_version,
			 title, media)
		VALUES ($1, 'v1', 'p1', 's1', 'm1', 'A reel',
		        jsonb_build_object('thumbnail_url', 'https://cdn.example/' || $2 || '.jpg'))
		RETURNING id::text`, contentID, sourceID).Scan(&versionID); err != nil {
		t.Fatalf("seeding a version for %s: %v", sourceID, err)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE reelpin.contents SET current_version_id = $2 WHERE id = $1`,
		contentID, versionID); err != nil {
		t.Fatalf("pointing %s at its version: %v", sourceID, err)
	}
	return contentID
}

func seedSave(t *testing.T, pool *pgxpool.Pool, userID, contentID string) string {
	t.Helper()
	var saveID string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO reelpin.user_saves (user_id, content_id) VALUES ($1, $2)
		RETURNING id::text`, userID, contentID).Scan(&saveID); err != nil {
		t.Fatalf("seeding a save: %v", err)
	}
	return saveID
}

func count(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// stubAuth records identity deletions and can be told to fail, which is the
// only way to reach the half-deleted state on purpose.
type stubAuth struct {
	deleted []string
	err     error
}

func (s *stubAuth) DeleteUser(_ context.Context, userID string) error {
	if s.err != nil {
		return s.err
	}
	s.deleted = append(s.deleted, userID)
	return nil
}

func TestDeletingOneSaveLeavesTheOtherUserAlone(t *testing.T) {
	pool := testPool(t)
	service := New(pool, &stubAuth{}, nil, quiet())
	ctx := context.Background()

	content := seedContent(t, pool, "SHARED1", "public")
	saveA := seedSave(t, pool, userA, content)
	seedSave(t, pool, userB, content)

	if err := service.DeleteReel(ctx, userA, saveA); err != nil {
		t.Fatalf("DeleteReel: %v", err)
	}

	if n := count(t, pool, `SELECT count(*) FROM reelpin.user_saves WHERE user_id = $1`, userA); n != 0 {
		t.Errorf("user A still has %d saves", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM reelpin.user_saves WHERE user_id = $1`, userB); n != 1 {
		t.Errorf("user B has %d saves, want theirs untouched", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM reelpin.contents`); n != 1 {
		t.Errorf("contents = %d, want the shared content kept", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM reelpin.content_versions`); n != 1 {
		t.Errorf("versions = %d, want the extraction user B is reading kept", n)
	}
}

func TestDeletingTheLastSaveKeepsPublicContentReusable(t *testing.T) {
	pool := testPool(t)
	service := New(pool, &stubAuth{}, nil, quiet())
	ctx := context.Background()

	content := seedContent(t, pool, "PUBLIC1", "public")
	save := seedSave(t, pool, userA, content)

	if err := service.DeleteReel(ctx, userA, save); err != nil {
		t.Fatalf("DeleteReel: %v", err)
	}

	// The content survives with no user pointing at it: the next person to
	// share the same link reuses it instead of paying for it again.
	if n := count(t, pool, `SELECT count(*) FROM reelpin.contents WHERE id = $1`, content); n != 1 {
		t.Fatal("public content was removed with its last save")
	}
	if n := count(t, pool, `SELECT count(*) FROM reelpin.user_saves WHERE content_id = $1`, content); n != 0 {
		t.Errorf("saves = %d, want none", n)
	}
}

func TestDeletingTheLastSaveRemovesUnreachablePrivateContent(t *testing.T) {
	pool := testPool(t)
	service := New(pool, &stubAuth{}, nil, quiet())
	ctx := context.Background()

	// A user-scoped identity: nobody else can ever reach it, so once its last
	// save is gone it is unreachable rather than reusable.
	content := seedContent(t, pool, "PRIVATE1", "userhash1234abcd")
	save := seedSave(t, pool, userA, content)

	if err := service.DeleteReel(ctx, userA, save); err != nil {
		t.Fatalf("DeleteReel: %v", err)
	}

	if n := count(t, pool, `SELECT count(*) FROM reelpin.contents WHERE id = $1`, content); n != 0 {
		t.Error("unreachable private content survived its last save")
	}
	if n := count(t, pool, `SELECT count(*) FROM reelpin.content_versions`); n != 0 {
		t.Errorf("versions = %d, want the private extraction gone", n)
	}
}

func TestDeletingAReelThatIsNotYours(t *testing.T) {
	pool := testPool(t)
	service := New(pool, &stubAuth{}, nil, quiet())
	ctx := context.Background()

	content := seedContent(t, pool, "MINE1", "public")
	save := seedSave(t, pool, userA, content)

	for _, id := range []string{save, "not-a-uuid", "99999999-9999-4999-8999-999999999999"} {
		if err := service.DeleteReel(ctx, userB, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("deleting %q as the wrong user: err = %v, want ErrNotFound", id, err)
		}
	}
	if n := count(t, pool, `SELECT count(*) FROM reelpin.user_saves`); n != 1 {
		t.Error("another user's delete removed the save")
	}
}

func TestAccountDeletionRemovesEveryOwnedRow(t *testing.T) {
	pool := testPool(t)
	auth := &stubAuth{}
	service := New(pool, auth, nil, quiet())
	ctx := context.Background()

	shared := seedContent(t, pool, "SHARED2", "public")
	private := seedContent(t, pool, "PRIVATE2", "userhashaaaa1111")
	seedSave(t, pool, userA, shared)
	seedSave(t, pool, userA, private)
	seedSave(t, pool, userB, shared)

	if _, err := pool.Exec(ctx, `
		INSERT INTO reelpin.processing_jobs (user_id, url, normalized_url)
		VALUES ($1, 'https://example.com/x', 'https://example.com/x')`, userA); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO reelpin.idempotency_keys
			(user_id, endpoint, idempotency_key, request_hash, expires_at)
		VALUES ($1, 'processing-jobs/reels', gen_random_uuid(), 'hash', now() + interval '1 day')`,
		userA); err != nil {
		t.Fatal(err)
	}

	report, err := service.DeleteAccount(ctx, userA)
	if err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}

	if !report.DatabaseCleaned || !report.IdentityDeleted || report.Pending {
		t.Fatalf("report = %+v, want both halves done", report)
	}
	if report.Saves != 2 || report.ProcessingJobs != 1 || report.IdempotencyKeys != 1 {
		t.Errorf("report = %+v, want the seeded counts", report)
	}
	if report.PrivateContent != 1 {
		t.Errorf("private content removed = %d, want 1", report.PrivateContent)
	}

	for _, check := range []struct {
		name  string
		query string
	}{
		{"saves", `SELECT count(*) FROM reelpin.user_saves WHERE user_id = $1`},
		{"jobs", `SELECT count(*) FROM reelpin.processing_jobs WHERE user_id = $1`},
		{"idempotency keys", `SELECT count(*) FROM reelpin.idempotency_keys WHERE user_id = $1`},
	} {
		if n := count(t, pool, check.query, userA); n != 0 {
			t.Errorf("%s left behind: %d", check.name, n)
		}
	}

	// User B is untouched, and the content they share is still there.
	if n := count(t, pool, `SELECT count(*) FROM reelpin.user_saves WHERE user_id = $1`, userB); n != 1 {
		t.Error("the other user's save was removed")
	}
	if n := count(t, pool, `SELECT count(*) FROM reelpin.contents WHERE id = $1`, shared); n != 1 {
		t.Error("shared public content was removed")
	}
	if n := count(t, pool, `SELECT count(*) FROM reelpin.contents WHERE id = $1`, private); n != 0 {
		t.Error("the private content nobody can reach survived")
	}
	if len(auth.deleted) != 1 || auth.deleted[0] != userA {
		t.Errorf("identity deletions = %v", auth.deleted)
	}
}

func TestAccountDeletionReportsAMissingIdentityDeleterTruthfully(t *testing.T) {
	pool := testPool(t)
	// No adapter: the old branch's open item. The data goes, the sign-in does
	// not, and the report has to say so rather than claim success.
	service := New(pool, nil, nil, quiet())
	ctx := context.Background()

	seedSave(t, pool, userA, seedContent(t, pool, "NOAUTH1", "public"))

	report, err := service.DeleteAccount(ctx, userA)
	if err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	if !report.DatabaseCleaned {
		t.Error("the data half did not run")
	}
	if report.IdentityDeleted {
		t.Fatal("identity deletion was reported as done with no deleter configured")
	}
	if !report.Pending {
		t.Error("the request was not left pending for a retry")
	}

	pending, err := service.Pending(ctx, userA)
	if err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Error("the subject is not blocked while the deletion is unfinished")
	}
}

func TestAccountDeletionResumesAfterACrash(t *testing.T) {
	pool := testPool(t)
	failing := &stubAuth{err: errors.New("the identity service is away")}
	service := New(pool, failing, nil, quiet())
	ctx := context.Background()

	seedSave(t, pool, userA, seedContent(t, pool, "CRASH1", "public"))

	// First attempt: the data half commits, the identity half fails.
	first, err := service.DeleteAccount(ctx, userA)
	if err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	if !first.DatabaseCleaned || first.IdentityDeleted || !first.Pending {
		t.Fatalf("report = %+v, want the data half done and the identity half pending", first)
	}
	if n := count(t, pool, `SELECT count(*) FROM reelpin.user_saves WHERE user_id = $1`, userA); n != 0 {
		t.Fatal("the data half did not commit")
	}

	// The subject stays blocked in the gap: the crash must not restore access.
	if pending, err := service.Pending(ctx, userA); err != nil || !pending {
		t.Fatalf("pending = %v, %v; want the subject still blocked", pending, err)
	}
	if err := service.DeleteReel(ctx, userA, "99999999-9999-4999-8999-999999999999"); !errors.Is(err, ErrDeletionPending) {
		t.Errorf("a pending subject was served: %v", err)
	}

	// The retry finishes the identity half and does not redo the data half.
	working := &stubAuth{}
	resumed := New(pool, working, nil, quiet())
	second, err := resumed.ResumeAccountDeletion(ctx, userA)
	if err != nil {
		t.Fatalf("ResumeAccountDeletion: %v", err)
	}
	if !second.IdentityDeleted || second.Pending {
		t.Fatalf("report = %+v, want the identity half finished", second)
	}
	if second.Saves != 0 {
		t.Errorf("the resume redid the data half: %+v", second)
	}
	if len(working.deleted) != 1 {
		t.Errorf("identity deletions = %v", working.deleted)
	}
	if pending, err := service.Pending(ctx, userA); err != nil || pending {
		t.Errorf("still pending after both halves finished: %v, %v", pending, err)
	}
}

func TestPendingRequestsListsUnfinishedDeletions(t *testing.T) {
	pool := testPool(t)
	service := New(pool, &stubAuth{err: errors.New("away")}, nil, quiet())
	ctx := context.Background()

	if _, err := service.DeleteAccount(ctx, userA); err != nil {
		t.Fatal(err)
	}

	pending, err := service.PendingRequests(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0] != userA {
		t.Fatalf("pending = %v, want just user A", pending)
	}

	// Once finished, it drops off the list rather than being retried forever.
	if _, err := New(pool, &stubAuth{}, nil, quiet()).ResumeAccountDeletion(ctx, userA); err != nil {
		t.Fatal(err)
	}
	pending, err = service.PendingRequests(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending = %v, want none", pending)
	}
}
