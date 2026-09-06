package taxonomy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CuratorModel is the stable, deliberately cheap model the weekly run uses.
// It is pinned rather than following the pipeline's model: a taxonomy decision
// should not silently change because extraction was upgraded.
const CuratorModel = "gemini-3.5-flash-lite"

// CuratorPromptVersion is stored with every run, so a decision can be read
// back knowing what it was asked.
const CuratorPromptVersion = "curate-v1"

// The auto-approval policy. All three must hold, and they are deliberately
// conservative: a wrong addition is visible to every user and costs a
// migration to undo, while a missed one simply waits a week.
const (
	// MinContentCount is distinct runs that wanted the concept. One run asking
	// five times is one opinion.
	MinContentCount = 3
	// MinConfidence is the model's own certainty.
	MinConfidence = 0.90
	// MaxAdditionsPerRun bounds the blast radius of one bad week.
	MaxAdditionsPerRun = 5
)

// ErrModelFailed means the curator could not get a usable decision. Nothing is
// applied and the run is recorded as unapplied: a failed run changes nothing
// and can wait a week.
var ErrModelFailed = errors.New("taxonomy curation model failed")

// Judge is the one model call curation makes. Declared here because the
// curator is its only consumer.
type Judge interface {
	Judge(ctx context.Context, prompt string) (Decision, error)
}

// Decision is what the model answered, before policy is applied.
type Decision struct {
	Actions []Action `json:"actions"`
}

// Action is one verdict on one proposed concept.
type Action struct {
	NormalizedName string  `json:"normalized_name"`
	Verdict        string  `json:"action"`
	Name           string  `json:"name,omitempty"`
	Description    string  `json:"description,omitempty"`
	AliasOf        string  `json:"alias_of,omitempty"`
	Confidence     float64 `json:"confidence"`

	// Filled by the curator when policy runs. Applied says whether it changed
	// anything; Skipped says why not, so a run's record explains itself.
	Applied    bool   `json:"applied"`
	Skipped    string `json:"skipped,omitempty"`
	CategoryID string `json:"category_id,omitempty"`
}

const (
	VerdictAdd    = "add"
	VerdictAlias  = "alias"
	VerdictReject = "reject"
)

// Rollback is the inverse of one applied run, recorded at apply time so a
// rollback never has to reconstruct what happened from the current state.
type Rollback struct {
	Actions []RollbackAction `json:"actions"`
}

type RollbackAction struct {
	Kind string `json:"kind"`
	// CategoryID is deactivated by undo_add.
	CategoryID string `json:"category_id,omitempty"`
	// NormalizedAlias is removed by undo_alias.
	NormalizedAlias string `json:"normalized_alias,omitempty"`
	// ProposalIDs go back to pending in every case, so next week can
	// reconsider what a rolled-back run decided.
	ProposalIDs []string `json:"proposal_ids,omitempty"`
}

const (
	undoAdd   = "undo_add"
	undoAlias = "undo_alias"
)

// Input is exactly what the model was shown, stored so a decision is
// reproducible without guessing at the state it was made against.
type Input struct {
	TreeVersion string     `json:"tree_version"`
	Existing    []string   `json:"existing"`
	Proposals   []Proposal `json:"proposals"`
}

// Report is what one run did, for an operator reading the log.
type Report struct {
	RunID     string   `json:"run_id"`
	DryRun    bool     `json:"dry_run"`
	Applied   bool     `json:"applied"`
	Additions int      `json:"additions"`
	Aliases   int      `json:"aliases"`
	Rejected  int      `json:"rejected"`
	Skipped   int      `json:"skipped"`
	Actions   []Action `json:"actions"`
}

type Curator struct {
	pool   *pgxpool.Pool
	judge  Judge
	logger *slog.Logger
}

func NewCurator(pool *pgxpool.Pool, judge Judge, logger *slog.Logger) *Curator {
	return &Curator{pool: pool, judge: judge, logger: logger}
}

// Curate runs one weekly curation. A dry run reads and decides but writes
// nothing at all, including no run record: the point of a dry run is to leave
// the database exactly as it was found.
func (c *Curator) Curate(ctx context.Context, dryRun bool) (Report, error) {
	tree, err := New(c.pool).ActiveTree(ctx)
	if err != nil {
		return Report{}, err
	}
	proposals, err := PendingProposals(ctx, c.pool)
	if err != nil {
		return Report{}, err
	}
	existing, err := existingNames(ctx, c.pool)
	if err != nil {
		return Report{}, err
	}

	if len(proposals) == 0 {
		c.logger.Info("taxonomy curation found nothing to decide", "tree_version", tree.Version)
		return Report{DryRun: dryRun}, nil
	}

	input := Input{TreeVersion: tree.Version, Proposals: proposals}
	for name := range existing {
		input.Existing = append(input.Existing, name)
	}

	decision, err := c.judge.Judge(ctx, curationPrompt(tree, proposals))
	if err != nil {
		// A failed run changes nothing. It is still recorded, so a week of
		// silence is distinguishable from a week of failures.
		c.record(ctx, input, Decision{}, Rollback{}, false)
		return Report{}, fmt.Errorf("%w: %v", ErrModelFailed, err)
	}

	actions := c.applyPolicy(decision.Actions, proposals, existing)
	report := summarize(actions)
	report.DryRun = dryRun

	if dryRun {
		c.logger.Info("taxonomy curation dry run",
			"additions", report.Additions, "aliases", report.Aliases,
			"rejected", report.Rejected, "skipped", report.Skipped)
		report.Actions = actions
		return report, nil
	}

	runID, err := c.apply(ctx, input, actions, proposals)
	if err != nil {
		return Report{}, err
	}

	report.RunID = runID
	report.Applied = report.Additions+report.Aliases+report.Rejected > 0
	report.Actions = actions
	c.logger.Info("taxonomy curation applied",
		"run_id", runID, "additions", report.Additions, "aliases", report.Aliases,
		"rejected", report.Rejected, "skipped", report.Skipped)
	return report, nil
}

