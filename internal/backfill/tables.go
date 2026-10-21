package backfill

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// TableReport counts one mapped legacy table. Like Report, it carries no
// content and no user data.
type TableReport struct {
	// Absent is a legacy table this source does not have, which is a fact
	// about the project rather than a failure: reminders and price_tracks
	// were never promoted to every Supabase project.
	Absent          bool           `json:"absent"`
	Scanned         int            `json:"scanned"`
	Copied          int            `json:"copied"`
	Skipped         int            `json:"skipped"`
	SkippedByAction map[string]int `json:"skipped_by_action"`
}

// tableMapping carries one legacy table into its canonical table with a single
// INSERT ... SELECT per row. The mapping is SQL rather than Go so a row is
// never decoded and re-encoded on the way through, and a value the canonical
// schema rejects is rejected by the constraint that names why.
type tableMapping struct {
	name string
	// key is the legacy primary key column. It becomes the canonical id through
	// keyExpression, so a rerun lands on the row it wrote before.
	key string
	// owner is the legacy column naming the user, or "" for a table nobody owns.
	owner     string
	canonical string
	// canonicalKey is the canonical column the derived key lands in, or "" when
	// the canonical row has no single id (a pin preference is keyed by user
	// and location).
	canonicalKey string
	// changed is when a legacy row last moved, for --since. nil means the
	// table has no stamp Python keeps current, so it is copied whole on every
	// run, with no cursor and no --since filter.
	changed func(tableShape) string
	// insert is the statement, given the source table aliased l and the
	// predicate that picks one row.
	insert func(shape tableShape, from, where string) string
	// unmatched is the action recorded when the statement selects nothing
	// because a joined canonical row is missing. Only a mapping that joins
	// one sets it.
	unmatched string
	// owned replaces the verifier's default ownership check, a predicate over
	// the legacy row l, for a canonical row with no single id.
	owned string
}

