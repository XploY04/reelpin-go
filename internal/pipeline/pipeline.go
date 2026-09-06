// Package pipeline turns one shared link into an immutable content version and
// every subscriber's private save. It is platform-neutral: everything that
// touches a provider or a file is an interface, so the whole pipeline runs in
// tests with fakes.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/XploY04/reelpin-go/internal/ai"
	"github.com/XploY04/reelpin-go/internal/lease"
	"github.com/XploY04/reelpin-go/internal/platform"
	"github.com/XploY04/reelpin-go/internal/queue"
	"github.com/XploY04/reelpin-go/internal/sourceidentity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ProcessorVersion identifies this pipeline's overall behaviour. It lives on
// runs and content versions; a bump reprocesses on purpose.
const ProcessorVersion = "go-v1"

// Stage names, in order. Persist is not checkpointed: it is idempotent through
// the run's own status.
const (
	stagePrepare    = "prepare"
	stageDownload   = "download"
	stageTranscribe = "transcribe"
	stageExtract    = "extract"
	stageCategorize = "categorize"
	stagePersist    = "persist"
)

// Per-stage budgets and the whole run's lifetime, from the plan.
var stageTimeouts = map[string]time.Duration{
	stagePrepare:    30 * time.Second,
	stageDownload:   180 * time.Second,
	stageTranscribe: 300 * time.Second,
	stageExtract:    90 * time.Second,
	stageCategorize: 45 * time.Second,
	stagePersist:    30 * time.Second,
}

const runLifetime = 30 * time.Minute

// maxStageExecutions is how many times one stage may run before its failure is
// terminal for the run.
const maxStageExecutions = 3

// retryBackoffs are the waits before the second and third execution.
var retryBackoffs = []time.Duration{30 * time.Second, 5 * time.Minute}

// Cooldowns is the slice of internal/providers the pipeline needs.
type Cooldowns interface {
	Remaining(ctx context.Context, provider string) (time.Duration, error)
}

// Deps are the seams. Everything that costs money or touches a file is behind
// one.
type Deps struct {
	Pool        *pgxpool.Pool
	Handlers    *platform.Registry
	Transcriber ai.Transcriber
	ImageReader ai.ImageReader
	Extractor   ai.Extractor
	Categorizer ai.Categorizer
	// Cooldowns may be nil: no Redis means no shared push-back, and the
	// per-run backoff still applies.
	Cooldowns Cooldowns
	// ModelVersion is stored on every content version this pipeline writes.
	ModelVersion string
	Logger       *slog.Logger
	WorkerID     string
	// TempRoot is where a run's media lives while it is being processed. The
	// run deletes its directory on every exit.
	TempRoot string
	Now      func() time.Time
}

type Pipeline struct {
	deps Deps
}

func New(deps Deps) *Pipeline {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.TempRoot == "" {
		deps.TempRoot = os.TempDir()
	}
	return &Pipeline{deps: deps}
}

// run is everything the stages share for one message.
type run struct {
	ID        string
	ContentID string
	CreatedAt time.Time
	Identity  sourceidentity.SourceIdentity
	Lease     lease.Lease
	WorkDir   string

	Prepared   platform.Prepared
	Media      []ai.Media
	Transcript string
	Extraction ai.Extraction
	Category   ai.Category
	Taxonomy   []ai.TaxonomyOption
}

// Handle is the queue handler: it owns the lease, the outcome, and the line
// between transport retries and durable failure state.
func (p *Pipeline) Handle(ctx context.Context, message queue.Message) (queue.Outcome, error) {
	held, err := lease.Acquire(ctx, p.deps.Pool, message.RunID, p.deps.WorkerID)
	if errors.Is(err, lease.ErrNotAcquired) {
		// Finished, or another worker holds it: this delivery is a duplicate
		// either way, and acknowledging it is the idempotent answer.
		return queue.Outcome{Kind: queue.Done}, nil
	}
	if err != nil {
		// The database is unreachable before any durable state could be
		// written: the one case for a broker-side transport retry.
		return queue.Outcome{Kind: queue.Retry, Attempt: 1}, err
	}

	err = p.process(ctx, held)
	switch {
	case err == nil:
		return queue.Outcome{Kind: queue.Done}, nil
	case errors.Is(err, lease.ErrFenced):
		// A newer claim owns the run. Discard everything; theirs counts.
		p.deps.Logger.Warn("fenced mid-run, discarding", "run_id", held.RunID)
		return queue.Outcome{Kind: queue.Done}, nil
	default:
		// The failure and its retry or terminal state were committed durably
		// inside process; the message itself is finished.
		return queue.Outcome{Kind: queue.Done}, err
	}
}

