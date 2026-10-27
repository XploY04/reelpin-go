//go:build integration

package taxonomy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/XploY04/reelpin-go/internal/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

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

	name := "reelpin_taxonomy_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
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
	return pool
}

func quiet() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// stubJudge answers with whatever the test set up, or fails on demand.
type stubJudge struct {
	decision Decision
	err      error
	calls    int
}

func (s *stubJudge) Judge(context.Context, string) (Decision, error) {
	s.calls++
	if s.err != nil {
		return Decision{}, s.err
	}
	return s.decision, nil
}

func seedCategory(t *testing.T, pool *pgxpool.Pool, name string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO reelpin.categories (name, normalized_name, description)
		VALUES ($1, $2, 'seeded')
		RETURNING id::text`, name, Normalize(name)).Scan(&id); err != nil {
		t.Fatalf("seeding category %q: %v", name, err)
	}
	return id
}

// seedProposal files one proposal from one distinct run, which is what the
// content-count threshold measures.
func seedProposal(t *testing.T, pool *pgxpool.Pool, name string, runs int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < runs; i++ {
		var runID string
		if err := pool.QueryRow(ctx, `
			WITH content AS (
				INSERT INTO reelpin.contents
					(source_platform, source_content_type, source_content_id,
					 normalized_url, normalized_url_hash, access_scope_hash)
				VALUES ('instagram', 'reel', $1, 'https://example.com/'||$1, $1, 'public')
				RETURNING id
			)
			INSERT INTO reelpin.processing_runs (content_id, processor_version)
			SELECT id, 'v1' FROM content
			RETURNING id::text`, name+"-"+string(rune('a'+i))).Scan(&runID); err != nil {
			t.Fatalf("seeding a run: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO reelpin.category_proposals
				(proposed_name, normalized_name, description, source_run_id)
			VALUES ($1, $2, 'what the model wanted', $3)`,
			name, Normalize(name), runID); err != nil {
			t.Fatalf("seeding a proposal: %v", err)
		}
	}
}

