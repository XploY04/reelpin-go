package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/XploY04/reelpin-go/internal/ai"
	"github.com/XploY04/reelpin-go/internal/lease"
	"github.com/XploY04/reelpin-go/internal/queue"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var persistNamespace = uuid.MustParse("6ba7b812-9dad-11d1-80b4-00c04fd430c8")

// persist is the one transaction that makes a run real: the immutable content
// version, the current-version pointer, every subscriber's save, their job
// completions, the model's category proposal, and the notification and index
// events — all of it, or none of it.
func (p *Pipeline) persist(ctx context.Context, state *run) error {
	tx, err := p.deps.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("starting the persist: %w", err)
	}
	defer tx.Rollback(ctx)

	err = lease.GuardedExec(ctx, tx, state.Lease, func(guarded pgx.Tx) error {
		// The run's own status is the idempotency anchor: a redelivered
		// message after a committed persist finds it completed and stops.
		var status string
		if err := guarded.QueryRow(ctx, `
			SELECT status FROM reelpin.processing_runs WHERE id = $1 FOR UPDATE`,
			state.ID).Scan(&status); err != nil {
			return err
		}
		if status == "completed" {
			return nil
		}

		versionID, err := p.insertVersion(ctx, guarded, state)
		if err != nil {
			return err
		}

		// Reads move to the new version only here, atomically with everything
		// else: until this commit, they stay on the prior version.
		if _, err := guarded.Exec(ctx, `
			UPDATE reelpin.contents SET current_version_id = $2, updated_at = now() WHERE id = $1`,
			state.ContentID, versionID); err != nil {
			return fmt.Errorf("moving the current version: %w", err)
		}

		if err := p.completeSubscribers(ctx, guarded, state); err != nil {
			return err
		}

		if state.Category.Proposal != nil {
			if err := p.fileProposal(ctx, guarded, state); err != nil {
				return err
			}
		}

		// Indexing is separate durable light work: a search-index failure can
		// never turn a completed save into a failed job.
		indexID := uuid.NewSHA1(persistNamespace, []byte("index:"+versionID)).String()
		indexPayload := fmt.Sprintf(`{"run_id":%q,"dispatch_generation":%d}`, state.ID, state.Lease.Generation)
		if _, err := guarded.Exec(ctx, `
			INSERT INTO reelpin.outbox_events (event_id, event_type, routing_key, schema_version, payload)
			VALUES ($1, 'content.index', $2, 1, $3::jsonb)
			ON CONFLICT (event_id) DO NOTHING`,
			indexID, queue.QueueLight, indexPayload); err != nil {
			return fmt.Errorf("writing the index event: %w", err)
		}

		_, err = guarded.Exec(ctx, `
			UPDATE reelpin.processing_runs
			SET status = 'completed', stage = 'persist',
			    lease_owner = NULL, lease_expires_at = NULL, updated_at = now()
			WHERE id = $1`, state.ID)
		return err
	})
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (p *Pipeline) insertVersion(ctx context.Context, tx pgx.Tx, state *run) (string, error) {
	// The selected category rides the versioned extraction JSONB: it is
	// model-versioned data. It gets a typed column with the task that first
	// queries it canonically; the legacy adapter serves category reads until
	// then.
	raw := map[string]any{
		"extraction":  state.Extraction,
		"category":    state.Category.Category,
		"subcategory": state.Category.Subcategory,
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return "", fmt.Errorf("encoding the extraction: %w", err)
	}

	media := map[string]any{"thumbnail_url": state.Prepared.ThumbnailURL}
	mediaEncoded, err := json.Marshal(media)
	if err != nil {
		return "", fmt.Errorf("encoding media metadata: %w", err)
	}

	var versionID string
	err = tx.QueryRow(ctx, `
		INSERT INTO reelpin.content_versions
			(content_id, processor_version, prompt_version, schema_version, model_version,
			 title, summary, caption, transcript, tags, key_facts, raw_extraction, media)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12::jsonb, $13::jsonb)
		RETURNING id::text`,
		state.ContentID, ProcessorVersion, ai.PromptVersion, ai.SchemaVersion, p.deps.ModelVersion,
		state.Extraction.Title, state.Extraction.Summary, state.Prepared.Caption, state.Transcript,
		state.Extraction.TopicalTags, state.Extraction.KeyFacts, string(encoded), string(mediaEncoded),
	).Scan(&versionID)
	if err != nil {
		return "", fmt.Errorf("inserting the content version: %w", err)
	}
	return versionID, nil
}