func (p *Pipeline) process(ctx context.Context, held lease.Lease) error {
	state, err := p.load(ctx, held)
	if err != nil {
		return p.applyFailure(ctx, held, stagePrepare, Classify(err))
	}

	// The lease renews itself while stages run; a fence cancels everything.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go p.renew(ctx, held, cancel)

	workDir, err := os.MkdirTemp(p.deps.TempRoot, "run-")
	if err != nil {
		return p.applyFailure(ctx, held, stagePrepare, Classify(err))
	}
	state.WorkDir = workDir
	defer os.RemoveAll(workDir)

	for _, stage := range []string{stagePrepare, stageDownload, stageTranscribe, stageExtract, stageCategorize} {
		if p.deps.Now().After(state.CreatedAt.Add(runLifetime)) {
			return p.applyFailure(ctx, held, stage, errRunExpired)
		}
		if err := p.runStage(ctx, stage, state); err != nil {
			if errors.Is(err, lease.ErrFenced) {
				return err
			}
			return p.applyFailure(ctx, held, stage, Classify(err))
		}
	}

	stageCtx, cancelPersist := context.WithTimeout(ctx, stageTimeouts[stagePersist])
	defer cancelPersist()
	if err := p.persist(stageCtx, state); err != nil {
		if errors.Is(err, lease.ErrFenced) {
			return err
		}
		return p.applyFailure(ctx, held, stagePersist, Classify(err))
	}
	return nil
}

func (p *Pipeline) renew(ctx context.Context, held lease.Lease, cancel context.CancelFunc) {
	ticker := time.NewTicker(lease.RenewalInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := lease.Renew(ctx, p.deps.Pool, held); err != nil {
				// Fenced or unreachable: either way this worker must stop
				// before it commits anything stale.
				cancel()
				return
			}
		}
	}
}

func (p *Pipeline) runStage(ctx context.Context, stage string, state *run) error {
	ctx, cancel := context.WithTimeout(ctx, stageTimeouts[stage])
	defer cancel()

	switch stage {
	case stagePrepare:
		return p.prepare(ctx, state)
	case stageDownload:
		return p.download(ctx, state)
	case stageTranscribe:
		return p.transcribe(ctx, state)
	case stageExtract:
		return p.extract(ctx, state)
	case stageCategorize:
		return p.categorize(ctx, state)
	default:
		return fmt.Errorf("unknown stage %q", stage)
	}
}

func (p *Pipeline) prepare(ctx context.Context, state *run) error {
	hash := InputHash(state.Identity.NormalizedURL, ProcessorVersion)
	if ok, err := loadCheckpoint(ctx, p.deps.Pool, state.ID, stagePrepare, hash, &state.Prepared); err != nil {
		return err
	} else if ok {
		return nil
	}

	handler, ok := p.deps.Handlers.Get(state.Identity.Platform)
	if !ok {
		return errUnsupportedPlatform
	}
	prepared, err := handler.Prepare(ctx, state.Identity)
	if err != nil {
		return err
	}
	state.Prepared = prepared
	return p.commitStage(ctx, state, stagePrepare, hash, prepared, 20)
}

func (p *Pipeline) download(ctx context.Context, state *run) error {
	if !state.Prepared.NeedsMedia {
		state.Media = nil
		return nil
	}
	hash := InputHash(state.Identity.NormalizedURL, "media")
	if ok, err := loadCheckpoint(ctx, p.deps.Pool, state.ID, stageDownload, hash, &state.Media); err != nil {
		return err
	} else if ok && mediaStillOnDisk(state.Media) {
		return nil
	}

	handler, ok := p.deps.Handlers.Get(state.Identity.Platform)
	if !ok {
		return errUnsupportedPlatform
	}
	media, err := handler.Download(ctx, state.Identity, state.WorkDir)
	if err != nil {
		return err
	}
	state.Media = media
	return p.commitStage(ctx, state, stageDownload, hash, media, 40)
}

