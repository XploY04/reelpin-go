package backfill

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/XploY04/reelpin-go/internal/sourceidentity"
	"github.com/jackc/pgx/v5"
)

const sourceCollections = "collections"

var collectionTables = []string{sourceCollections, "collection_members", "collection_items", "collection_invites"}

type collectionMapping struct {
	name       string
	columns    []string
	selectRows string
	match      string
	conflict   string
}

// A legacy reel can have merged into another save of the same user's content.
func (s source) collectionSave(reel string) string {
	return `SELECT s.id FROM ` + s.table(sourceReels) + ` r
 JOIN reelpin.user_saves s ON s.user_id::text = lower(r.user_id::text)
 AND (s.id = r.id OR s.content_id = (SELECT a.content_id FROM reelpin.backfill_audit a
 WHERE a.backfill_version = '` + Version + `' AND a.source_table = '` + sourceReels + `'
 AND a.source_id = r.id AND a.action = 'skipped_conflict')) WHERE r.id = ` + reel
}

func (s source) collectionMappings() []collectionMapping {
	return []collectionMapping{
		{name: sourceCollections,
			columns: strings.Fields("id owner_id name description cover_save_id visibility link_token_hash link_expires_at created_at updated_at"),
			selectRows: `SELECT l.id AS source_id,l.id,` + uuidOrNull("l.owner_id") + ` AS owner_id,l.name,l.description,
 (` + s.collectionSave("l.cover_reel_id") + `) AS cover_save_id,l.visibility,l.link_token_hash,NULL::timestamptz AS link_expires_at,l.created_at,l.updated_at FROM ` + s.table(sourceCollections) + ` l`,
			match: `c.id=e.id`, conflict: "id"},
		{name: "collection_members",
			columns: strings.Fields("collection_id user_id role invited_by created_at"),
			selectRows: `SELECT l.id AS source_id,l.collection_id,` + uuidOrNull("l.user_id") + ` AS user_id,l.role,
 ` + uuidOrNull("l.invited_by") + ` AS invited_by,l.created_at FROM ` + s.table("collection_members") + ` l`,
			match: `c.collection_id=e.collection_id AND c.user_id=e.user_id`, conflict: "collection_id,user_id"},
		{name: "collection_items",
			columns: strings.Fields("collection_id save_id added_by added_at"),
			selectRows: `SELECT l.id AS source_id,l.collection_id,(` + s.collectionSave("l.reel_id") + `) AS save_id,
 ` + uuidOrNull("l.added_by") + ` AS added_by,l.created_at AS added_at FROM ` + s.table("collection_items") + ` l`,
			match: `c.collection_id=e.collection_id AND c.save_id=e.save_id`, conflict: "collection_id,save_id"},
		{name: "collection_invites",
			columns: strings.Fields("id collection_id token_hash role created_by expires_at max_uses uses revoked created_at"),
			selectRows: `SELECT l.id AS source_id,l.id,l.collection_id,l.token_hash,l.role,` + uuidOrNull("l.created_by") + ` AS created_by,
 l.expires_at,l.max_uses,l.uses,l.revoked,l.created_at FROM ` + s.table("collection_invites") + ` l`,
			match: `c.id=e.id`, conflict: "id"},
	}
}

func (s source) requireCollections() error {
	for _, name := range collectionTables {
		if s.shape(name).exists {
			return s.require(collectionTables...)
		}
	}
	return nil
}

// Audited participants prevent resurrection after Go removes the last membership.
func (s source) legacyCollectionParticipants(id string) string {
	return `SELECT lower(owner_id::text) AS user_id FROM ` + s.table(sourceCollections) + ` WHERE id=` + id + `
 UNION SELECT lower(user_id::text) FROM ` + s.table("collection_members") + ` WHERE collection_id=` + id + `
 UNION SELECT lower(invited_by::text) FROM ` + s.table("collection_members") + ` WHERE collection_id=` + id + `
 UNION SELECT lower(added_by::text) FROM ` + s.table("collection_items") + ` WHERE collection_id=` + id + `
 UNION SELECT lower(r.user_id::text) FROM ` + s.table("collection_items") + ` i JOIN ` + s.table(sourceReels) + ` r ON r.id=i.reel_id WHERE i.collection_id=` + id + `
 UNION SELECT lower(r.user_id::text) FROM ` + s.table(sourceCollections) + ` c JOIN ` + s.table(sourceReels) + ` r ON r.id=c.cover_reel_id WHERE c.id=` + id + `
 UNION SELECT lower(created_by::text) FROM ` + s.table("collection_invites") + ` WHERE collection_id=` + id
}