// completeSubscribers creates one save per subscriber job and completes the
// job pointing at it. Two users sharing one link each get their own save from
// this one run.
func (p *Pipeline) completeSubscribers(ctx context.Context, tx pgx.Tx, state *run) error {
	rows, err := tx.Query(ctx, `
		SELECT id::text, user_id::text, url, normalized_url, collection_ids::text[]
		FROM reelpin.processing_jobs
		WHERE run_id = $1 AND status IN ('queued', 'processing')
		FOR UPDATE`, state.ID)
	if err != nil {
		return fmt.Errorf("finding subscriber jobs: %w", err)
	}
	type subscriber struct {
		jobID, userID string
		url           string
		normalizedURL string
		collectionIDs []string
	}
	subscribers := []subscriber{}
	for rows.Next() {
		var s subscriber
		if err := rows.Scan(&s.jobID, &s.userID, &s.url, &s.normalizedURL, &s.collectionIDs); err != nil {
			rows.Close()
			return fmt.Errorf("reading a subscriber: %w", err)
		}
		subscribers = append(subscribers, s)
	}
	rows.Close()
	if rows.Err() != nil {
		return fmt.Errorf("finding subscriber jobs: %w", rows.Err())
	}

	for _, s := range subscribers {
		var saveID string
		var savedAt time.Time
		// A user who already saved this content keeps their save and its id.
		err := tx.QueryRow(ctx, `
			INSERT INTO reelpin.user_saves (user_id, content_id)
			VALUES ($1, $2)
			ON CONFLICT (user_id, content_id) DO UPDATE SET content_id = EXCLUDED.content_id
			RETURNING id::text, saved_at`,
			s.userID, state.ContentID).Scan(&saveID, &savedAt)
		if err != nil {
			return fmt.Errorf("creating the save for job %s: %w", s.jobID, err)
		}

		// The legacy reel is what the app actually reads until Task 33 moves
		// the reader to user_saves, so it is not a best-effort copy: a failure
		// here rolls the whole persist back rather than completing a job whose
		// id resolves to nothing. Nothing durable is lost by that. Every
		// finished stage is checkpointed, so the retry replays the persist
		// without paying a provider again, and it is the only failure mode
		// that cannot leave the two halves disagreeing.
		if err := writeLegacyReel(ctx, tx, state, legacySave{
			ID:            saveID,
			UserID:        s.userID,
			URL:           s.url,
			NormalizedURL: s.normalizedURL,
			SavedAt:       savedAt,
		}); err != nil {
			return fmt.Errorf("mirroring the save for job %s: %w", s.jobID, err)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE reelpin.processing_jobs
			SET status = 'completed', user_save_id = $2, progress_percent = 100,
			    current_step = 'completed', completed_at = now(), updated_at = now()
			WHERE id = $1`, s.jobID, saveID); err != nil {
			return fmt.Errorf("completing job %s: %w", s.jobID, err)
		}

		if err := fileIntoCollections(ctx, tx, s.userID, saveID, s.collectionIDs); err != nil {
			return fmt.Errorf("filing job %s into its collections: %w", s.jobID, err)
		}

		notifyID := uuid.NewSHA1(persistNamespace, []byte("notify:"+s.jobID)).String()
		payload := fmt.Sprintf(`{"run_id":%q,"dispatch_generation":%d}`, state.ID, state.Lease.Generation)
		if _, err := tx.Exec(ctx, `
			INSERT INTO reelpin.outbox_events (event_id, event_type, routing_key, schema_version, payload)
			VALUES ($1, $2, $3, 1, $4::jsonb)
			ON CONFLICT (event_id) DO NOTHING`,
			notifyID, queue.EventNotification, queue.QueueNotify, payload); err != nil {
			return fmt.Errorf("writing the notification event for job %s: %w", s.jobID, err)
		}
	}
	return nil
}

// fileIntoCollections honours the filing intent the submission recorded on the
// job, now that the save it named exists. It runs in the persist transaction,
// so a save and its filing commit together. A collection deleted, or a
// membership dropped, between submission and completion files nothing rather
// than failing: a finished save must never become a failed job over a
// collection that is gone. The item primary key absorbs a redelivery.
func fileIntoCollections(ctx context.Context, tx pgx.Tx, userID, saveID string, collectionIDs []string) error {
	if len(collectionIDs) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
		WITH filed AS (
			INSERT INTO reelpin.collection_items (collection_id, save_id, added_by)
			SELECT c.id, $2::uuid, $3::uuid
			FROM reelpin.collections c
			WHERE c.id = ANY($1::uuid[])
			  AND (c.owner_id = $3::uuid OR EXISTS (
				SELECT 1 FROM reelpin.collection_members m
				WHERE m.collection_id = c.id AND m.user_id = $3::uuid AND m.role = 'editor'))
			ON CONFLICT (collection_id, save_id) DO NOTHING
			RETURNING collection_id
		)
		UPDATE reelpin.collections SET updated_at = now()
		WHERE id IN (SELECT collection_id FROM filed)`,
		collectionIDs, saveID, userID)
	if err != nil {
		return fmt.Errorf("filing the save into its collections: %w", err)
	}
	return nil
}

// fileProposal records what the model wished existed. A processing job can
// propose a category; only the weekly curator can activate one.
func (p *Pipeline) fileProposal(ctx context.Context, tx pgx.Tx, state *run) error {
	proposal := state.Category.Proposal
	_, err := tx.Exec(ctx, `
		INSERT INTO reelpin.category_proposals (proposed_name, normalized_name, description, source_run_id)
		VALUES ($1, lower(trim($1)), $2, $3)`,
		proposal.Name, proposal.Description, state.ID)
	if err != nil {
		return fmt.Errorf("filing the category proposal: %w", err)
	}
	return nil
}
