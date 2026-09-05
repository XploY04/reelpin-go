package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/XploY04/reelpin-go/internal/ai"
	"github.com/XploY04/reelpin-go/internal/enqueue"
	"github.com/XploY04/reelpin-go/internal/outbox"
)

// jobSubscriber is one private job waiting on this run.
type jobSubscriber struct {
	JobID         string
	UserID        string
	CollectionIDs []string
}

// persistContent writes the shared half: one content version everybody's save
// points at, plus its locations and transcript chunks.
func (p *Pipeline) persistContent(ctx context.Context, state *run) error {
	hash := InputHash(ai.SchemaVersion, state.Transcript, state.Caption, state.Extraction.Title)

	var cachedVersion string
	if found, err := p.checkpoints.Load(ctx, state.ID, StagePersistContent, hash, &cachedVersion); err != nil {
		return err
	} else if found && cachedVersion != "" {
		state.VersionID = cachedVersion
		return nil
	}

	structured, err := json.Marshal(state.Extraction)
	if err != nil {
		return fmt.Errorf("encoding the extraction: %w", err)
	}

	transaction, err := p.deps.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("starting the content transaction: %w", err)
	}
	defer transaction.Rollback(ctx)

	// One version per content and processor version, so a redelivered message
	// updates the row it wrote instead of adding a second one.
	var versionID string
	if err := transaction.QueryRow(ctx, `
		INSERT INTO reelpin.content_versions
			(content_id, processor_version, extraction_schema_version, transcript, caption,
			 title, summary, structured, thumbnail_url, parse_status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'parsed')
		ON CONFLICT (content_id, processor_version)
		DO UPDATE SET transcript = EXCLUDED.transcript,
		              caption = EXCLUDED.caption,
		              title = EXCLUDED.title,
		              summary = EXCLUDED.summary,
		              structured = EXCLUDED.structured,
		              thumbnail_url = EXCLUDED.thumbnail_url,
		              parse_status = 'parsed',
		              updated_at = now()
		RETURNING id::text`,
		state.ContentID, enqueue.ProcessorVersion, ai.SchemaVersion,
		state.Transcript, state.Caption, state.Extraction.Title, state.Extraction.Summary,
		structured, nullable(state.Prepared.ThumbnailURL),
	).Scan(&versionID); err != nil {
		return fmt.Errorf("writing the content version: %w", err)
	}

	// Locations and chunks are replaced wholesale: they are derived data, and
	// half-updated derived data is worse than rebuilt derived data.
	if _, err := transaction.Exec(ctx,
		`DELETE FROM reelpin.content_locations WHERE content_version_id = $1`, versionID); err != nil {
		return fmt.Errorf("clearing old locations: %w", err)
	}
	for index, location := range state.Extraction.Locations {
		if location.Latitude == nil || location.Longitude == nil {
			// Without coordinates it is a fact, not a map pin.
			continue
		}
		if _, err := transaction.Exec(ctx, `
			INSERT INTO reelpin.content_locations
				(content_version_id, ordinal, name, address, neighborhood, city, state, country,
				 geog, mention_source, display_label)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8,
			        ST_SetSRID(ST_MakePoint($9, $10), 4326)::geography, 'extraction', $11)`,
			versionID, index, location.Name, nullable(location.Address()),
			nullable(location.Neighborhood), nullable(location.City),
			nullable(location.State), nullable(location.Country),
			*location.Longitude, *location.Latitude, location.Name,
		); err != nil {
			return fmt.Errorf("writing a location: %w", err)
		}
	}

	if _, err := transaction.Exec(ctx,
		`DELETE FROM reelpin.content_chunks WHERE content_version_id = $1`, versionID); err != nil {
		return fmt.Errorf("clearing old chunks: %w", err)
	}
	for index, chunk := range Chunk(state.Transcript) {
		if _, err := transaction.Exec(ctx, `
			INSERT INTO reelpin.content_chunks (content_version_id, ordinal, chunk_text, content_hash)
			VALUES ($1, $2, $3, $4)`,
			versionID, index, chunk, InputHash(chunk),
		); err != nil {
			return fmt.Errorf("writing a chunk: %w", err)
		}
	}

	if _, err := transaction.Exec(ctx,
		`UPDATE reelpin.contents SET current_content_version_id = $2, updated_at = now() WHERE id = $1`,
		state.ContentID, versionID,
	); err != nil {
		return fmt.Errorf("pointing content at its version: %w", err)
	}

	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf("committing the content: %w", err)
	}

	state.VersionID = versionID
	return p.checkpoints.Save(ctx, state.ID, StagePersistContent, hash, versionID)
}

