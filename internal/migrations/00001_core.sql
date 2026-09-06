-- migrate:up

-- The private schema. Everything the Go service owns lives here; the Supabase
-- API surface (public, auth, storage) never sees it.
CREATE SCHEMA IF NOT EXISTS reelpin;

-- Two NOLOGIN roles carry the privilege boundary. reelpin_app is what the
-- service runs as; reelpin_maintenance exists only for the audited global
-- purge, which is the one operation allowed to delete a content version.
-- Roles are cluster-wide, so creation tolerates a concurrent migration in
-- another database on the same server. Both codes matter: a role that already
-- existed raises duplicate_object, while two transactions creating it at the
-- same instant raise unique_violation on the catalogue index instead. Test
-- packages migrate in parallel, so the second case is the common one and cost
-- a CI run before it was caught.
DO $$
BEGIN
    CREATE ROLE reelpin_app NOLOGIN;
EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL;
END $$;

DO $$
BEGIN
    CREATE ROLE reelpin_maintenance NOLOGIN;
EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL;
END $$;

REVOKE ALL ON SCHEMA reelpin FROM PUBLIC;
GRANT USAGE ON SCHEMA reelpin TO reelpin_app, reelpin_maintenance;

-- One row per piece of source content, shared by every user who saves it.
-- Identity is global and never an ownership boundary: privacy lives in the
-- access-scope hash, which keeps private and credential-scoped fetches from
-- ever deduplicating across users.
CREATE TABLE reelpin.contents (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_platform     TEXT NOT NULL,
    source_content_type TEXT NOT NULL
        CONSTRAINT contents_content_type_check CHECK (source_content_type IN
            ('reel', 'post', 'video', 'image', 'carousel', 'article', 'page', 'place', 'other')),
    normalized_url      TEXT NOT NULL,
    normalized_url_hash TEXT NOT NULL,
    source_content_id   TEXT,
    access_scope_hash   TEXT NOT NULL,
    current_version_id  UUID,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Global public identity: platform, type, stable source id, access scope.
CREATE UNIQUE INDEX contents_source_identity_key
    ON reelpin.contents (source_platform, source_content_type, source_content_id, access_scope_hash)
    WHERE source_content_id IS NOT NULL;

-- The fallback identity when a platform has no stable id: the normalized URL.
CREATE UNIQUE INDEX contents_url_identity_key
    ON reelpin.contents (normalized_url_hash, access_scope_hash)
    WHERE source_content_id IS NULL;

-- One immutable extraction. Reprocessing under a new prompt, schema or model
-- writes a new row; nothing ever edits an old one.
CREATE TABLE reelpin.content_versions (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    content_id        UUID NOT NULL REFERENCES reelpin.contents(id) ON DELETE CASCADE,
    processor_version TEXT NOT NULL,
    prompt_version    TEXT NOT NULL,
    schema_version    TEXT NOT NULL,
    model_version     TEXT NOT NULL,
    title             TEXT NOT NULL DEFAULT '',
    summary           TEXT NOT NULL DEFAULT '',
    caption           TEXT NOT NULL DEFAULT '',
    transcript        TEXT NOT NULL DEFAULT '',
    tags              TEXT[] NOT NULL DEFAULT '{}',
    key_facts         TEXT[] NOT NULL DEFAULT '{}',
    raw_extraction    JSONB NOT NULL DEFAULT '{}'::jsonb,
    media             JSONB NOT NULL DEFAULT '{}'::jsonb,
    extraction_status TEXT NOT NULL DEFAULT 'completed'
        CONSTRAINT content_versions_status_check CHECK (extraction_status IN
            ('completed', 'partial')),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- The composite target for contents.current_version_id: a content row can
    -- only point at one of its own versions.
    CONSTRAINT content_versions_content_key UNIQUE (content_id, id)
);

CREATE INDEX content_versions_content_idx ON reelpin.content_versions (content_id);
CREATE INDEX content_versions_tags_idx ON reelpin.content_versions USING GIN (tags);

ALTER TABLE reelpin.contents
    ADD CONSTRAINT contents_current_version_fk
    FOREIGN KEY (id, current_version_id)
    REFERENCES reelpin.content_versions (content_id, id)
    ON DELETE SET NULL (current_version_id);

-- Immutability is enforced twice: no UPDATE grant below, and this trigger for
-- any session that outranks the grants.
CREATE FUNCTION reelpin.reject_content_version_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'content versions are immutable; write a new version';
END $$;

CREATE TRIGGER content_versions_immutable
    BEFORE UPDATE ON reelpin.content_versions
    FOR EACH ROW EXECUTE FUNCTION reelpin.reject_content_version_update();

-- One user's save of one content. Its id is the public reel id, which is why
-- the backfill can preserve every existing public.reels.id.
CREATE TABLE reelpin.user_saves (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
    content_id UUID NOT NULL REFERENCES reelpin.contents(id) ON DELETE CASCADE,
    saved_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT user_saves_user_content_key UNIQUE (user_id, content_id)
);

-- The list path: saved_at DESC, id DESC per the v2 contract.
CREATE INDEX user_saves_list_idx ON reelpin.user_saves (user_id, saved_at DESC, id DESC);
CREATE INDEX user_saves_content_idx ON reelpin.user_saves (content_id);

-- The application writes content and saves, reads everything, updates only the
-- mutable pointer on contents, and can never edit or delete a version.
GRANT SELECT, INSERT, UPDATE ON reelpin.contents TO reelpin_app;
GRANT SELECT, INSERT ON reelpin.content_versions TO reelpin_app;
GRANT SELECT, INSERT, DELETE ON reelpin.user_saves TO reelpin_app;
GRANT DELETE ON reelpin.contents TO reelpin_maintenance;
GRANT SELECT, DELETE ON reelpin.content_versions TO reelpin_maintenance;
GRANT SELECT, DELETE ON reelpin.user_saves TO reelpin_maintenance;

-- migrate:down

-- Disposable databases only; production corrects forward.
DROP TABLE IF EXISTS reelpin.user_saves;
ALTER TABLE reelpin.contents DROP CONSTRAINT IF EXISTS contents_current_version_fk;
DROP TABLE IF EXISTS reelpin.content_versions;
DROP FUNCTION IF EXISTS reelpin.reject_content_version_update();
DROP TABLE IF EXISTS reelpin.contents;
DROP SCHEMA IF EXISTS reelpin;