// applyPolicy is where the thresholds live. The model advises; this decides.
// Policy is applied here rather than trusted to the prompt because a prompt
// cannot be tested and a threshold can.
func (c *Curator) applyPolicy(actions []Action, proposals []Proposal, existing map[string]string) []Action {
	counts := map[string]int{}
	known := map[string]bool{}
	for _, proposal := range proposals {
		counts[proposal.NormalizedName] = proposal.ContentCount
		known[proposal.NormalizedName] = true
	}

	additions := 0
	decided := make([]Action, 0, len(actions))
	for _, action := range actions {
		action.NormalizedName = Normalize(action.NormalizedName)

		switch {
		case !known[action.NormalizedName]:
			// The model answered about something nobody proposed. Ignoring it
			// is the only safe reading.
			action.Skipped = "not a pending proposal"

		case action.Verdict == VerdictReject:
			action.Applied = true

		case action.Verdict == VerdictAlias:
			target := action.AliasOf
			if id, ok := existing[Normalize(target)]; ok {
				action.CategoryID = id
				action.Applied = true
			} else {
				action.Skipped = "alias target is not an active category"
			}

		case action.Verdict != VerdictAdd:
			action.Skipped = "unknown verdict " + action.Verdict

		// From here the verdict is add, and every threshold must hold.
		case existing[action.NormalizedName] != "":
			// A duplicate is never an addition, whatever the model said.
			action.Skipped = "already an active category or alias"

		case counts[action.NormalizedName] < MinContentCount:
			action.Skipped = fmt.Sprintf("only %d of %d distinct proposals",
				counts[action.NormalizedName], MinContentCount)

		case action.Confidence < MinConfidence:
			action.Skipped = fmt.Sprintf("confidence %.2f below %.2f",
				action.Confidence, MinConfidence)

		case additions >= MaxAdditionsPerRun:
			action.Skipped = fmt.Sprintf("run already added %d categories", MaxAdditionsPerRun)

		default:
			action.Applied = true
			additions++
		}

		decided = append(decided, action)
	}
	return decided
}

// apply writes every applied action and its inverse in one transaction, so a
// half-applied run cannot exist.
func (c *Curator) apply(ctx context.Context, input Input, actions []Action, proposals []Proposal) (string, error) {
	byName := map[string][]string{}
	for _, proposal := range proposals {
		byName[proposal.NormalizedName] = proposal.IDs
	}

	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("starting the curation: %w", err)
	}
	defer tx.Rollback(ctx)

	rollback := Rollback{}
	for index, action := range actions {
		if !action.Applied {
			continue
		}
		ids := byName[action.NormalizedName]

		switch action.Verdict {
		case VerdictAdd:
			name := action.Name
			if name == "" {
				name = action.NormalizedName
			}
			var categoryID string
			if err := tx.QueryRow(ctx, `
				INSERT INTO reelpin.categories (name, normalized_name, description)
				VALUES ($1, $2, $3)
				RETURNING id::text`,
				name, action.NormalizedName, action.Description).Scan(&categoryID); err != nil {
				return "", fmt.Errorf("adding category %q: %w", name, err)
			}
			actions[index].CategoryID = categoryID
			if err := setProposals(ctx, tx, ids, "approved"); err != nil {
				return "", err
			}
			rollback.Actions = append(rollback.Actions, RollbackAction{
				Kind: undoAdd, CategoryID: categoryID, ProposalIDs: ids,
			})

		case VerdictAlias:
			if _, err := tx.Exec(ctx, `
				INSERT INTO reelpin.category_aliases (normalized_alias, category_id)
				VALUES ($1, $2)
				ON CONFLICT (normalized_alias) DO NOTHING`,
				action.NormalizedName, action.CategoryID); err != nil {
				return "", fmt.Errorf("aliasing %q: %w", action.NormalizedName, err)
			}
			if err := setProposals(ctx, tx, ids, "merged"); err != nil {
				return "", err
			}
			rollback.Actions = append(rollback.Actions, RollbackAction{
				Kind: undoAlias, NormalizedAlias: action.NormalizedName, ProposalIDs: ids,
			})

		case VerdictReject:
			if err := setProposals(ctx, tx, ids, "rejected"); err != nil {
				return "", err
			}
			rollback.Actions = append(rollback.Actions, RollbackAction{
				Kind: "undo_reject", ProposalIDs: ids,
			})
		}
	}

	runID, err := recordRun(ctx, tx, input, Decision{Actions: actions}, rollback, len(rollback.Actions) > 0)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("committing the curation: %w", err)
	}
	return runID, nil
}