// mediaStillOnDisk guards a resumed run on a different worker: the checkpoint
// row survives a crash, the temp files do not.
func mediaStillOnDisk(media []ai.Media) bool {
	for _, item := range media {
		if _, err := os.Stat(item.Path); err != nil {
			return false
		}
	}
	return len(media) > 0
}

func (p *Pipeline) transcribe(ctx context.Context, state *run) error {
	if !state.Prepared.NeedsMedia {
		state.Transcript = strings.TrimSpace(state.Prepared.PageText)
		return nil
	}
	hash := InputHash(mediaHashParts(state.Media)...)
	if ok, err := loadCheckpoint(ctx, p.deps.Pool, state.ID, stageTranscribe, hash, &state.Transcript); err != nil {
		return err
	} else if ok {
		return nil
	}

	var spoken, read string
	var audio *ai.Media
	images := []ai.Media{}
	for index, item := range state.Media {
		if strings.HasPrefix(item.MIMEType, "audio/") || strings.HasPrefix(item.MIMEType, "video/") {
			if audio == nil {
				audio = &state.Media[index]
			}
			continue
		}
		if strings.HasPrefix(item.MIMEType, "image/") {
			images = append(images, item)
		}
	}
	if audio != nil {
		text, err := p.deps.Transcriber.Transcribe(ctx, *audio)
		if err != nil {
			return err
		}
		spoken = text
	}
	if len(images) > 0 {
		text, err := p.deps.ImageReader.ReadText(ctx, images)
		if err != nil {
			return err
		}
		read = text
	}

	state.Transcript = strings.TrimSpace(strings.TrimSpace(spoken) + "\n" + strings.TrimSpace(read))
	return p.commitStage(ctx, state, stageTranscribe, hash, state.Transcript, 60)
}

func mediaHashParts(media []ai.Media) []string {
	parts := make([]string, 0, len(media)*2)
	for _, item := range media {
		parts = append(parts, item.Path, item.MIMEType)
	}
	if len(parts) == 0 {
		parts = []string{"none"}
	}
	return parts
}

func (p *Pipeline) extract(ctx context.Context, state *run) error {
	hash := InputHash(state.Transcript, state.Prepared.Caption, ai.PromptVersion, ai.SchemaVersion)
	if ok, err := loadCheckpoint(ctx, p.deps.Pool, state.ID, stageExtract, hash, &state.Extraction); err != nil {
		return err
	} else if ok {
		return nil
	}

	extraction, err := p.deps.Extractor.Extract(ctx, state.Transcript, state.Prepared.Caption)
	if err != nil {
		return err
	}
	// The gate: the provider's schema enforcement is a hint, this is the rule.
	if err := extraction.Validate(); err != nil {
		return err
	}
	state.Extraction = extraction
	return p.commitStage(ctx, state, stageExtract, hash, extraction, 80)
}

func (p *Pipeline) categorize(ctx context.Context, state *run) error {
	taxonomy, err := p.loadTaxonomy(ctx)
	if err != nil {
		return err
	}
	state.Taxonomy = taxonomy

	hash := InputHash(state.Extraction.Title, state.Extraction.Summary, ai.PromptVersion, taxonomyHash(taxonomy))
	if ok, err := loadCheckpoint(ctx, p.deps.Pool, state.ID, stageCategorize, hash, &state.Category); err != nil {
		return err
	} else if ok {
		return nil
	}

	category, err := p.deps.Categorizer.Categorize(ctx, state.Extraction, taxonomy)
	if err != nil {
		return err
	}
	state.Category = category
	return p.commitStage(ctx, state, stageCategorize, hash, category, 90)
}