func (s source) collectionParticipants(id string) string {
	legacy := ""
	if s.shape(sourceCollections).exists {
		legacy = s.legacyCollectionParticipants(id) + ` UNION `
	}
	return legacy + `SELECT owner_id::text AS user_id FROM reelpin.collections WHERE id=` + id + `
 UNION SELECT user_id::text FROM reelpin.collection_members WHERE collection_id=` + id + `
 UNION SELECT invited_by::text FROM reelpin.collection_members WHERE collection_id=` + id + `
 UNION SELECT added_by::text FROM reelpin.collection_items WHERE collection_id=` + id + `
 UNION SELECT s.user_id::text FROM reelpin.collection_items i JOIN reelpin.user_saves s ON s.id=i.save_id WHERE i.collection_id=` + id + `
 UNION SELECT s.user_id::text FROM reelpin.collections c JOIN reelpin.user_saves s ON s.id=c.cover_save_id WHERE c.id=` + id + `
 UNION SELECT created_by::text FROM reelpin.collection_invites WHERE collection_id=` + id + `
 UNION SELECT jsonb_array_elements_text(history.note::jsonb->'participants') FROM reelpin.backfill_audit history
 WHERE history.backfill_version='` + Version + `' AND history.source_table='collections' AND (history.action IN ('copied','skipped_migrated','deleted_in_legacy') OR history.note::jsonb ? 'canonical_hash' OR history.note::jsonb->>'protected'='true') AND history.source_id=` + id
}

func (s source) collectionProtected(id string) string {
	return `(` + s.collectionParticipantServed(id) + ` OR EXISTS(SELECT 1 FROM reelpin.backfill_audit protection
    WHERE protection.backfill_version='` + Version + `' AND protection.source_table='collections' AND protection.source_id=` + id + `
    AND protection.note::jsonb->>'protected'='true'))`
}

func (s source) collectionParticipantServed(id string) string {
	return `EXISTS(SELECT 1 FROM (` + s.collectionParticipants(id) + `) participants
 JOIN ` + servedUsers + ` m ON m.user_id::text=participants.user_id)`
}

func collectionFingerprint(id string) string {
	return `encode(sha256(convert_to(jsonb_build_object(
      'collection',(SELECT to_jsonb(fingerprint_collection) FROM reelpin.collections fingerprint_collection WHERE fingerprint_collection.id=` + id + `),
      'members',(SELECT coalesce(jsonb_agg(to_jsonb(fingerprint_member) ORDER BY user_id),'[]') FROM reelpin.collection_members fingerprint_member WHERE collection_id=` + id + `),
      'items',(SELECT coalesce(jsonb_agg(to_jsonb(fingerprint_item) ORDER BY save_id),'[]') FROM reelpin.collection_items fingerprint_item WHERE collection_id=` + id + `),
      'invites',(SELECT coalesce(jsonb_agg(to_jsonb(fingerprint_invite) ORDER BY id),'[]') FROM reelpin.collection_invites fingerprint_invite WHERE collection_id=` + id + `)
    )::text,'UTF8')),'hex')`
}

func collectionDrift(id string) string {
	return `EXISTS(SELECT 1 FROM reelpin.backfill_audit baseline
      WHERE baseline.backfill_version='` + Version + `' AND baseline.source_table='collections' AND baseline.source_id=` + id + `
      AND baseline.note::jsonb->>'canonical_hash' IS NOT NULL
      AND baseline.note::jsonb->>'canonical_hash' IS DISTINCT FROM ` + collectionFingerprint(id) + `)
    OR (EXISTS(SELECT 1 FROM reelpin.collections existing_collection WHERE existing_collection.id=` + id + `)
      AND NOT EXISTS(SELECT 1 FROM reelpin.backfill_audit baseline WHERE baseline.backfill_version='` + Version + `'
      AND baseline.source_table='collections' AND baseline.source_id=` + id + ` AND baseline.note::jsonb ? 'canonical_hash'))`
}