// wave1Mappings run after the reels and jobs, because reminders, price
// tracks and hidden pins point at saves the reel pass creates.
var wave1Mappings = []tableMapping{
	{
		// synced_at is set on insert and nothing moves it: Pro is granted and
		// extended by hand in the SQL editor, so a changed pro_expires_at
		// carries no stamp. The table is a row per paying user, so it is
		// copied whole every run.
		name:         "user_entitlements",
		key:          "user_id",
		owner:        "user_id",
		canonical:    "reelpin.user_entitlements",
		canonicalKey: "user_id",
		insert: func(s tableShape, from, where string) string {
			return `
				INSERT INTO reelpin.user_entitlements
					(user_id, pro_expires_at, product_id, store, is_sandbox, synced_at, note)
				SELECT ` + uuidOrNull("l.user_id") + `, ` + s.col("pro_expires_at") + `,
				       ` + s.col("product_id") + `, ` + s.col("store") + `,
				       coalesce(` + s.col("is_sandbox") + `, false), ` + s.col("synced_at") + `,
				       ` + s.col("note") + `
				FROM ` + from + ` WHERE ` + where + `
				ON CONFLICT (user_id) DO UPDATE SET
					pro_expires_at = EXCLUDED.pro_expires_at,
					product_id = EXCLUDED.product_id,
					store = EXCLUDED.store,
					is_sandbox = EXCLUDED.is_sandbox,
					synced_at = EXCLUDED.synced_at,
					note = EXCLUDED.note`
		},
	},
	{
		name:         "profiles",
		key:          "id",
		owner:        "id",
		canonical:    "reelpin.profiles",
		canonicalKey: "user_id",
		changed:      tableShape.changed,
		insert: func(s tableShape, from, where string) string {
			// A profile the user edited on the new app since the snapshot is
			// newer than the legacy row and keeps its values.
			return `
				INSERT INTO reelpin.profiles AS p
					(user_id, email, full_name, avatar_url, created_at, updated_at)
				SELECT ` + uuidOrNull("l.id") + `, ` + s.col("email") + `,
				       ` + s.col("full_name") + `, ` + s.col("avatar_url") + `,
				       coalesce(` + s.col("created_at") + `, ` + s.col("updated_at") + `, now()),
				       coalesce(` + s.col("updated_at") + `, ` + s.col("created_at") + `, now())
				FROM ` + from + ` WHERE ` + where + `
				ON CONFLICT (user_id) DO UPDATE SET
					email = EXCLUDED.email,
					full_name = EXCLUDED.full_name,
					avatar_url = EXCLUDED.avatar_url,
					updated_at = EXCLUDED.updated_at
				WHERE p.updated_at <= EXCLUDED.updated_at`
		},
	},
	{
		name:         "manual_map_pins",
		key:          "id",
		owner:        "user_id",
		canonical:    "reelpin.user_manual_pins",
		canonicalKey: "id",
		changed:      tableShape.changed,
		insert: func(s tableShape, from, where string) string {
			return `
				INSERT INTO reelpin.user_manual_pins
					(id, user_id, name, address, point, google_place_id, google_maps_url,
					 category, subcategory, secondary_categories, place_types, metadata, created_at)
				SELECT l.id, ` + uuidOrNull("l.user_id") + `, coalesce(` + s.col("name") + `, ''),
				       nullif(` + s.col("address") + `, ''),
				       ST_MakePoint(l.longitude, l.latitude)::geography,
				       ` + s.col("google_place_id") + `, ` + s.col("google_maps_url") + `,
				       ` + s.col("category") + `, ` + s.col("subcategory") + `,
				       ` + textArray(s.col("secondary_categories")) + `,
				       ` + textArray(s.col("place_types")) + `,
				       coalesce(` + s.col("metadata") + `, '{}'::jsonb),
				       coalesce(` + s.col("created_at") + `, now())
				FROM ` + from + ` WHERE ` + where + `
				ON CONFLICT (id) DO UPDATE SET
					name = EXCLUDED.name,
					address = EXCLUDED.address,
					point = EXCLUDED.point,
					google_place_id = EXCLUDED.google_place_id,
					google_maps_url = EXCLUDED.google_maps_url,
					category = EXCLUDED.category,
					subcategory = EXCLUDED.subcategory,
					secondary_categories = EXCLUDED.secondary_categories,
					place_types = EXCLUDED.place_types,
					metadata = EXCLUDED.metadata`
		},
	},
	{
		// A hidden pin names a reel and the index of a location on it. The
		// canonical preference names the location row, so the pin carries over
		// only once that location exists for the save the reel became.
		name:      "hidden_reel_map_pins",
		key:       "id",
		owner:     "user_id",
		canonical: "reelpin.user_pin_preferences",
		changed:   func(s tableShape) string { return latest(s, "hidden_at", "created_at") },
		insert: func(s tableShape, from, where string) string {
			return `
				INSERT INTO reelpin.user_pin_preferences (user_id, location_id, hidden, created_at)
				SELECT s.user_id, cl.id, true,
				       coalesce(` + s.col("hidden_at") + `, ` + s.col("created_at") + `, now())
				FROM ` + from + hiddenPinJoin + ` WHERE ` + where + `
				ON CONFLICT (user_id, location_id) DO UPDATE SET hidden = true`
		},
		unmatched: "skipped_no_location",
		owned: `EXISTS (SELECT 1 FROM reelpin.user_saves s` + hiddenPinPreference + `
			WHERE s.id = l.reel_id AND s.user_id::text = lower(l.user_id::text))`,
	},
	{
		name:         "reminders",
		key:          "id",
		owner:        "user_id",
		canonical:    "reelpin.reminders",
		canonicalKey: "id",
		changed:      func(s tableShape) string { return latest(s, "created_at", "sent_at") },
		insert: func(s tableShape, from, where string) string {
			return `
				INSERT INTO reelpin.reminders
					(id, user_id, save_id, title, body, fire_at, status, created_at, sent_at)
				SELECT l.id, ` + uuidOrNull("l.user_id") + `, s.id, l.title,
				       coalesce(` + s.col("body") + `, ''), l.fire_at,
				       coalesce(` + s.col("status") + `, 'pending'),
				       coalesce(` + s.col("created_at") + `, now()), ` + s.col("sent_at") + `
				FROM ` + from + saveJoin + ` WHERE ` + where + `
				ON CONFLICT (id) DO UPDATE SET
					save_id = EXCLUDED.save_id,
					title = EXCLUDED.title,
					body = EXCLUDED.body,
					fire_at = EXCLUDED.fire_at,
					status = EXCLUDED.status,
					sent_at = EXCLUDED.sent_at`
		},
	},
	{
		name:         "price_tracks",
		key:          "id",
		owner:        "user_id",
		canonical:    "reelpin.price_tracks",
		canonicalKey: "id",
		changed: func(s tableShape) string {
			return latest(s, "created_at", "last_checked_at", "notified_at")
		},
		insert: func(s tableShape, from, where string) string {
			return `
				INSERT INTO reelpin.price_tracks
					(id, user_id, save_id, url, title, target_price, currency, last_price,
					 active, last_checked_at, notified_at, created_at)
				SELECT l.id, ` + uuidOrNull("l.user_id") + `, s.id, l.url,
				       coalesce(` + s.col("title") + `, ''), l.target_price,
				       coalesce(` + s.col("currency") + `, ''), ` + s.col("last_price") + `,
				       coalesce(` + s.col("active") + `, true), ` + s.col("last_checked_at") + `,
				       ` + s.col("notified_at") + `, coalesce(` + s.col("created_at") + `, now())
				FROM ` + from + saveJoin + ` WHERE ` + where + `
				ON CONFLICT (id) DO UPDATE SET
					save_id = EXCLUDED.save_id,
					url = EXCLUDED.url,
					title = EXCLUDED.title,
					target_price = EXCLUDED.target_price,
					currency = EXCLUDED.currency,
					last_price = EXCLUDED.last_price,
					active = EXCLUDED.active,
					last_checked_at = EXCLUDED.last_checked_at,
					notified_at = EXCLUDED.notified_at`
		},
	},
	{
		name:         "feedback",
		key:          "id",
		canonical:    "reelpin.feedback",
		canonicalKey: "id",
		changed:      tableShape.changed,
		insert: func(s tableShape, from, where string) string {
			return `
				INSERT INTO reelpin.feedback (id, user_id, topic, email, message, created_at)
				SELECT ` + keyExpression("feedback", "id", s) + `, ` + uuidOrNull(s.col("user_id")) + `,
				       l.topic, nullif(` + s.col("email") + `, ''), l.message,
				       coalesce(` + s.col("created_at") + `, now())
				FROM ` + from + ` WHERE ` + where + `
				ON CONFLICT (id) DO UPDATE SET
					user_id = EXCLUDED.user_id,
					topic = EXCLUDED.topic,
					email = EXCLUDED.email,
					message = EXCLUDED.message`
		},
	},
	{
		name:         "link_clicks",
		key:          "id",
		canonical:    "reelpin.link_clicks",
		canonicalKey: "id",
		changed:      tableShape.changed,
		insert: func(s tableShape, from, where string) string {
			return `
				INSERT INTO reelpin.link_clicks (id, target, referer, user_agent, country, created_at)
				SELECT ` + keyExpression("link_clicks", "id", s) + `, l.target,
				       ` + s.col("referer") + `, ` + s.col("user_agent") + `, ` + s.col("country") + `,
				       coalesce(` + s.col("created_at") + `, now())
				FROM ` + from + ` WHERE ` + where + `
				ON CONFLICT (id) DO UPDATE SET
					target = EXCLUDED.target,
					referer = EXCLUDED.referer,
					user_agent = EXCLUDED.user_agent,
					country = EXCLUDED.country`
		},
	},
}