// loadTaxonomy reads the active tree once per stage run, so categorization is
// deterministic against the tree as it stood.
func (p *Pipeline) loadTaxonomy(ctx context.Context) ([]ai.TaxonomyOption, error) {
	rows, err := p.deps.Pool.Query(ctx, `
		SELECT id::text, COALESCE(parent_id::text, ''), name, description
		FROM reelpin.categories
		WHERE active
		ORDER BY parent_id NULLS FIRST, normalized_name`)
	if err != nil {
		return nil, fmt.Errorf("loading the taxonomy: %w", err)
	}
	defer rows.Close()

	byID := map[string]*ai.TaxonomyOption{}
	roots := []string{}
	type child struct{ parent, id string }
	children := []child{}
	for rows.Next() {
		var id, parent, name, description string
		if err := rows.Scan(&id, &parent, &name, &description); err != nil {
			return nil, fmt.Errorf("reading a category: %w", err)
		}
		byID[id] = &ai.TaxonomyOption{ID: id, Name: name, Description: description}
		if parent == "" {
			roots = append(roots, id)
		} else {
			children = append(children, child{parent: parent, id: id})
		}
	}
	if rows.Err() != nil {
		return nil, fmt.Errorf("loading the taxonomy: %w", rows.Err())
	}
	for _, c := range children {
		if parent, ok := byID[c.parent]; ok {
			parent.Subcategories = append(parent.Subcategories, *byID[c.id])
		}
	}
	taxonomy := make([]ai.TaxonomyOption, 0, len(roots))
	for _, id := range roots {
		taxonomy = append(taxonomy, *byID[id])
	}
	return taxonomy, nil
}

func taxonomyHash(taxonomy []ai.TaxonomyOption) string {
	parts := []string{}
	for _, option := range taxonomy {
		parts = append(parts, option.ID)
		for _, sub := range option.Subcategories {
			parts = append(parts, sub.ID)
		}
	}
	return InputHash(parts...)
}

// commitStage checkpoints a finished stage and moves the visible progress, in
// one guarded transaction, so a fenced worker checkpoints nothing.
func (p *Pipeline) commitStage(ctx context.Context, state *run, stage, hash string, output any, progress int) error {
	tx, err := p.deps.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("starting the %s commit: %w", stage, err)
	}
	defer tx.Rollback(ctx)

	err = lease.GuardedExec(ctx, tx, state.Lease, func(guarded pgx.Tx) error {
		if err := saveCheckpoint(ctx, guarded, state.ID, stage, hash, output); err != nil {
			return err
		}
		if _, err := guarded.Exec(ctx, `
			UPDATE reelpin.processing_runs SET stage = $2, updated_at = now() WHERE id = $1`,
			state.ID, stage); err != nil {
			return err
		}
		_, err := guarded.Exec(ctx, `
			UPDATE reelpin.processing_jobs
			SET status = 'processing', current_step = $2, progress_percent = $3, updated_at = now()
			WHERE run_id = $1 AND status IN ('queued', 'processing')`,
			state.ID, stage, progress)
		return err
	})
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// load reads the run and reconstructs the identity the handlers need.
func (p *Pipeline) load(ctx context.Context, held lease.Lease) (*run, error) {
	state := &run{ID: held.RunID, Lease: held}
	var contentID *string
	err := p.deps.Pool.QueryRow(ctx, `
		SELECT r.created_at, c.id::text, c.source_platform, c.source_content_type,
		       COALESCE(c.source_content_id, ''), c.normalized_url
		FROM reelpin.processing_runs r
		JOIN reelpin.contents c ON c.id = r.content_id
		WHERE r.id = $1`, held.RunID,
	).Scan(&state.CreatedAt, &state.ContentID, &state.Identity.Platform,
		&state.Identity.ContentType, &contentID, &state.Identity.NormalizedURL)
	if err != nil {
		return nil, fmt.Errorf("loading the run: %w", err)
	}
	if contentID != nil {
		state.Identity.ContentID = *contentID
	}
	state.Identity.OriginalURL = state.Identity.NormalizedURL
	return state, nil
}

// applyFailure commits what happens next: a scheduled retry or a terminal end,
// durably, before the message is acknowledged.
func (p *Pipeline) applyFailure(ctx context.Context, held lease.Lease, stage string, failure *Failure) error {
	// Bookkeeping must survive the stage context being done.
	ctx = context.WithoutCancel(ctx)

	tx, err := p.deps.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("starting the failure commit: %w", err)
	}
	defer tx.Rollback(ctx)

	var terminal bool
	err = lease.GuardedExec(ctx, tx, held, func(guarded pgx.Tx) error {
		attempts, err := recordStageFailure(ctx, guarded, held.RunID, stage, failure.Class)
		if err != nil {
			return err
		}

		terminal = !failure.Retryable() || attempts >= maxStageExecutions
		if !terminal {
			delay, deadlineErr := p.retryDelay(ctx, guarded, held.RunID, attempts, failure)
			if deadlineErr != nil {
				terminal = true
				failure = errRunExpired
			} else {
				return p.scheduleRetry(ctx, guarded, held, stage, attempts, delay)
			}
		}
		return p.completeAsFailed(ctx, guarded, held.RunID, failure)
	})
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing the failure: %w", err)
	}

	p.deps.Logger.Warn("stage failed",
		"run_id", held.RunID, "stage", stage, "class", string(failure.Class),
		"code", failure.Code, "terminal", terminal, "error", failure.Err)
	return failure
}

