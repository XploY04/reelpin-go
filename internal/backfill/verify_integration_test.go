//go:build integration

package backfill

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func verified(t *testing.T, pool *pgxpool.Pool) VerifyReport {
	t.Helper()
	report, err := NewVerifier(pool).Verify(context.Background(), VerifyOptions{})
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	return report
}

func tableOf(t *testing.T, report VerifyReport, name string) TableVerification {
	t.Helper()
	for _, table := range report.Tables {
		if table.SourceTable == name {
			return table
		}
	}
	t.Fatalf("the report has no verification for %s", name)
	return TableVerification{}
}

func mismatched(report VerifyReport, sourceID, field string) bool {
	for _, mismatch := range report.Sample.Examples {
		if mismatch.SourceID == sourceID && mismatch.Field == field {
			return true
		}
	}
	return false
}

func TestVerifyPassesAfterACleanBackfill(t *testing.T) {
	pool := testPool(t)
	seed(t, pool)

	if _, err := New(pool, quiet()).Run(context.Background(), Options{Execute: true}); err != nil {
		t.Fatalf("execute: %v", err)
	}

	report := verified(t, pool)
	if !report.OK() {
		t.Errorf("a clean backfill does not verify: %+v", report)
	}

	reels := tableOf(t, report, sourceReels)
	if reels.InScope != 5 || reels.Carried != 4 || reels.Skipped != 1 {
		t.Errorf("reels: in_scope=%d carried=%d skipped=%d, want 5, 4 and 1",
			reels.InScope, reels.Carried, reels.Skipped)
	}
	if reels.SkippedByAction["skipped_invalid_url"] != 1 {
		t.Errorf("reels skipped by action = %v, want one skipped_invalid_url", reels.SkippedByAction)
	}
	if reels.Unexplained != 0 || reels.CarriedButMissing != 0 {
		t.Errorf("reels: unexplained=%d carried_but_missing=%d, want none",
			reels.Unexplained, reels.CarriedButMissing)
	}

	jobs := tableOf(t, report, sourceJobs)
	if jobs.InScope != 3 || jobs.Carried != 1 || jobs.Skipped != 2 {
		t.Errorf("jobs: in_scope=%d carried=%d skipped=%d, want 3, 1 and 2",
			jobs.InScope, jobs.Carried, jobs.Skipped)
	}
	if jobs.Unexplained != 0 {
		t.Errorf("jobs: unexplained=%d, want none", jobs.Unexplained)
	}

	// Four carried reels and the one carried job.
	if report.Sample.Compared != 5 {
		t.Errorf("compared = %d, want 5", report.Sample.Compared)
	}
	// The two saves of one shared reel: one version, and nothing records which
	// of them built it.
	if report.Sample.TextNotComparable != 2 {
		t.Errorf("text_not_comparable = %d, want the 2 saves of the shared reel",
			report.Sample.TextNotComparable)
	}
	if report.Sample.Mismatches != 0 {
		t.Errorf("mismatches = %d, want none: %+v", report.Sample.Mismatches, report.Sample.Examples)
	}
}

func TestVerifyReportsALegacyRowWithNoAuditEntry(t *testing.T) {
	pool := testPool(t)
	seed(t, pool)
	ctx := context.Background()

	if _, err := New(pool, quiet()).Run(ctx, Options{Execute: true}); err != nil {
		t.Fatalf("execute: %v", err)
	}

	const missed = "aaaaaaaa-0000-4000-8000-00000000009f"
	if _, err := pool.Exec(ctx, `
		INSERT INTO public.reels (id, user_id, url, title)
		VALUES ($1, $2, 'https://www.instagram.com/reel/MISSED1/', 'Never looked at')`,
		missed, userA); err != nil {
		t.Fatalf("adding a legacy row the backfill never saw: %v", err)
	}

	report := verified(t, pool)
	if report.OK() {
		t.Error("a legacy row nothing decided about still verifies")
	}

	reels := tableOf(t, report, sourceReels)
	if reels.InScope != 6 {
		t.Errorf("in_scope = %d, want 6", reels.InScope)
	}
	if reels.Unexplained != 1 {
		t.Fatalf("unexplained = %d, want 1", reels.Unexplained)
	}
	if len(reels.UnexplainedIDs) != 1 || reels.UnexplainedIDs[0] != missed {
		t.Errorf("unexplained_ids = %v, want just %s", reels.UnexplainedIDs, missed)
	}
	if reels.CarriedButMissing != 0 {
		t.Errorf("carried_but_missing = %d, want none", reels.CarriedButMissing)
	}
}