// A first Go request must record ownership before or after the whole copy.
func lockCollectionOwnership(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `LOCK TABLE reelpin.user_migrations,reelpin.account_deletion_requests IN SHARE MODE`)
	return err
}

func lockCollection(ctx context.Context, tx pgx.Tx, id string) error {
	for _, name := range collectionTables {
		column := "collection_id"
		if name == sourceCollections {
			column = "id"
		}
		rows, err := tx.Query(ctx, `SELECT 1 FROM reelpin.`+name+` WHERE `+column+`=$1 FOR UPDATE`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

type collectionAudit struct {
	CollectionID  string   `json:"collection_id"`
	Participants  []string `json:"participants,omitempty"`
	UserID        string   `json:"user_id,omitempty"`
	SaveID        string   `json:"save_id,omitempty"`
	CanonicalHash string   `json:"canonical_hash,omitempty"`
	Protected     bool     `json:"protected,omitempty"`
}

type collectionRow struct{ table, id, note string }

func (b *Backfiller) collectionRows(ctx context.Context, tx pgx.Tx, id string) ([]collectionRow, error) {
	var participants []string
	if err := tx.QueryRow(ctx, `SELECT coalesce(array_agg(user_id ORDER BY user_id) FILTER(WHERE user_id IS NOT NULL),'{}') FROM (`+b.source.collectionParticipants("$1::uuid")+`) p`, id).Scan(&participants); err != nil {
		return nil, err
	}
	result := []collectionRow{}
	for _, name := range collectionTables {
		parent := "collection_id"
		if name == sourceCollections {
			parent = "id"
		}
		rows, err := tx.Query(ctx, `SELECT id::text FROM `+b.source.table(name)+` WHERE `+parent+`=$1 ORDER BY id`, id)
		if err != nil {
			return nil, err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return nil, err
		}
		for _, sourceID := range ids {
			note := collectionAudit{CollectionID: id}
			switch name {
			case sourceCollections:
				var previous string
				if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT note FROM reelpin.backfill_audit WHERE backfill_version=$1 AND source_table='collections' AND source_id=$2),'{}')`, Version, id).Scan(&previous); err != nil {
					return nil, err
				}
				if err := json.Unmarshal([]byte(previous), &note); err != nil {
					return nil, err
				}
				note.CollectionID = id
				note.Participants = participants
			case "collection_members":
				if err := tx.QueryRow(ctx, `SELECT lower(user_id::text) FROM `+b.source.table(name)+` WHERE id=$1`, sourceID).Scan(&note.UserID); err != nil {
					return nil, err
				}
			case "collection_items":
				var save *string
				if err := tx.QueryRow(ctx, `SELECT (`+b.source.collectionSave("l.reel_id")+`)::text FROM `+b.source.table(name)+` l WHERE l.id=$1`, sourceID).Scan(&save); err != nil {
					return nil, err
				}
				note.SaveID = text(save)
			}
			encoded, err := json.Marshal(note)
			if err != nil {
				return nil, err
			}
			result = append(result, collectionRow{table: name, id: sourceID, note: string(encoded)})
		}
	}
	return result, nil
}

func (b *Backfiller) copyCollections(ctx context.Context, options Options, report *Report) error {
	if err := b.source.requireCollections(); err != nil {
		return err
	}
	for _, name := range collectionTables {
		report.Tables[name] = TableReport{Absent: !b.source.shape(name).exists, SkippedByAction: map[string]int{}}
	}
	if !b.source.shape(sourceCollections).exists {
		return nil
	}
	cursor := ""
	for {
		rows, err := b.pool.Query(ctx, `SELECT id::text FROM `+b.source.table(sourceCollections)+` WHERE ($1::uuid IS NULL OR id>$1) ORDER BY id LIMIT $2`, nullableUUID(cursor), options.BatchSize)
		if err != nil {
			return fmt.Errorf("reading legacy collections: %w", err)
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		for _, id := range ids {
			if err := b.copyCollection(ctx, options, id, report); err != nil {
				return err
			}
			cursor = id
		}
	}
}

func (b *Backfiller) copyCollection(ctx context.Context, options Options, id string, report *Report) error {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockCollectionOwnership(ctx, tx); err != nil {
		return fmt.Errorf("locking collection ownership: %w", err)
	}
	if err := lockCollection(ctx, tx, id); err != nil {
		return fmt.Errorf("locking collection: %w", err)
	}
	rows, err := b.collectionRows(ctx, tx, id)
	if err != nil {
		return fmt.Errorf("reading collection references: %w", err)
	}
	var protected bool
	if err := tx.QueryRow(ctx, `SELECT `+b.source.collectionProtected("$1::uuid"), id).Scan(&protected); err != nil {
		return err
	}
	action := "copied"
	if protected {
		action = "skipped_migrated"
	} else {
		if err := tx.QueryRow(ctx, `SELECT `+collectionDrift("$1::uuid"), id).Scan(&protected); err != nil {
			return err
		}
		if protected {
			action = "skipped_canonical_change"
		}
	}
	if action == "copied" {
		var invalid, unknown bool
		if err := tx.QueryRow(ctx, `SELECT coalesce(bool_or(p.user_id !~ `+uuidPattern+`),false),
          coalesce(bool_or(NOT EXISTS(SELECT 1 FROM auth.users u WHERE u.id::text=p.user_id) OR
 NOT EXISTS(SELECT 1 FROM `+b.source.table(sourceUsers)+` u WHERE u.id::text=p.user_id)),false)
          FROM (`+b.source.legacyCollectionParticipants("$1::uuid")+`) p WHERE p.user_id IS NOT NULL`, id).Scan(&invalid, &unknown); err != nil {
			return err
		}
		if invalid {
			action = skippedInvalidOwner
		} else if unknown {
			action = skippedUnknownReference
		}
	}
	if action == "copied" {
		complete, err := b.collectionReferencesComplete(ctx, tx, id, !options.Execute)
		if err != nil {
			return err
		}
		if !complete {
			action = skippedUnknownReference
		} else {
			for _, mapping := range b.source.collectionMappings() {
				assignments := []string{}
				for _, column := range mapping.columns {
					assignments = append(assignments, column+"=EXCLUDED."+column)
				}
				parent := "e.collection_id"
				if mapping.name == sourceCollections {
					parent = "e.id"
				}
				selected := `SELECT e.` + strings.Join(mapping.columns, ",e.") + ` FROM (` + mapping.selectRows + `) e WHERE ` + parent + `=$1`
				if mapping.name == "collection_items" {
					selected = `SELECT DISTINCT ON(e.collection_id,e.save_id) e.` + strings.Join(mapping.columns, ",e.") + ` FROM (` + mapping.selectRows + `) e WHERE ` + parent + `=$1`
					if !options.Execute {
						selected += ` AND e.save_id IS NOT NULL`
					}
					selected += ` ORDER BY e.collection_id,e.save_id,e.added_at DESC,e.source_id DESC`
				}
				_, err = tx.Exec(ctx, `INSERT INTO reelpin.`+mapping.name+` (`+strings.Join(mapping.columns, ",")+`) `+selected+`
 ON CONFLICT (`+mapping.conflict+`) DO UPDATE SET `+strings.Join(assignments, ","), id)
				if err != nil {
					var ok bool
					action, _, ok = skipAction(err)
					if !ok {
						return fmt.Errorf("copying legacy %s: %w", mapping.name, err)
					}
					break
				}
			}
		}
	}
	if action == "copied" {
		if err := tx.QueryRow(ctx, `SELECT `+b.source.collectionParticipantServed("$1::uuid"), id).Scan(&protected); err != nil {
			return err
		}
		if protected {
			action = "skipped_migrated"
		}
	}
	if action != "copied" {
		if err := tx.Rollback(ctx); err != nil {
			return err
		}
		tx, err = b.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
	}
	for _, row := range rows {
		if row.table == sourceCollections {
			var note collectionAudit
			if err := json.Unmarshal([]byte(row.note), &note); err != nil {
				return err
			}
			if action == "copied" {
				if err := tx.QueryRow(ctx, `SELECT `+collectionFingerprint("$1::uuid"), id).Scan(&note.CanonicalHash); err != nil {
					return err
				}
			}
			if protected {
				note.Protected = true
			}
			encoded, err := json.Marshal(note)
			if err != nil {
				return err
			}
			row.note = string(encoded)
		}
		counts := report.Tables[row.table]
		counts.Scanned++
		if action == "copied" {
			counts.Copied++
		} else {
			counts.Skipped++
			counts.SkippedByAction[action]++
		}
		report.Tables[row.table] = counts
		if options.Execute {
			if err := auditTx(ctx, tx, 0, row.table, row.id, action, nil, nil, nil, row.note); err != nil {
				return err
			}
		}
	}
	if !options.Execute {
		return nil
	}
	return tx.Commit(ctx)
}

func (b *Backfiller) deleteCollections(ctx context.Context, tx pgx.Tx, report *Report) error {
	if !b.source.shape(sourceCollections).exists {
		return nil
	}
	mappings := b.source.collectionMappings()
	// Children go first. Only rows this backfill carried may be removed.
	for _, index := range []int{1, 2, 3, 0} {
		mapping := mappings[index]
		id := `(a.note::jsonb->>'collection_id')::uuid`
		match := `c.id=a.source_id`
		switch mapping.name {
		case "collection_members":
			match = `c.collection_id=` + id + ` AND c.user_id::text=a.note::jsonb->>'user_id'`
		case "collection_items":
			match = `c.collection_id=` + id + ` AND c.save_id::text=a.note::jsonb->>'save_id'`
		}
		var count int
		err := tx.QueryRow(ctx, `WITH gone AS (
 SELECT a.source_id,a.note FROM reelpin.backfill_audit a
 WHERE a.backfill_version=$1 AND a.source_table='`+mapping.name+`' AND a.action='copied'
 AND NOT EXISTS(SELECT 1 FROM `+b.source.table(mapping.name)+` l WHERE l.id=a.source_id)
 AND NOT `+b.source.collectionProtected(id)+`
 ), removed AS (DELETE FROM reelpin.`+mapping.name+` c USING gone a WHERE `+match+`
 AND NOT EXISTS(SELECT 1 FROM (`+mapping.selectRows+`) e WHERE `+mapping.match+`) RETURNING 1),
 marked AS (UPDATE reelpin.backfill_audit a SET action='`+deletedInLegacy+`' FROM gone g
 WHERE a.backfill_version=$1 AND a.source_table='`+mapping.name+`' AND a.source_id=g.source_id RETURNING 1)
 SELECT count(*)::int FROM removed`, Version).Scan(&count)
		if err != nil {
			return fmt.Errorf("propagating collection deletes: %w", err)
		}
		report.Deleted[mapping.name] = count
	}
	return nil
}

// A save or user delete must not cascade through an aggregate Go already owns.
func (s source) protectedCollectionSave(save string) string {
	return `EXISTS(SELECT 1 FROM reelpin.collections protected_collection
 WHERE (protected_collection.cover_save_id=` + save + ` OR EXISTS(SELECT 1 FROM reelpin.collection_items protected_item
 WHERE protected_item.collection_id=protected_collection.id AND protected_item.save_id=` + save + `))
 AND ` + s.collectionProtected("protected_collection.id") + `)`
}

func (s source) protectedCollectionUser(user string) string {
	return `EXISTS(SELECT 1 FROM reelpin.collections protected_collection WHERE ` + s.collectionProtected("protected_collection.id") + `
 AND EXISTS(SELECT 1 FROM (` + s.collectionParticipants("protected_collection.id") + `) participant WHERE participant.user_id=` + user + `::text))`
}

func collectionAuditMatch(mapping collectionMapping) string {
	switch mapping.name {
	case "collection_members":
		return `c.collection_id::text=a.note::jsonb->>'collection_id' AND c.user_id::text=a.note::jsonb->>'user_id'`
	case "collection_items":
		return `c.collection_id::text=a.note::jsonb->>'collection_id' AND c.save_id::text=a.note::jsonb->>'save_id'`
	default:
		return `c.id=a.source_id`
	}
}

func (v *Verifier) verifyCollections(ctx context.Context, options VerifyOptions, report *VerifyReport) error {
	if err := v.source.requireCollections(); err != nil {
		return err
	}
	if !v.source.shape(sourceCollections).exists {
		for _, name := range collectionTables {
			report.Tables = append(report.Tables, TableVerification{SourceTable: name, Absent: true})
		}
		return nil
	}
	for _, mapping := range v.source.collectionMappings() {
		counted := TableVerification{SourceTable: mapping.name, SkippedByAction: map[string]int{}}
		parent := "e.collection_id"
		if mapping.name == sourceCollections {
			parent = "e.id"
		}
		base := ` FROM (` + mapping.selectRows + `) e LEFT JOIN reelpin.backfill_audit a
 ON a.backfill_version=$1 AND a.source_table='` + mapping.name + `' AND a.source_id=e.source_id`
		scope := ` NOT ` + v.source.collectionProtected(parent)
		exists := `EXISTS(SELECT 1 FROM reelpin.` + mapping.name + ` c WHERE ` + mapping.match + `)`
		if err := v.pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE `+scope+`),
 count(*) FILTER(WHERE `+scope+` AND a.source_id IS NULL),
 count(*) FILTER(WHERE `+scope+` AND coalesce(a.action,'') NOT LIKE 'skipped%'),
 count(*) FILTER(WHERE `+scope+` AND coalesce(a.action,'') NOT LIKE 'skipped%' AND `+exists+`),
 count(*) FILTER(WHERE NOT (`+scope+`)),(SELECT count(*) FROM reelpin.`+mapping.name+`)`+base, Version).
			Scan(&counted.InScope, &counted.Unexplained, &counted.Expected, &counted.Present, &counted.Migrated, &counted.CanonicalTotal); err != nil {
			return fmt.Errorf("verifying %s counts: %w", mapping.name, err)
		}
		if err := v.pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE `+collectionAuditMatch(mapping)+`) FROM reelpin.`+mapping.name+` c JOIN reelpin.backfill_audit a ON `+collectionAuditMatch(mapping)+`
 WHERE a.backfill_version=$1 AND a.source_table='`+mapping.name+`' AND a.action='copied'
 AND NOT `+v.source.collectionProtected("(a.note::jsonb->>'collection_id')::uuid"), Version).Scan(&counted.Carried); err != nil {
			return err
		}
		missing := `a.backfill_version=$1 AND a.source_table='` + mapping.name + `' AND a.action='copied'
 AND NOT ` + v.source.collectionProtected("(a.note::jsonb->>'collection_id')::uuid") + `
 AND NOT EXISTS(SELECT 1 FROM reelpin.` + mapping.name + ` c WHERE ` + collectionAuditMatch(mapping) + `)`
		if err := v.pool.QueryRow(ctx, `SELECT count(*) FROM reelpin.backfill_audit a WHERE `+missing, Version).Scan(&counted.CarriedButMissing); err != nil {
			return err
		}
		var err error
		counted.MissingIDs, err = v.ids(ctx, `SELECT a.source_id::text FROM reelpin.backfill_audit a WHERE `+missing+` ORDER BY md5(a.source_id::text) LIMIT $2`, Version, options.Sample)
		if err != nil {
			return err
		}
		counted.UnexplainedIDs, err = v.ids(ctx, `SELECT e.source_id::text`+base+` WHERE `+scope+` AND a.source_id IS NULL ORDER BY md5(e.source_id::text) LIMIT $2`, Version, options.Sample)
		if err != nil {
			return err
		}
		rows, err := v.pool.Query(ctx, `SELECT a.action,count(*)`+base+` WHERE `+scope+` AND a.action LIKE 'skipped%' GROUP BY a.action`, Version)
		if err != nil {
			return err
		}
		for rows.Next() {
			var action string
			var total int
			if err := rows.Scan(&action, &total); err != nil {
				rows.Close()
				return err
			}
			counted.Skipped += total
			counted.SkippedByAction[action] = total
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		report.Tables = append(report.Tables, counted)
		comparable := mapping.selectRows
		if mapping.name == "collection_items" {
			comparable = `SELECT DISTINCT ON(collection_id,save_id) * FROM (` + mapping.selectRows + `) items ORDER BY collection_id,save_id,added_at DESC,source_id DESC`
		}
		comparisonBase := ` FROM (` + comparable + `) e LEFT JOIN reelpin.backfill_audit a
 ON a.backfill_version=$1 AND a.source_table='` + mapping.name + `' AND a.source_id=e.source_id`
		rows, err = v.pool.Query(ctx, `SELECT e.source_id::text,ARRAY(SELECT field.key FROM jsonb_each(to_jsonb(e)-'source_id') field
 WHERE to_jsonb(c)->field.key IS DISTINCT FROM field.value ORDER BY field.key)
 `+comparisonBase+` JOIN reelpin.`+mapping.name+` c ON `+mapping.match+`
 WHERE `+scope+` AND a.action='copied' ORDER BY md5(e.source_id::text) LIMIT $2`, Version, options.Sample)
		if err != nil {
			return fmt.Errorf("comparing %s fields: %w", mapping.name, err)
		}
		for rows.Next() {
			var id string
			var fields []string
			if err := rows.Scan(&id, &fields); err != nil {
				rows.Close()
				return err
			}
			report.Sample.Compared++
			for _, field := range fields {
				report.Sample.differ(mapping.name, id, mapping.name+"."+field)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func (b *Backfiller) protectCollectionDeletes(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, `SELECT id::text FROM reelpin.collections UNION SELECT source_id::text FROM reelpin.backfill_audit
 WHERE backfill_version=$1 AND source_table='collections' ORDER BY 1`, Version)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := lockCollection(ctx, tx, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE reelpin.backfill_audit a SET note=jsonb_set(a.note::jsonb,'{protected}','true')::text
 WHERE a.backfill_version=$1 AND a.source_table='collections' AND a.source_id=$2
 AND (`+b.source.collectionProtected("$2::uuid")+` OR `+collectionDrift("$2::uuid")+`)`, Version, id); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backfiller) refreshCollectionFingerprints(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `UPDATE reelpin.backfill_audit a SET note=jsonb_set(a.note::jsonb,'{canonical_hash}',to_jsonb(`+collectionFingerprint("a.source_id")+`))::text
 WHERE a.backfill_version=$1 AND a.source_table='collections' AND NOT `+b.source.collectionProtected("a.source_id"), Version)
	return err
}

