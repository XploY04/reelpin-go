package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// stageVersions pins each stage's logic. Bumping one invalidates that stage's
// checkpoints and re-runs it; earlier stages keep their results.
var stageVersions = map[string]string{
	stagePrepare:    "v1",
	stageDownload:   "v1",
	stageTranscribe: "v1",
	stageExtract:    "v1",
	stageCategorize: "v1",
}

// maxOutputBytes bounds what one stage may checkpoint. Raw provider output is
// diagnostic, not an archive; anything bigger is truncated at the source.
const maxOutputBytes = 256 << 10

// InputHash is how a checkpoint knows its inputs did not change: same stage
// version and same input hash mean the stored result is this run's answer.
func InputHash(parts ...string) string {
	digest := sha256.New()
	for _, part := range parts {
		digest.Write([]byte(part))
		digest.Write([]byte{0})
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// loadCheckpoint returns a stage's stored output when its version and input
// hash still match. A mismatch is not an error; it means the stage must run.
func loadCheckpoint(ctx context.Context, pool *pgxpool.Pool, runID, stage, inputHash string, target any) (bool, error) {
	var output string
	err := pool.QueryRow(ctx, `
		SELECT output_ref FROM reelpin.processing_stage_results
		WHERE run_id = $1 AND stage = $2 AND stage_version = $3
		  AND input_hash = $4 AND error_class IS NULL AND finished_at IS NOT NULL`,
		runID, stage, stageVersions[stage], inputHash,
	).Scan(&output)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("loading the %s checkpoint: %w", stage, err)
	}
	if err := json.Unmarshal([]byte(output), target); err != nil {
		// A checkpoint this code wrote and cannot read is stale by definition.
		return false, nil
	}
	return true, nil
}

// saveCheckpoint stores a finished stage's output inside the caller's guarded
// transaction, so a fenced worker cannot checkpoint a stale result.
func saveCheckpoint(ctx context.Context, tx pgx.Tx, runID, stage, inputHash string, output any) error {
	encoded, err := json.Marshal(output)
	if err != nil {
		return fmt.Errorf("encoding the %s checkpoint: %w", stage, err)
	}
	if len(encoded) > maxOutputBytes {
		return fmt.Errorf("the %s checkpoint is %d bytes, cap is %d", stage, len(encoded), maxOutputBytes)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO reelpin.processing_stage_results
			(run_id, stage, stage_version, input_hash, output_ref, attempt_count, started_at, finished_at, error_class)
		VALUES ($1, $2, $3, $4, $5, 1, now(), now(), NULL)
		ON CONFLICT (run_id, stage, stage_version)
		DO UPDATE SET input_hash = EXCLUDED.input_hash,
		              output_ref = EXCLUDED.output_ref,
		              attempt_count = reelpin.processing_stage_results.attempt_count + 1,
		              finished_at = now(),
		              error_class = NULL,
		              updated_at = now()`,
		runID, stage, stageVersions[stage], inputHash, string(encoded),
	)
	if err != nil {
		return fmt.Errorf("saving the %s checkpoint: %w", stage, err)
	}
	return nil
}

// recordStageFailure stores the attempt and its class inside the caller's
// guarded transaction, and returns the attempt count after this failure.
func recordStageFailure(ctx context.Context, tx pgx.Tx, runID, stage string, class Class) (int, error) {
	var attempts int
	err := tx.QueryRow(ctx, `
		INSERT INTO reelpin.processing_stage_results
			(run_id, stage, stage_version, input_hash, attempt_count, started_at, error_class)
		VALUES ($1, $2, $3, '', 1, now(), $4)
		ON CONFLICT (run_id, stage, stage_version)
		DO UPDATE SET attempt_count = reelpin.processing_stage_results.attempt_count + 1,
		              error_class = EXCLUDED.error_class,
		              updated_at = now()
		RETURNING attempt_count`,
		runID, stage, stageVersions[stage], string(class),
	).Scan(&attempts)
	if err != nil {
		return 0, fmt.Errorf("recording the %s failure: %w", stage, err)
	}
	return attempts, nil
}