func counts(t *testing.T, pool *pgxpool.Pool) (categories, aliases, pending, runs int) {
	t.Helper()
	ctx := context.Background()
	for query, target := range map[string]*int{
		`SELECT count(*) FROM reelpin.categories WHERE active`:                     &categories,
		`SELECT count(*) FROM reelpin.category_aliases`:                            &aliases,
		`SELECT count(*) FROM reelpin.category_proposals WHERE status = 'pending'`: &pending,
		`SELECT count(*) FROM reelpin.taxonomy_runs`:                               &runs,
	} {
		if err := pool.QueryRow(ctx, query).Scan(target); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	return
}

func TestADryRunChangesNothing(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	seedCategory(t, pool, "Food")
	seedProposal(t, pool, "Street Food", 4)

	before := [4]int{}
	before[0], before[1], before[2], before[3] = counts(t, pool)

	judge := &stubJudge{decision: Decision{Actions: []Action{
		{NormalizedName: "street food", Verdict: VerdictAdd, Name: "Street Food", Confidence: 0.99},
	}}}
	report, err := NewCurator(pool, judge, quiet()).Curate(ctx, true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if report.Additions != 1 {
		t.Fatalf("the dry run decided %d additions, want it to still decide", report.Additions)
	}

	after := [4]int{}
	after[0], after[1], after[2], after[3] = counts(t, pool)
	if after != before {
		t.Fatalf("a dry run mutated rows: before %v after %v; even the run record must wait", before, after)
	}
}

func TestBelowThresholdAndDuplicatesAreNotActivated(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	seedCategory(t, pool, "Food")
	seedProposal(t, pool, "Street Food", 2) // one short of the threshold
	seedProposal(t, pool, "Food", 9)        // already exists

	judge := &stubJudge{decision: Decision{Actions: []Action{
		{NormalizedName: "street food", Verdict: VerdictAdd, Name: "Street Food", Confidence: 1},
		{NormalizedName: "food", Verdict: VerdictAdd, Name: "Food", Confidence: 1},
	}}}
	report, err := NewCurator(pool, judge, quiet()).Curate(ctx, false)
	if err != nil {
		t.Fatalf("curate: %v", err)
	}
	if report.Additions != 0 || report.Skipped != 2 {
		t.Fatalf("report = %+v, want both skipped", report)
	}

	categories, _, pending, _ := counts(t, pool)
	if categories != 1 {
		t.Fatalf("active categories = %d, want only the seeded one", categories)
	}
	// Nothing was decided about them, so they stay pending for next week.
	if pending != 11 {
		t.Fatalf("pending proposals = %d, want all 11 left for another week", pending)
	}
}

func TestAModelFailureLeavesTheTaxonomyUnchanged(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	seedCategory(t, pool, "Food")
	seedProposal(t, pool, "Street Food", 5)
	categoriesBefore, aliasesBefore, pendingBefore, _ := counts(t, pool)

	for _, judge := range []*stubJudge{
		{err: errors.New("the model is away")},
		// A structured-output failure looks the same from here: no usable
		// decision, so nothing is applied.
		{err: errors.New("the curation model did not answer with the schema")},
	} {
		_, err := NewCurator(pool, judge, quiet()).Curate(ctx, false)
		if !errors.Is(err, ErrModelFailed) {
			t.Fatalf("err = %v, want ErrModelFailed", err)
		}
	}

	categories, aliases, pending, runs := counts(t, pool)
	if categories != categoriesBefore || aliases != aliasesBefore || pending != pendingBefore {
		t.Fatal("a failed run changed the taxonomy")
	}
	// The failures are recorded, so a quiet week is distinguishable from a
	// broken one.
	if runs != 2 {
		t.Fatalf("taxonomy runs = %d, want both failures recorded", runs)
	}

	var applied bool
	if err := pool.QueryRow(ctx,
		`SELECT bool_or(applied) FROM reelpin.taxonomy_runs`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("a failed run was recorded as applied")
	}
}

func TestAnAppliedRunRollsBackAndKeepsItsHistory(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	foodID := seedCategory(t, pool, "Food")
	seedProposal(t, pool, "Street Food", 4)
	seedProposal(t, pool, "Eats", 3)

	before, err := New(pool).ActiveTree(ctx)
	if err != nil {
		t.Fatal(err)
	}

	judge := &stubJudge{decision: Decision{Actions: []Action{
		{NormalizedName: "street food", Verdict: VerdictAdd, Name: "Street Food",
			Description: "food sold on a street", Confidence: 0.97},
		{NormalizedName: "eats", Verdict: VerdictAlias, AliasOf: "Food", Confidence: 0.95},
	}}}
	report, err := NewCurator(pool, judge, quiet()).Curate(ctx, false)
	if err != nil {
		t.Fatalf("curate: %v", err)
	}
	if report.Additions != 1 || report.Aliases != 1 || report.RunID == "" {
		t.Fatalf("report = %+v", report)
	}

	categories, aliases, pending, _ := counts(t, pool)
	if categories != 2 || aliases != 1 || pending != 0 {
		t.Fatalf("after curation: categories=%d aliases=%d pending=%d", categories, aliases, pending)
	}
	// The alias points at the category the model named.
	var target string
	if err := pool.QueryRow(ctx,
		`SELECT category_id::text FROM reelpin.category_aliases WHERE normalized_alias = 'eats'`).Scan(&target); err != nil {
		t.Fatal(err)
	}
	if target != foodID {
		t.Fatalf("alias points at %s, want %s", target, foodID)
	}

	undone, err := NewCurator(pool, nil, quiet()).Rollback(ctx, report.RunID)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if undone != 2 {
		t.Fatalf("undone = %d, want both actions", undone)
	}

	after, err := New(pool).ActiveTree(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != before.Version {
		t.Fatalf("the active tree did not return to its prior version: %s then %s",
			before.Version, after.Version)
	}

	categories, aliases, pending, runs := counts(t, pool)
	if categories != 1 || aliases != 0 {
		t.Fatalf("after rollback: categories=%d aliases=%d", categories, aliases)
	}
	// The proposals come back so next week can reconsider them.
	if pending != 7 {
		t.Fatalf("pending = %d, want every proposal returned", pending)
	}
	// History is append-only: the run is still there, marked not applied.
	if runs != 1 {
		t.Fatalf("taxonomy runs = %d, want the record kept", runs)
	}
	var applied bool
	var decision, rollback []byte
	if err := pool.QueryRow(ctx,
		`SELECT applied, decision, rollback FROM reelpin.taxonomy_runs WHERE id = $1`,
		report.RunID).Scan(&applied, &decision, &rollback); err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Error("the rolled-back run is still marked applied")
	}
	if !json.Valid(decision) || !json.Valid(rollback) {
		t.Error("the audit record is not readable")
	}
	if !strings.Contains(string(decision), "street food") {
		t.Error("the decision record does not say what was decided")
	}
}

func TestRollingBackTwiceIsSafe(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	seedCategory(t, pool, "Food")
	seedProposal(t, pool, "Street Food", 4)

	judge := &stubJudge{decision: Decision{Actions: []Action{
		{NormalizedName: "street food", Verdict: VerdictAdd, Name: "Street Food", Confidence: 0.99},
	}}}
	report, err := NewCurator(pool, judge, quiet()).Curate(ctx, false)
	if err != nil {
		t.Fatal(err)
	}

	curator := NewCurator(pool, nil, quiet())
	if _, err := curator.Rollback(ctx, report.RunID); err != nil {
		t.Fatal(err)
	}
	// An operator retrying is not a mistake.
	undone, err := curator.Rollback(ctx, report.RunID)
	if err != nil {
		t.Fatalf("second rollback: %v", err)
	}
	if undone != 0 {
		t.Fatalf("the second rollback undid %d actions", undone)
	}

	if _, err := curator.Rollback(ctx, "11111111-1111-4111-8111-111111111111"); err == nil {
		t.Error("rolling back a run that does not exist succeeded")
	}
}

func TestTheTreeVersionIsStableWithinARun(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	seedCategory(t, pool, "Food")
	seedCategory(t, pool, "Travel")
	service := New(pool)

	first, err := service.ActiveTree(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.ActiveTree(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.Version != second.Version {
		t.Fatalf("two reads of an unchanged tree gave %s and %s; categorization would not be reproducible",
			first.Version, second.Version)
	}
	if first.Version == "" || len(first.Options) != 2 {
		t.Fatalf("tree = %+v", first)
	}

	// Activating a category changes the version, which is what makes a
	// pipeline checkpoint stale and forces recategorization.
	seedCategory(t, pool, "Fitness")
	third, err := service.ActiveTree(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if third.Version == first.Version {
		t.Fatal("adding a category left the version unchanged; stale checkpoints would be reused")
	}

	// Deactivating returns it, so a rollback restores the pinned version.
	if _, err := pool.Exec(ctx,
		`UPDATE reelpin.categories SET active = false WHERE normalized_name = 'fitness'`); err != nil {
		t.Fatal(err)
	}
	fourth, err := service.ActiveTree(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fourth.Version != first.Version {
		t.Fatal("deactivating did not restore the earlier version")
	}
}

func TestSubcategoriesNestUnderTheirParent(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	foodID := seedCategory(t, pool, "Food")
	if _, err := pool.Exec(ctx, `
		INSERT INTO reelpin.categories (parent_id, name, normalized_name, description)
		VALUES ($1, 'Cafes', 'cafes', 'places to sit')`, foodID); err != nil {
		t.Fatal(err)
	}

	tree, err := New(pool).ActiveTree(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Options) != 1 {
		t.Fatalf("roots = %d, want the subcategory nested rather than listed", len(tree.Options))
	}
	if len(tree.Options[0].Subcategories) != 1 || tree.Options[0].Subcategories[0].Name != "Cafes" {
		t.Fatalf("tree = %+v", tree.Options[0])
	}
}

func TestPendingProposalsCountDistinctRuns(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	seedProposal(t, pool, "Street Food", 3)

	// The same run proposing again is one opinion, not two.
	var runID string
	if err := pool.QueryRow(ctx,
		`SELECT source_run_id::text FROM reelpin.category_proposals LIMIT 1`).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO reelpin.category_proposals
			(proposed_name, normalized_name, description, source_run_id)
		VALUES ('street food', 'street food', 'again', $1)`, runID); err != nil {
		t.Fatal(err)
	}

	proposals, err := PendingProposals(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposals) != 1 {
		t.Fatalf("proposals = %d, want one concept", len(proposals))
	}
	if proposals[0].ContentCount != 3 {
		t.Fatalf("content count = %d, want 3 distinct runs", proposals[0].ContentCount)
	}
	if len(proposals[0].IDs) != 4 {
		t.Fatalf("ids = %d, want every row so all four are marked", len(proposals[0].IDs))
	}
}

// TestTheJudgeReadsAStructuredAnswer covers the model client itself against a
// local server, so the parsing is exercised without a key or a network.
func TestTheJudgeReadsAStructuredAnswer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, CuratorModel) {
			t.Errorf("called %s, want the pinned curation model", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		// A fenced answer, which the real model sometimes returns anyway.
		w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"` +
			"```json\\n{\\\"actions\\\":[{\\\"normalized_name\\\":\\\"street food\\\",\\\"action\\\":\\\"add\\\",\\\"confidence\\\":0.93}]}\\n```" +
			`"}]}}]}`))
	}))
	defer server.Close()

	decision, err := NewGeminiJudge("test-key", server.URL).Judge(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if len(decision.Actions) != 1 || decision.Actions[0].Confidence != 0.93 {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestTheJudgeRefusesUnusableAnswers(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "provider error", status: 503, body: `{}`},
		{name: "no candidate", status: 200, body: `{"candidates":[]}`},
		{name: "not the schema", status: 200,
			body: `{"candidates":[{"content":{"parts":[{"text":"sorry, I cannot"}]}}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				w.Write([]byte(tt.body))
			}))
			defer server.Close()

			if _, err := NewGeminiJudge("k", server.URL).Judge(context.Background(), "p"); err == nil {
				t.Fatal("an unusable answer was accepted")
			}
		})
	}
}

func TestNothingToDecideIsNotARun(t *testing.T) {
	pool := testPool(t)
	seedCategory(t, pool, "Food")

	judge := &stubJudge{}
	report, err := NewCurator(pool, judge, quiet()).Curate(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if judge.calls != 0 {
		t.Error("the model was called with no proposals to judge")
	}
	if report.Applied {
		t.Error("an empty week was reported as applied")
	}
	if _, _, _, runs := counts(t, pool); runs != 0 {
		t.Errorf("taxonomy runs = %d, want no record for a week with nothing to decide", runs)
	}
}