func (b *Backfiller) planCollectionReferences(ctx context.Context, execute bool) error {
	b.plannedCollectionSaves = map[string]string{}
	if execute || !b.source.shape(sourceCollections).exists {
		return nil
	}
	rows, err := b.pool.Query(ctx, `SELECT reel_id::text FROM `+b.source.table("collection_items")+`
 UNION SELECT cover_reel_id::text FROM `+b.source.table(sourceCollections)+` WHERE cover_reel_id IS NOT NULL`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		b.plannedCollectionSaves[id] = ""
	}
	return rows.Err()
}

func (b *Backfiller) planCollectionSave(ctx context.Context, row reelRow, identity sourceidentity.SourceIdentity, scopeHash string) error {
	if _, needed := b.plannedCollectionSaves[row.ID]; !needed {
		return nil
	}
	var blocked bool
	if err := b.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM reelpin.source_blocklist b
 WHERE (b.source_platform=$1 AND b.source_content_type=$2 AND b.source_content_id=$3 AND b.source_content_id IS NOT NULL)
 OR b.normalized_url_hash=$4)
 AND NOT EXISTS(SELECT 1 FROM reelpin.contents c WHERE c.access_scope_hash=$5
 AND (($3<>'' AND c.source_platform=$1 AND c.source_content_type=$2 AND c.source_content_id=$3)
 OR ($3='' AND c.source_content_id IS NULL AND c.normalized_url_hash=$4)))`, identity.Platform, identity.ContentType, identity.ContentID, sourceidentity.URLHash(identity.NormalizedURL), scopeHash).Scan(&blocked); err != nil {
		return err
	}
	if !blocked {
		b.plannedCollectionSaves[row.ID] = strings.ToLower(row.UserID)
	}
	return nil
}

func (b *Backfiller) collectionReferencesComplete(ctx context.Context, tx pgx.Tx, id string, planned bool) (bool, error) {
	rows, err := tx.Query(ctx, `SELECT r.id::text,lower(r.user_id::text),(`+b.source.collectionSave("reference.reel_id")+`)::text
 FROM (SELECT reel_id FROM `+b.source.table("collection_items")+` WHERE collection_id=$1
 UNION SELECT cover_reel_id FROM `+b.source.table(sourceCollections)+` WHERE id=$1 AND cover_reel_id IS NOT NULL) reference
 LEFT JOIN `+b.source.table(sourceReels)+` r ON r.id=reference.reel_id`, id)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	complete := true
	for rows.Next() {
		var reel, owner, save *string
		if err := rows.Scan(&reel, &owner, &save); err != nil {
			return false, err
		}
		if save == nil && !(planned && reel != nil && owner != nil && b.plannedCollectionSaves[*reel] == *owner && *owner != "") {
			complete = false
		}
	}
	return complete, rows.Err()
}