// The legacy reel id is the save id, so a reminder or price track points at
// the save its reel became. A reel that lost a save conflict, or merged into
// the existing save, points at the save its user kept for the same content.
// One never carried points at nothing.
const saveJoin = ` LEFT JOIN reelpin.user_saves s
	ON s.user_id::text = lower(l.user_id::text)
	AND (s.id = l.reel_id OR s.content_id = (
		SELECT a.content_id FROM reelpin.backfill_audit a
		WHERE a.backfill_version = '` + Version + `' AND a.source_table = '` + sourceReels + `'
		  AND a.source_id = l.reel_id AND a.action = 'skipped_conflict'))`

const hiddenPinJoin = `
	JOIN reelpin.user_saves s ON s.id = l.reel_id AND s.user_id::text = lower(l.user_id::text)
	JOIN reelpin.contents c ON c.id = s.content_id
	JOIN reelpin.content_locations cl
	  ON cl.content_version_id = c.current_version_id AND cl.ordinal = l.location_index`

const hiddenPinPreference = `
	JOIN reelpin.contents c ON c.id = s.content_id
	JOIN reelpin.content_locations cl
	  ON cl.content_version_id = c.current_version_id AND cl.ordinal = l.location_index
	JOIN reelpin.user_pin_preferences p
	  ON p.location_id = cl.id AND p.user_id = s.user_id AND p.hidden`