// personalize files the content for every user waiting on this run. The
// category is per user, so it is asked for per user, and cached per user.
func (p *Pipeline) personalize(ctx context.Context, state *run) error {
	subscribers, err := p.subscribers(ctx, state.ID)
	if err != nil {
		return err
	}

	for _, subscriber := range subscribers {
		hash := InputHash(subscriber.UserID, state.VersionID, ai.SchemaVersion)

		var cached ai.Category
		stage := StagePersonalize + ":" + subscriber.UserID
		if found, err := p.checkpoints.Load(ctx, state.ID, stage, hash, &cached); err != nil {
			return err
		} else if found {
			continue
		}

		existing, err := p.existingCategories(ctx, subscriber.UserID)
		if err != nil {
			return err
		}
		category, err := p.deps.Categorizer.Categorize(ctx, state.Extraction, existing)
		if err != nil {
			return Classify(err)
		}
		if err := p.checkpoints.Save(ctx, state.ID, stage, hash, ai.NormalizeCategory(category)); err != nil {
			return err
		}
	}
	return nil
}

// save writes each user's reel and links their job to it. It is the last write
// that matters to the app, and it is keyed so redelivery cannot duplicate it.
func (p *Pipeline) save(ctx context.Context, state *run) error {
	subscribers, err := p.subscribers(ctx, state.ID)
	if err != nil {
		return err
	}

	for _, subscriber := range subscribers {
		var category ai.Category
		stage := StagePersonalize + ":" + subscriber.UserID
		hash := InputHash(subscriber.UserID, state.VersionID, ai.SchemaVersion)
		if _, err := p.checkpoints.Load(ctx, state.ID, stage, hash, &category); err != nil {
			return err
		}
		category = ai.NormalizeCategory(category)

		if err := p.saveOne(ctx, state, subscriber, category); err != nil {
			return err
		}
	}
	return nil
}

func (p *Pipeline) saveOne(ctx context.Context, state *run, subscriber jobSubscriber, category ai.Category) error {
	transaction, err := p.deps.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("starting the save transaction: %w", err)
	}
	defer transaction.Rollback(ctx)

	// A job that already carries a reel was saved by an earlier delivery.
	var existing *string
	if err := transaction.QueryRow(ctx,
		`SELECT result_reel_id::text FROM public.processing_jobs WHERE id = $1 FOR UPDATE`,
		subscriber.JobID,
	).Scan(&existing); err != nil {
		return fmt.Errorf("locking the job: %w", err)
	}
	if existing != nil && *existing != "" {
		return transaction.Commit(ctx)
	}

	facts, _ := json.Marshal(state.Extraction.KeyFacts)
	people, _ := json.Marshal(state.Extraction.PeopleMentioned)
	actions, _ := json.Marshal(state.Extraction.ActionableItems)
	events, _ := json.Marshal(state.Extraction.Events)
	secondary, _ := json.Marshal(category.SecondaryCategories)
	locations, err := json.Marshal(displayLocations(state.Extraction.Locations))
	if err != nil {
		return fmt.Errorf("encoding locations: %w", err)
	}

	var reelID string
	if err := transaction.QueryRow(ctx, `
		INSERT INTO public.reels
			(user_id, url, normalized_url, source_platform, source_content_type, source_content_id,
			 processing_version, ingestion_method, transcript_source, thumbnail_url,
			 title, summary, transcript, category, subcategory, secondary_categories,
			 key_facts, locations, people_mentioned, actionable_items, events,
			 parse_status, content_version_id)
		VALUES ($1, $2, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15,
		        $16, $17, $18, $19, $20, 'parsed', $21)
		RETURNING id::text`,
		subscriber.UserID, state.Identity.NormalizedURL, state.Identity.Platform,
		state.Identity.ContentType, nullable(state.Identity.ContentID),
		enqueue.ProcessorVersion, nullable(state.Prepared.IngestionMethod),
		nullable(state.Prepared.TranscriptSource), nullable(state.Prepared.ThumbnailURL),
		state.Extraction.Title, state.Extraction.Summary, state.Transcript,
		category.Category, category.Subcategory, secondary,
		facts, locations, people, actions, events, state.VersionID,
	).Scan(&reelID); err != nil {
		return fmt.Errorf("saving the reel: %w", err)
	}

	if _, err := transaction.Exec(ctx, `
		UPDATE public.processing_jobs
		SET result_reel_id = $2, status = 'completed', current_step = 'completed',
		    progress_percent = 100, completed_at = now(), updated_at = now()
		WHERE id = $1`,
		subscriber.JobID, reelID,
	); err != nil {
		return fmt.Errorf("completing the job: %w", err)
	}

	// The reel is saved; filing it and telling the user are separate work that
	// must not be able to undo it.
	if err := outbox.Insert(ctx, transaction, outbox.Event{
		Environment: p.deps.Environment,
		EventID:     deterministicEventID(state.ID, subscriber.JobID, "save"),
		EventType:   "reel.saved",
		RoutingKey:  "reelpin.notifications",
		Payload: map[string]any{
			"run_id":         state.ID,
			"platform":       state.Identity.Platform,
			"job_id":         subscriber.JobID,
			"reel_id":        reelID,
			"user_id":        subscriber.UserID,
			"collection_ids": subscriber.CollectionIDs,
		},
	}); err != nil {
		return err
	}

	return transaction.Commit(ctx)
}