// retryDelay is the maximum of normal backoff, the provider's own push-back
// and the shared cooldown, bounded by the run deadline.
func (p *Pipeline) retryDelay(ctx context.Context, tx pgx.Tx, runID string, attempts int, failure *Failure) (time.Duration, error) {
	delay := retryBackoffs[len(retryBackoffs)-1]
	if attempts-1 < len(retryBackoffs) {
		delay = retryBackoffs[attempts-1]
	}

	var provider *ai.ProviderError
	if errors.As(failure.Err, &provider) && provider.RetryAfter > delay {
		delay = provider.RetryAfter
	}
	if p.deps.Cooldowns != nil {
		if remaining, err := p.deps.Cooldowns.Remaining(ctx, "gemini"); err == nil && remaining > delay {
			delay = remaining
		}
	}

	var createdAt time.Time
	if err := tx.QueryRow(ctx,
		`SELECT created_at FROM reelpin.processing_runs WHERE id = $1`, runID).Scan(&createdAt); err != nil {
		return 0, err
	}
	if p.deps.Now().Add(delay).After(createdAt.Add(runLifetime)) {
		return 0, errors.New("the retry would land past the run deadline")
	}
	return delay, nil
}

var retryNamespace = uuid.MustParse("6ba7b811-9dad-11d1-80b4-00c04fd430c8")

func (p *Pipeline) scheduleRetry(ctx context.Context, tx pgx.Tx, held lease.Lease, stage string, attempts int, delay time.Duration) error {
	if _, err := tx.Exec(ctx, `
		UPDATE reelpin.processing_runs
		SET status = 'retry_scheduled', lease_owner = NULL, lease_expires_at = NULL, updated_at = now()
		WHERE id = $1`, held.RunID); err != nil {
		return err
	}

	routingKey, err := p.routingKeyFor(ctx, tx, held.RunID)
	if err != nil {
		return err
	}
	eventID := uuid.NewSHA1(retryNamespace,
		[]byte(fmt.Sprintf("retry:%s:%s:%d", held.RunID, stage, attempts))).String()
	payload := fmt.Sprintf(`{"run_id":%q,"dispatch_generation":%d}`, held.RunID, held.Generation)
	_, err = tx.Exec(ctx, `
		INSERT INTO reelpin.outbox_events (event_id, event_type, routing_key, schema_version, payload, available_at)
		VALUES ($1, 'run.resume', $2, 1, $3::jsonb, now() + $4::interval)
		ON CONFLICT (event_id) DO NOTHING`,
		eventID, routingKey, payload, delay.String())
	return err
}

func (p *Pipeline) routingKeyFor(ctx context.Context, tx pgx.Tx, runID string) (string, error) {
	var routingKey string
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(
			(SELECT routing_key FROM reelpin.outbox_events
			 WHERE payload->>'run_id' = $1 AND event_type != 'run.resume'
			 ORDER BY created_at DESC LIMIT 1),
			$2)`, runID, queue.QueueLight).Scan(&routingKey)
	return routingKey, err
}

// completeAsFailed ends the run and every subscriber job with the stable
// public code. Internal detail stays in logs and stage rows.
func (p *Pipeline) completeAsFailed(ctx context.Context, tx pgx.Tx, runID string, failure *Failure) error {
	if _, err := tx.Exec(ctx, `
		UPDATE reelpin.processing_runs
		SET status = 'failed', failure_code = $2, failure_message = $3,
		    lease_owner = NULL, lease_expires_at = NULL, updated_at = now()
		WHERE id = $1`, runID, failure.Code, failure.Message); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		UPDATE reelpin.processing_jobs
		SET status = 'failed', failure_code = $2, completed_at = now(), updated_at = now()
		WHERE run_id = $1 AND status IN ('queued', 'processing')`,
		runID, failure.Code)
	return err
}