func TestVerifyCatchesAnAlteredCanonicalField(t *testing.T) {
	pool := testPool(t)
	seed(t, pool)
	ctx := context.Background()

	if _, err := New(pool, quiet()).Run(ctx, Options{Execute: true}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !verified(t, pool).OK() {
		t.Fatal("the backfill did not verify before anything was altered")
	}

	if _, err := pool.Exec(ctx, `
		UPDATE reelpin.user_saves SET saved_at = saved_at - interval '1 day' WHERE id = $1`,
		linkReel); err != nil {
		t.Fatalf("altering the save: %v", err)
	}

	// Versions are immutable by trigger, so corrupting one has to go around it.
	// That is the point: this is the damage the check exists to notice.
	for _, statement := range []string{
		`ALTER TABLE reelpin.content_versions DISABLE TRIGGER content_versions_immutable`,
		`UPDATE reelpin.content_versions v SET title = 'Something else'
		 FROM reelpin.contents c JOIN reelpin.user_saves s ON s.content_id = c.id
		 WHERE v.id = c.current_version_id AND s.id = '` + cachedReel + `'`,
		`ALTER TABLE reelpin.content_versions ENABLE TRIGGER content_versions_immutable`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("altering the version: %v", err)
		}
	}

	report := verified(t, pool)
	if report.OK() {
		t.Error("altered canonical rows still verify")
	}
	if report.Sample.Mismatches != 2 {
		t.Errorf("mismatches = %d, want 2: %+v", report.Sample.Mismatches, report.Sample.Examples)
	}
	if !mismatched(report, linkReel, "user_saves.saved_at") {
		t.Errorf("the altered saved_at was not reported: %+v", report.Sample.Examples)
	}
	if !mismatched(report, cachedReel, "content_versions.title") {
		t.Errorf("the altered title was not reported: %+v", report.Sample.Examples)
	}

	// Nothing was carried over any less: the counts still agree.
	reels := tableOf(t, report, sourceReels)
	if reels.Unexplained != 0 || reels.CarriedButMissing != 0 {
		t.Errorf("the counts moved: unexplained=%d carried_but_missing=%d",
			reels.Unexplained, reels.CarriedButMissing)
	}
}

func TestVerifyWritesNothing(t *testing.T) {
	pool := testPool(t)
	seed(t, pool)

	if _, err := New(pool, quiet()).Run(context.Background(), Options{Execute: true}); err != nil {
		t.Fatalf("execute: %v", err)
	}

	legacyBefore, canonicalBefore := legacySnapshot(t, pool), canonicalSnapshot(t, pool)
	verified(t, pool)
	verified(t, pool)

	if after := legacySnapshot(t, pool); after != legacyBefore {
		t.Errorf("verifying changed the legacy tables:\nbefore %s\nafter  %s", legacyBefore, after)
	}
	if after := canonicalSnapshot(t, pool); after != canonicalBefore {
		t.Errorf("verifying changed the canonical tables:\nbefore %s\nafter  %s", canonicalBefore, after)
	}
}

// canonicalSnapshot is every row the backfill writes, so a test can prove the
// verifier only ever reads them.
func canonicalSnapshot(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var snapshot string
	if err := pool.QueryRow(context.Background(), `
		SELECT coalesce(string_agg(line, E'\n' ORDER BY line), '') FROM (
			SELECT c::text AS line FROM reelpin.contents c
			UNION ALL
			SELECT v::text FROM reelpin.content_versions v
			UNION ALL
			SELECT s::text FROM reelpin.user_saves s
			UNION ALL
			SELECT j::text FROM reelpin.processing_jobs j
			UNION ALL
			SELECT r::text FROM reelpin.processing_runs r
			UNION ALL
			SELECT a::text FROM reelpin.backfill_audit a
			UNION ALL
			SELECT p::text FROM reelpin.backfill_progress p
		) rows`).Scan(&snapshot); err != nil {
		t.Fatalf("snapshotting the canonical tables: %v", err)
	}
	return snapshot
}
