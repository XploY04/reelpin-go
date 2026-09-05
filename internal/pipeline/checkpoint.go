package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Stage names are stored with their results, so a redelivered message can tell
// which work is already done.
const (
	StagePrepare           = "prepare"
	StageTranscribeOrOCR   = "transcribe_or_ocr"
	StageExtract           = "extract"
	StageGeocode           = "geocode"
	StagePersistContent    = "persist_content"
	StagePersonalize       = "personalize"
	StageSave              = "save"
	StageEmitIndex         = "emit_index"
	StageEmitNotifications = "emit_notifications"
	StageComplete          = "complete"
)

// Stages is the order the pipeline runs them in, and the order progress moves
// through.
var Stages = []string{
	StagePrepare,
	StageTranscribeOrOCR,
	StageExtract,
	StageGeocode,
	StagePersistContent,
	StagePersonalize,
	StageSave,
	StageEmitIndex,
	StageEmitNotifications,
	StageComplete,
}

// StageVersion changes when a stage's behaviour changes enough that its stored
// output should no longer be reused.
const StageVersion = "v1"

// Checkpoints remember what a stage produced, so redelivery after a crash
// resumes instead of repeating a download or a paid model call.
type Checkpoints struct {
	pool *pgxpool.Pool
}

func NewCheckpoints(pool *pgxpool.Pool) *Checkpoints {
	return &Checkpoints{pool: pool}
}

// InputHash identifies what a stage was given. A stored result is only reused
// when the stage version and this hash both match, so changing the input
// re-runs the stage instead of silently returning a stale answer.
func InputHash(parts ...string) string {
	digest := sha256.New()
	for _, part := range parts {
		digest.Write([]byte(part))
		digest.Write([]byte{0})
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// Load returns a completed stage's output when it still applies.
func (c *Checkpoints) Load(ctx context.Context, runID, stage, inputHash string, target any) (bool, error) {
	var output []byte
	err := c.pool.QueryRow(ctx, `
		SELECT output FROM reelpin.processing_stage_results
		WHERE run_id = $1 AND stage = $2 AND stage_version = $3
		  AND input_hash = $4 AND status = 'completed'`,
		runID, stage, StageVersion, inputHash,
	).Scan(&output)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading the %s checkpoint: %w", stage, err)
	}
	if target == nil || len(output) == 0 {
		return true, nil
	}
	if err := json.Unmarshal(output, target); err != nil {
		// A checkpoint that no longer decodes is not usable; run the stage again.
		return false, nil
	}
	return true, nil
}

// Save records a stage's output. Re-running a stage overwrites its own row
// rather than accumulating history.
func (c *Checkpoints) Save(ctx context.Context, runID, stage, inputHash string, value any) error {
	output, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encoding the %s checkpoint: %w", stage, err)
	}

	if _, err := c.pool.Exec(ctx, `
		INSERT INTO reelpin.processing_stage_results
			(run_id, stage, stage_version, input_hash, output, status)
		VALUES ($1, $2, $3, $4, $5, 'completed')
		ON CONFLICT (run_id, stage, stage_version)
		DO UPDATE SET input_hash = EXCLUDED.input_hash,
		              output = EXCLUDED.output,
		              status = 'completed',
		              updated_at = now()`,
		runID, stage, StageVersion, inputHash, output,
	); err != nil {
		return fmt.Errorf("writing the %s checkpoint: %w", stage, err)
	}
	return nil
}

// Progress moves the run and every private job watching it. The app polls the
// job, so a stage that does not report progress looks stuck.
func (c *Checkpoints) Progress(ctx context.Context, runID, stage string) error {
	percent := progressFor(stage)

	transaction, err := c.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("starting the progress update: %w", err)
	}
	defer transaction.Rollback(ctx)

	if _, err := transaction.Exec(ctx, `
		UPDATE reelpin.processing_runs
		SET stage = $2, progress_percent = GREATEST(progress_percent, $3), updated_at = now()
		WHERE id = $1`,
		runID, stage, percent,
	); err != nil {
		return fmt.Errorf("updating run progress: %w", err)
	}

	// Progress only ever moves forward, so a redelivered older stage cannot
	// make a job look like it went backwards.
	if _, err := transaction.Exec(ctx, `
		UPDATE public.processing_jobs
		SET current_step = $2,
		    progress_percent = GREATEST(progress_percent, $3),
		    status = CASE WHEN status = 'queued' THEN 'processing' ELSE status END,
		    started_at = COALESCE(started_at, now()),
		    updated_at = now()
		WHERE processing_run_id = $1 AND status IN ('queued', 'processing')`,
		runID, stage, percent,
	); err != nil {
		return fmt.Errorf("updating job progress: %w", err)
	}

	return transaction.Commit(ctx)
}

// progressFor spreads the stages between 5 and 95. The last five points belong
// to the save, so a job never shows 100 before its reel exists.
func progressFor(stage string) int {
	for index, name := range Stages {
		if name == stage {
			if name == StageComplete {
				return 100
			}
			return 5 + (index*90)/len(Stages)
		}
	}
	return 5
}
