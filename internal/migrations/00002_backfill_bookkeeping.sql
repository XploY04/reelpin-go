-- migrate:up

-- Where a backfill got to, so a stopped run resumes instead of restarting.
CREATE TABLE IF NOT EXISTS reelpin.backfill_progress (
    backfill_version TEXT NOT NULL,
    source_table     TEXT NOT NULL,
    last_source_id   UUID,
    scanned          BIGINT NOT NULL DEFAULT 0,
    finished_at      TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (backfill_version, source_table)
);

-- What the backfill decided about each row, so a disagreement can be traced
-- without re-deriving it. No content or user text is stored here.
CREATE TABLE IF NOT EXISTS reelpin.backfill_audit (
    id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    backfill_version   TEXT NOT NULL,
    batch              BIGINT NOT NULL,
    source_table       TEXT NOT NULL,
    source_id          UUID NOT NULL,
    action             TEXT NOT NULL,
    content_id         UUID,
    content_version_id UUID,
    processing_run_id  UUID,
    note               TEXT NOT NULL DEFAULT '',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT backfill_audit_row_key UNIQUE (backfill_version, source_table, source_id)
);

CREATE INDEX IF NOT EXISTS backfill_audit_action_idx
    ON reelpin.backfill_audit (backfill_version, action);

-- migrate:down

DROP TABLE IF EXISTS reelpin.backfill_audit;
DROP TABLE IF EXISTS reelpin.backfill_progress;
