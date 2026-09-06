package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// coexistence is the one switch for everything in this file. Flutter is still
// served by Python, and the Go read path resolves `reels` to public.reels, so
// a run that wrote only canonical rows would produce a save the app cannot
// open. Task 33 moves the reader to reelpin.user_saves; turning this off is
// what retires the legacy write, and this file goes with it.
const coexistence = true

// legacySave is one subscriber's half of the persist: the canonical save, and
// the submission it came from.
type legacySave struct {
	ID            string
	UserID        string
	URL           string
	NormalizedURL string
	SavedAt       time.Time
}

// writeLegacyReel mirrors one canonical save into public.reels, the table
// Python writes and the reel reader still reads. It is not a copy that can
// lag: it runs inside the persist transaction, so the two rows exist together
// or neither does.
//
// The id is the canonical save's id, never a new one. Deep links, app caches
// and the backfill all hold that id, and the read path resolves by it.
//
// Supabase owns this table, so no migration here creates or alters it. The
// columns are the ones internal/postgres/reels.go selects; ingestion_method
// and transcript_source stay null because the Go pipeline has no equivalent of
// the Python handler labels, and inventing one would be worse than the null
// Python itself writes when it has no metadata.
func writeLegacyReel(ctx context.Context, tx pgx.Tx, state *run, save legacySave) error {
	if !coexistence {
		return nil
	}

	tags, err := jsonArray(state.Extraction.TopicalTags)
	if err != nil {
		return err
	}
	keyFacts, err := jsonArray(state.Extraction.KeyFacts)
	if err != nil {
		return err
	}
	locations, err := jsonArray(state.Extraction.Locations)
	if err != nil {
		return err
	}
	people, err := jsonArray(state.Extraction.PeopleMentioned)
	if err != nil {
		return err
	}
	actions, err := jsonArray(state.Extraction.ActionableItems)
	if err != nil {
		return err
	}
	events, err := jsonArray(state.Extraction.Events)
	if err != nil {
		return err
	}

	// A user who already had this content keeps their row, exactly as the
	// canonical save keeps theirs: the primary key absorbs a redelivery.
	_, err = tx.Exec(ctx, `
		INSERT INTO public.reels
			(id, user_id, url, normalized_url, source_platform, source_content_type,
			 source_content_id, processing_version, thumbnail_url, title, summary,
			 transcript, category, subcategory, secondary_categories, key_facts,
			 locations, people_mentioned, actionable_items, events, parse_status, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8, NULLIF($9, ''), $10, $11,
		        $12, $13, $14, $15::jsonb, $16::jsonb, $17::jsonb, $18::jsonb,
		        $19::jsonb, $20::jsonb, 'parsed', $21)
		ON CONFLICT (id) DO NOTHING`,
		save.ID, save.UserID, save.URL, save.NormalizedURL,
		state.Identity.Platform, state.Identity.ContentType, state.Identity.ContentID,
		ProcessorVersion, state.Prepared.ThumbnailURL,
		state.Extraction.Title, state.Extraction.Summary, state.Transcript,
		state.Category.Category, state.Category.Subcategory,
		tags, keyFacts, locations, people, actions, events, save.SavedAt)
	if err != nil {
		return fmt.Errorf("writing the legacy reel: %w", err)
	}
	return nil
}

// jsonArray encodes one extraction list for a legacy JSONB column. A nil slice
// becomes an empty array rather than a JSON null: Python reads these columns
// and expects a list.
func jsonArray(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encoding a legacy reel field: %w", err)
	}
	if string(encoded) == "null" {
		return "[]", nil
	}
	return string(encoded), nil
}