// latest is the newest of the timestamps a table keeps, NULLs ignored.
func latest(s tableShape, columns ...string) string {
	present := []string{}
	for _, column := range columns {
		if s.has(column) {
			present = append(present, "l."+column)
		}
	}
	if len(present) == 0 {
		return s.changed()
	}
	return "greatest(" + strings.Join(present, ", ") + ")"
}

func textArray(expression string) string {
	return "ARRAY(SELECT jsonb_array_elements_text(coalesce(" + expression + ", '[]'::jsonb)))"
}

// skipAction names the canonical constraint a legacy row broke. Those rows are
// recorded and passed over; anything else is a fault in the run and stops it,
// so a resumed run retries the row rather than silently losing it.
func skipAction(err error) (string, string, bool) {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return "", "", false
	}
	note := pgErr.ConstraintName
	if note == "" {
		note = "sqlstate " + pgErr.Code
	}
	switch {
	case pgErr.Code == "23503":
		return "skipped_unknown_reference", note, true
	case pgErr.Code == "23505":
		return "skipped_conflict", note, true
	case strings.HasPrefix(pgErr.Code, "23"):
		return "skipped_invalid", note, true
	case strings.HasPrefix(pgErr.Code, "22"):
		return "skipped_unreadable", note, true
	}
	return "", "", false
}

type legacyKey struct {
	id  string
	raw string
	// owner is the lower-cased user the legacy row names, or "" for a table
	// nobody owns.
	owner string
}

// ownerNote is the audit note a carried hidden pin keeps. The preference it
// became has no id of its own, so once the legacy row is gone this is the only
// record of whose pin it was.
func ownerNote(owner string) string { return ownerNotePrefix + owner }

const ownerNotePrefix = "owner "

// noteOwnerMigrated is whether the audit row a was carried, by ownerNote, for
// a user Go has served.
const noteOwnerMigrated = `EXISTS (SELECT 1 FROM ` + servedUsers + ` m
	WHERE a.note = '` + ownerNotePrefix + `' || m.user_id::text)`

// upsertStatement is one mapping's per-row statement. It repeats the migrated
// filter the page was read with, so a user who moved after the read is not
// written.
func (b *Backfiller) upsertStatement(mapping tableMapping, shape tableShape) (string, error) {
	cast, err := keyCast(shape, mapping.key)
	if err != nil {
		return "", err
	}
	return mapping.insert(shape, b.source.table(mapping.name)+" l",
		"l."+mapping.key+" = $1::"+cast+shape.environmentFilter()+skipMigrated(mapping.owner)), nil
}

func (b *Backfiller) copyTable(ctx context.Context, options Options, mapping tableMapping, report *Report) error {
	counts := TableReport{SkippedByAction: map[string]int{}}
	shape := b.source.shape(mapping.name)
	if !shape.exists {
		counts.Absent = true
		report.Tables[mapping.name] = counts
		return nil
	}

	statement, err := b.upsertStatement(mapping, shape)
	if err != nil {
		return err
	}

	// A table copied whole has no resume point: every run reads all of it.
	resumable := options.Since == nil && mapping.changed != nil
	cursor := ""
	if resumable {
		if cursor, err = b.cursor(ctx, mapping.name); err != nil {
			return err
		}
	}

	defer func() { report.Tables[mapping.name] = counts }()
	batch := int64(0)
	for {
		keys, err := b.readKeys(ctx, mapping, shape, cursor, options)
		if err != nil {
			return err
		}
		if len(keys) == 0 {
			if !resumable {
				return nil
			}
			return b.finishCursor(ctx, options, mapping.name)
		}
		batch++

		for _, key := range keys {
			cursor = key.id
			counts.Scanned++
			if err := b.copyOneRow(ctx, options, batch, mapping, statement, key, &counts); err != nil {
				return err
			}
		}
		if !resumable {
			continue
		}
		if err := b.saveCursor(ctx, options, mapping.name, cursor, counts.Scanned); err != nil {
			return err
		}
	}
}

