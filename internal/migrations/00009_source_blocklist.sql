-- migrate:up

-- Sources that must not be ingested again: a privacy or legal request, a
-- private source that should never have been global, or an operator decision.
-- A purge writes one of these rows so the next share of the same link cannot
-- quietly rebuild what was just removed.
CREATE TABLE reelpin.source_blocklist (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_platform     TEXT NOT NULL,
    source_content_type TEXT NOT NULL,
    source_content_id   TEXT,
    normalized_url_hash TEXT,
    reason              TEXT NOT NULL,
    blocked_by          TEXT NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- One of the two identity forms has to be present, matching how contents
    -- identifies itself: a stable source id, or the normalized URL's hash.
    CONSTRAINT source_blocklist_identity_check
        CHECK (source_content_id IS NOT NULL OR normalized_url_hash IS NOT NULL)
);

CREATE UNIQUE INDEX source_blocklist_source_key
    ON reelpin.source_blocklist (source_platform, source_content_type, source_content_id)
    WHERE source_content_id IS NOT NULL;

CREATE UNIQUE INDEX source_blocklist_url_key
    ON reelpin.source_blocklist (normalized_url_hash)
    WHERE normalized_url_hash IS NOT NULL;

-- The block is enforced by the database, not by whoever remembers to check it.
-- Enqueue, backfill and any future writer all insert into contents, and all of
-- them are refused here.
CREATE FUNCTION reelpin.reject_blocklisted_source() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM reelpin.source_blocklist b
        WHERE (b.source_content_id IS NOT NULL
               AND b.source_platform = NEW.source_platform
               AND b.source_content_type = NEW.source_content_type
               AND b.source_content_id = NEW.source_content_id)
           OR (b.normalized_url_hash IS NOT NULL
               AND b.normalized_url_hash = NEW.normalized_url_hash)
    ) THEN
        RAISE EXCEPTION 'source is blocklisted and cannot be ingested'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER contents_reject_blocklisted
    BEFORE INSERT ON reelpin.contents
    FOR EACH ROW EXECUTE FUNCTION reelpin.reject_blocklisted_source();

GRANT SELECT ON reelpin.source_blocklist TO reelpin_app;
GRANT SELECT, INSERT, DELETE ON reelpin.source_blocklist TO reelpin_maintenance;

-- migrate:down

DROP TRIGGER IF EXISTS contents_reject_blocklisted ON reelpin.contents;
DROP FUNCTION IF EXISTS reelpin.reject_blocklisted_source();
DROP TABLE IF EXISTS reelpin.source_blocklist;