// Rollback undoes one applied run and leaves its record in place. History is
// append-only: the rollback is a second fact about the run, not an erasure of
// the first.
func (c *Curator) Rollback(ctx context.Context, runID string) (int, error) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("starting the rollback: %w", err)
	}
	defer tx.Rollback(ctx)

	var raw []byte
	var applied bool
	err = tx.QueryRow(ctx, `
		SELECT rollback, applied FROM reelpin.taxonomy_runs WHERE id = $1 FOR UPDATE`,
		runID).Scan(&raw, &applied)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("no taxonomy run %s", runID)
	}
	if err != nil {
		return 0, fmt.Errorf("reading the run: %w", err)
	}
	if !applied {
		// Rolling back a run that changed nothing is a no-op, not an error:
		// an operator retrying is not a mistake worth failing.
		return 0, nil
	}

	var rollback Rollback
	if err := json.Unmarshal(raw, &rollback); err != nil {
		return 0, fmt.Errorf("reading the rollback record: %w", err)
	}

	undone := 0
	for _, action := range rollback.Actions {
		switch action.Kind {
		case undoAdd:
			// Deactivated, not deleted: content already filed against it must
			// keep resolving.
			if _, err := tx.Exec(ctx,
				`UPDATE reelpin.categories SET active = false WHERE id = $1`,
				action.CategoryID); err != nil {
				return 0, fmt.Errorf("deactivating %s: %w", action.CategoryID, err)
			}
		case undoAlias:
			if _, err := tx.Exec(ctx,
				`DELETE FROM reelpin.category_aliases WHERE normalized_alias = $1`,
				action.NormalizedAlias); err != nil {
				return 0, fmt.Errorf("removing alias %q: %w", action.NormalizedAlias, err)
			}
		}
		if err := setProposals(ctx, tx, action.ProposalIDs, "pending"); err != nil {
			return 0, err
		}
		undone++
	}

	if _, err := tx.Exec(ctx,
		`UPDATE reelpin.taxonomy_runs SET applied = false WHERE id = $1`, runID); err != nil {
		return 0, fmt.Errorf("marking the run rolled back: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("committing the rollback: %w", err)
	}
	return undone, nil
}

func setProposals(ctx context.Context, tx pgx.Tx, ids []string, status string) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx,
		`UPDATE reelpin.category_proposals SET status = $2 WHERE id = ANY($1)`,
		ids, status); err != nil {
		return fmt.Errorf("marking proposals %s: %w", status, err)
	}
	return nil
}

func recordRun(ctx context.Context, tx pgx.Tx, input Input, decision Decision, rollback Rollback, applied bool) (string, error) {
	inputJSON, err := json.Marshal(input)
	if err != nil {
		return "", fmt.Errorf("encoding the run input: %w", err)
	}
	decisionJSON, err := json.Marshal(decision)
	if err != nil {
		return "", fmt.Errorf("encoding the run decision: %w", err)
	}
	rollbackJSON, err := json.Marshal(rollback)
	if err != nil {
		return "", fmt.Errorf("encoding the run rollback: %w", err)
	}

	var runID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO reelpin.taxonomy_runs (model, prompt_version, input, decision, rollback, applied)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id::text`,
		CuratorModel, CuratorPromptVersion, inputJSON, decisionJSON, rollbackJSON, applied,
	).Scan(&runID); err != nil {
		return "", fmt.Errorf("recording the taxonomy run: %w", err)
	}
	return runID, nil
}

// record writes a run outside a transaction, for the failure path where there
// is nothing to apply.
func (c *Curator) record(ctx context.Context, input Input, decision Decision, rollback Rollback, applied bool) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		c.logger.Error("could not record the failed taxonomy run", "error", err)
		return
	}
	defer tx.Rollback(ctx)

	if _, err := recordRun(ctx, tx, input, decision, rollback, applied); err != nil {
		c.logger.Error("could not record the failed taxonomy run", "error", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		c.logger.Error("could not record the failed taxonomy run", "error", err)
	}
}

func summarize(actions []Action) Report {
	report := Report{}
	for _, action := range actions {
		switch {
		case !action.Applied:
			report.Skipped++
		case action.Verdict == VerdictAdd:
			report.Additions++
		case action.Verdict == VerdictAlias:
			report.Aliases++
		case action.Verdict == VerdictReject:
			report.Rejected++
		}
	}
	return report
}