func (b *Backfiller) readKeys(
	ctx context.Context,
	mapping tableMapping,
	shape tableShape,
	cursor string,
	options Options,
) ([]legacyKey, error) {
	key := keyExpression(mapping.name, mapping.key, shape)
	since, changed := options.Since, "NULL::timestamptz"
	if mapping.changed == nil {
		since = nil
	} else {
		changed = mapping.changed(shape)
	}
	owner := "''"
	if mapping.owner != "" {
		owner = "coalesce(lower(l." + mapping.owner + "::text), '')"
	}
	rows, err := b.pool.Query(ctx, `
		SELECT (`+key+`)::text, l.`+mapping.key+`::text, `+owner+`
		FROM `+b.source.table(mapping.name)+` l
		WHERE ($1::uuid IS NULL OR `+key+` > $1::uuid)
		  AND ($3::timestamptz IS NULL OR `+changed+` > $3)`+shape.environmentFilter()+
		skipMigrated(mapping.owner)+`
		ORDER BY `+key+`
		LIMIT $2`, nullableUUID(cursor), options.BatchSize, since)
	if err != nil {
		return nil, fmt.Errorf("reading legacy %s: %w", mapping.name, err)
	}
	defer rows.Close()

	collected := []legacyKey{}
	for rows.Next() {
		var key legacyKey
		if err := rows.Scan(&key.id, &key.raw, &key.owner); err != nil {
			return nil, fmt.Errorf("reading legacy %s: %w", mapping.name, err)
		}
		collected = append(collected, key)
	}
	return collected, rows.Err()
}

// copyOneRow runs the mapping in a transaction of its own. A dry run runs the
// same statement and rolls it back, so it is rejected by exactly the
// constraints a real run would be.
func (b *Backfiller) copyOneRow(
	ctx context.Context,
	options Options,
	batch int64,
	mapping tableMapping,
	statement string,
	key legacyKey,
	counts *TableReport,
) error {
	transaction, err := b.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("starting the %s transaction: %w", mapping.name, err)
	}
	defer transaction.Rollback(ctx)

	skip := func(action, note string) error {
		_ = transaction.Rollback(ctx)
		counts.Skipped++
		counts.SkippedByAction[action]++
		return b.audit(ctx, options, batch, mapping.name, key.id, action, nil, nil, nil, note)
	}

	tag, err := transaction.Exec(ctx, statement, key.raw)
	if err != nil {
		action, note, ok := skipAction(err)
		if !ok {
			return fmt.Errorf("copying legacy %s %s: %w", mapping.name, key.id, err)
		}
		return skip(action, note)
	}
	if tag.RowsAffected() == 0 {
		if mapping.owner != "" {
			moved, err := isMigrated(ctx, transaction, key.owner)
			if err != nil {
				return err
			}
			if moved {
				return nil
			}
		}
		if mapping.unmatched != "" {
			return skip(mapping.unmatched, "")
		}
		// An upsert that declined to overwrite a newer canonical row affects
		// nothing and is still a carried row. A source row deleted since the
		// page was read is not.
		var present bool
		if err := transaction.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+mapping.canonical+
			` WHERE `+mapping.canonicalKey+` = $1)`, key.id).Scan(&present); err != nil {
			return fmt.Errorf("checking canonical %s %s: %w", mapping.name, key.id, err)
		}
		if !present {
			return skip("skipped_vanished", "")
		}
	}

	counts.Copied++
	if !options.Execute {
		return nil
	}
	note := ""
	if mapping.canonicalKey == "" {
		note = ownerNote(key.owner)
	}
	if err := auditTx(ctx, transaction, batch, mapping.name, key.id, "copied", nil, nil, nil, note); err != nil {
		return err
	}
	return transaction.Commit(ctx)
}