// emit hands follow-up work to the outbox. Indexing and notifications must
// never be able to repeat a download or a save, so they are separate messages.
func (p *Pipeline) emit(ctx context.Context, state *run, eventType, routingKey string) error {
	hash := InputHash(eventType, state.VersionID)

	if found, err := p.checkpoints.Load(ctx, state.ID, eventType, hash, nil); err != nil {
		return err
	} else if found {
		return nil
	}

	transaction, err := p.deps.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("starting the emit transaction: %w", err)
	}
	defer transaction.Rollback(ctx)

	if err := outbox.Insert(ctx, transaction, outbox.Event{
		Environment: p.deps.Environment,
		EventID:     deterministicEventID(state.ID, state.VersionID, eventType),
		EventType:   eventType,
		RoutingKey:  routingKey,
		Payload: map[string]any{
			"run_id":             state.ID,
			"platform":           state.Identity.Platform,
			"content_id":         state.ContentID,
			"content_version_id": state.VersionID,
		},
	}); err != nil {
		return err
	}
	if err := transaction.Commit(ctx); err != nil {
		return fmt.Errorf("committing the emit: %w", err)
	}

	return p.checkpoints.Save(ctx, state.ID, eventType, hash, true)
}

func (p *Pipeline) complete(ctx context.Context, state *run) error {
	if _, err := p.deps.Pool.Exec(ctx, `
		UPDATE reelpin.processing_runs
		SET status = 'completed', stage = 'complete', progress_percent = 100,
		    lease_owner = NULL, lease_expires_at = NULL,
		    completed_at = now(), updated_at = now()
		WHERE id = $1`, state.ID,
	); err != nil {
		return fmt.Errorf("completing the run: %w", err)
	}
	return nil
}

func (p *Pipeline) subscribers(ctx context.Context, runID string) ([]jobSubscriber, error) {
	rows, err := p.deps.Pool.Query(ctx, `
		SELECT id::text, user_id, collection_ids
		FROM public.processing_jobs
		WHERE processing_run_id = $1
		ORDER BY created_at`, runID)
	if err != nil {
		return nil, fmt.Errorf("reading job subscribers: %w", err)
	}
	defer rows.Close()

	var subscribers []jobSubscriber
	for rows.Next() {
		var subscriber jobSubscriber
		var raw []byte
		if err := rows.Scan(&subscriber.JobID, &subscriber.UserID, &raw); err != nil {
			return nil, fmt.Errorf("reading job subscribers: %w", err)
		}
		if len(raw) > 0 && string(raw) != "null" {
			_ = json.Unmarshal(raw, &subscriber.CollectionIDs)
		}
		subscribers = append(subscribers, subscriber)
	}
	return subscribers, rows.Err()
}

// existingCategories is the tree the user already uses, so the model reuses a
// label instead of inventing a synonym.
func (p *Pipeline) existingCategories(ctx context.Context, userID string) ([]string, error) {
	rows, err := p.deps.Pool.Query(ctx, `
		SELECT DISTINCT category || ' > ' || subcategory
		FROM public.reels
		WHERE user_id = $1 AND category IS NOT NULL AND category <> ''
		ORDER BY 1
		LIMIT 100`, userID)
	if err != nil {
		return nil, fmt.Errorf("reading existing categories: %w", err)
	}
	defer rows.Close()

	var labels []string
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			return nil, fmt.Errorf("reading existing categories: %w", err)
		}
		labels = append(labels, label)
	}
	return labels, rows.Err()
}

// displayLocations shapes the extraction's places the way the app already reads
// them from a reel row.
func displayLocations(locations []ai.Location) []map[string]any {
	shaped := make([]map[string]any, 0, len(locations))
	for _, location := range locations {
		entry := map[string]any{
			"name":         location.Name,
			"address":      location.Address(),
			"neighborhood": location.Neighborhood,
			"city":         location.City,
			"state":        location.State,
			"country":      location.Country,
		}
		if location.Latitude != nil && location.Longitude != nil {
			entry["latitude"] = *location.Latitude
			entry["longitude"] = *location.Longitude
		}
		shaped = append(shaped, entry)
	}
	return shaped
}

// deterministicEventID makes redelivery write the same event id, so the outbox
// primary key collapses duplicates instead of sending twice.
func deterministicEventID(parts ...string) string {
	sum := InputHash(parts...)
	return strings.Join([]string{sum[0:8], sum[8:12], "4" + sum[13:16], "8" + sum[17:20], sum[20:32]}, "-")
}

func nullable(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}
