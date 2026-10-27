-- migrate:up

-- Where the legacy backfill got to, so a stopped run resumes instead of
-- starting over. One row per source table per backfill version.
CREATE TABLE reelpin.backfill_progress (
    backfill_version TEXT NOT NULL,
    source_table     TEXT NOT NULL,
    last_source_id   UUID,
    scanned          BIGINT NOT NULL DEFAULT 0,
    finished_at      TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT backfill_progress_pkey PRIMARY KEY (backfill_version, source_table)
);

-- What the backfill decided about each legacy row, so a disagreement can be
-- traced without re-deriving it. It stores identifiers only: no content, no
-- URLs and no user text.
CREATE TABLE reelpin.backfill_audit (
    id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    backfill_version   TEXT NOT NULL,
    batch              BIGINT NOT NULL,
    source_table       TEXT NOT NULL,
    source_id          UUID NOT NULL,
    action             TEXT NOT NULL,
    content_id         UUID,
    content_version_id UUID,
    run_id             UUID,
    note               TEXT NOT NULL DEFAULT '',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Keyed by row, so a resumed run corrects its own earlier decision rather
    -- than writing a second one beside it.
    CONSTRAINT backfill_audit_row_key UNIQUE (backfill_version, source_table, source_id)
);

CREATE INDEX backfill_audit_action_idx ON reelpin.backfill_audit (backfill_version, action);

-- Bookkeeping for a maintenance command. Nothing serving traffic reads it.
GRANT SELECT, INSERT, UPDATE, DELETE ON reelpin.backfill_progress TO reelpin_maintenance;
GRANT SELECT, INSERT, UPDATE, DELETE ON reelpin.backfill_audit TO reelpin_maintenance;

-- migrate:down

DROP TABLE IF EXISTS reelpin.backfill_audit;
DROP TABLE IF EXISTS reelpin.backfill_progress;
