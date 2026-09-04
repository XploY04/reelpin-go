-- migrate:up

-- Global processing lives in its own schema. Existing tables stay where they
-- are, so current readers and IDs are untouched.
CREATE SCHEMA IF NOT EXISTS reelpin;

CREATE EXTENSION IF NOT EXISTS postgis;
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- One row per piece of source content, shared by every user who saved it.
CREATE TABLE IF NOT EXISTS reelpin.contents (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_platform     TEXT NOT NULL,
    source_content_type TEXT NOT NULL,
    source_content_id   TEXT,
    normalized_url      TEXT NOT NULL,
    normalized_url_hash TEXT NOT NULL,
    -- Content behind a credential is not the same content as its public form,
    -- so the access scope is part of the identity.
    access_scope_hash   TEXT NOT NULL DEFAULT 'public',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One row per processing of a content by one processor version.
CREATE TABLE IF NOT EXISTS reelpin.content_versions (
    id                        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    content_id                UUID NOT NULL REFERENCES reelpin.contents(id) ON DELETE CASCADE,
    processor_version         TEXT NOT NULL,
    extraction_schema_version TEXT NOT NULL,
    prompt_version            TEXT NOT NULL DEFAULT '',
    model_version             TEXT NOT NULL DEFAULT '',
    transcript                TEXT NOT NULL DEFAULT '',
    caption                   TEXT NOT NULL DEFAULT '',
    title                     TEXT NOT NULL DEFAULT '',
    summary                   TEXT NOT NULL DEFAULT '',
    structured                JSONB NOT NULL DEFAULT '{}'::jsonb,
    thumbnail_url             TEXT,
    parse_status              TEXT NOT NULL DEFAULT 'parsed',
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT content_versions_unique_processor UNIQUE (content_id, processor_version)
);

-- contents points at its current version. The column is added after both tables
-- exist because the reference runs the other way.
ALTER TABLE reelpin.contents
    ADD COLUMN IF NOT EXISTS current_content_version_id UUID;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'contents_current_version_fk'
    ) THEN
        ALTER TABLE reelpin.contents
            ADD CONSTRAINT contents_current_version_fk
            FOREIGN KEY (current_content_version_id)
            REFERENCES reelpin.content_versions(id) ON DELETE SET NULL;
    END IF;
END
$$;

-- A public post is one row per (platform, type, id) inside an access scope.
CREATE UNIQUE INDEX IF NOT EXISTS contents_public_identity_key
    ON reelpin.contents (source_platform, source_content_type, source_content_id, access_scope_hash)
    WHERE source_content_id IS NOT NULL;

-- A generic link has no id of its own, so its normalized URL is the identity.
CREATE UNIQUE INDEX IF NOT EXISTS contents_generic_identity_key
    ON reelpin.contents (normalized_url_hash, access_scope_hash)
    WHERE source_content_id IS NULL;

CREATE INDEX IF NOT EXISTS contents_normalized_url_hash_idx
    ON reelpin.contents (normalized_url_hash);

CREATE INDEX IF NOT EXISTS content_versions_content_idx
    ON reelpin.content_versions (content_id, created_at DESC);

CREATE TABLE IF NOT EXISTS reelpin.content_locations (
    id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    content_version_id UUID NOT NULL REFERENCES reelpin.content_versions(id) ON DELETE CASCADE,
    ordinal            INTEGER NOT NULL,
    name               TEXT NOT NULL DEFAULT '',
    address            TEXT,
    neighborhood       TEXT,
    city               TEXT,
    state              TEXT,
    country            TEXT,
    geog               geography(Point, 4326) NOT NULL,
    -- Where the place came from: the extraction, a caption, or a curated link.
    mention_source     TEXT NOT NULL DEFAULT 'extraction',
    display_label      TEXT NOT NULL DEFAULT '',
    google_maps_url    TEXT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT content_locations_ordinal_key UNIQUE (content_version_id, ordinal)
);

CREATE INDEX IF NOT EXISTS content_locations_geog_idx
    ON reelpin.content_locations USING GIST (geog);

-- Transcript chunks. Embeddings arrive with the search task.
CREATE TABLE IF NOT EXISTS reelpin.content_chunks (
    id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    content_version_id UUID NOT NULL REFERENCES reelpin.content_versions(id) ON DELETE CASCADE,
    ordinal            INTEGER NOT NULL,
    chunk_text         TEXT NOT NULL,
    content_hash       TEXT NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT content_chunks_ordinal_key UNIQUE (content_version_id, ordinal)
);

-- One global processing attempt of one content. Private per-user jobs point at
-- it, so two users sharing the same link share one run.
CREATE TABLE IF NOT EXISTS reelpin.processing_runs (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    content_id        UUID NOT NULL REFERENCES reelpin.contents(id) ON DELETE CASCADE,
    processor_version TEXT NOT NULL,
    platform          TEXT NOT NULL,
    status            TEXT NOT NULL DEFAULT 'queued',
    stage             TEXT NOT NULL DEFAULT 'prepare',
    progress_percent  INTEGER NOT NULL DEFAULT 0,
    attempt_count     INTEGER NOT NULL DEFAULT 0,
    max_attempts      INTEGER NOT NULL DEFAULT 3,
    lease_owner       TEXT,
    lease_expires_at  TIMESTAMPTZ,
    next_retry_at     TIMESTAMPTZ,
    failure_code      TEXT,
    failure_message   TEXT,
    step_durations    JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at        TIMESTAMPTZ,
    completed_at      TIMESTAMPTZ,
    CONSTRAINT processing_runs_status_check CHECK (
        status IN ('queued', 'processing', 'retry_scheduled', 'completed', 'failed', 'dead_lettered')
    ),
    CONSTRAINT processing_runs_progress_check CHECK (progress_percent BETWEEN 0 AND 100)
);

-- At most one live run per content and processor version. This is what stops
-- two users from downloading the same video twice.
CREATE UNIQUE INDEX IF NOT EXISTS processing_runs_active_key
    ON reelpin.processing_runs (content_id, processor_version)
    WHERE status IN ('queued', 'processing', 'retry_scheduled');

CREATE INDEX IF NOT EXISTS processing_runs_lease_idx
    ON reelpin.processing_runs (lease_expires_at)
    WHERE status = 'processing';

CREATE INDEX IF NOT EXISTS processing_runs_retry_idx
    ON reelpin.processing_runs (next_retry_at)
    WHERE status = 'retry_scheduled';

-- A finished stage is not repeated on redelivery when its version and input
-- hash still match.
CREATE TABLE IF NOT EXISTS reelpin.processing_stage_results (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id        UUID NOT NULL REFERENCES reelpin.processing_runs(id) ON DELETE CASCADE,
    stage         TEXT NOT NULL,
    stage_version TEXT NOT NULL,
    input_hash    TEXT NOT NULL,
    output        JSONB NOT NULL DEFAULT '{}'::jsonb,
    artifact_refs JSONB NOT NULL DEFAULT '[]'::jsonb,
    status        TEXT NOT NULL DEFAULT 'completed',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT processing_stage_results_key UNIQUE (run_id, stage, stage_version)
);

-- Work is published from the same transaction that wrote the row it describes.
CREATE TABLE IF NOT EXISTS reelpin.outbox_events (
    event_id       UUID PRIMARY KEY,
    event_type     TEXT NOT NULL,
    routing_key    TEXT NOT NULL,
    schema_version INTEGER NOT NULL DEFAULT 1,
    payload        JSONB NOT NULL,
    attempts       INTEGER NOT NULL DEFAULT 0,
    available_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at   TIMESTAMPTZ,
    last_error     TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS outbox_events_pending_idx
    ON reelpin.outbox_events (available_at, event_id)
    WHERE published_at IS NULL;

-- A provider that pushed back is left alone until this expires.
CREATE TABLE IF NOT EXISTS reelpin.provider_cooldowns (
    platform       TEXT PRIMARY KEY,
    cooldown_until TIMESTAMPTZ NOT NULL,
    reason         TEXT NOT NULL DEFAULT '',
    source_run_id  UUID REFERENCES reelpin.processing_runs(id) ON DELETE SET NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Expand the existing tables. Both columns stay nullable: nothing reads them
-- yet, and old inserts must keep working. The delete rules are what stop one
-- user's deletion from removing content another user still references.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = 'public' AND tablename = 'reels') THEN
        ALTER TABLE public.reels ADD COLUMN IF NOT EXISTS content_version_id UUID;

        IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'reels_content_version_fk') THEN
            ALTER TABLE public.reels
                ADD CONSTRAINT reels_content_version_fk
                FOREIGN KEY (content_version_id)
                REFERENCES reelpin.content_versions(id) ON DELETE SET NULL;
        END IF;

        CREATE INDEX IF NOT EXISTS reels_content_version_idx
            ON public.reels (content_version_id);
    END IF;

    IF EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = 'public' AND tablename = 'processing_jobs') THEN
        ALTER TABLE public.processing_jobs ADD COLUMN IF NOT EXISTS processing_run_id UUID;

        IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'processing_jobs_run_fk') THEN
            ALTER TABLE public.processing_jobs
                ADD CONSTRAINT processing_jobs_run_fk
                FOREIGN KEY (processing_run_id)
                REFERENCES reelpin.processing_runs(id) ON DELETE SET NULL;
        END IF;

        CREATE INDEX IF NOT EXISTS processing_jobs_run_idx
            ON public.processing_jobs (processing_run_id);
    END IF;
END
$$;

-- The API roles reach this data through the service, never directly.
DO $$
DECLARE
    role_name TEXT;
BEGIN
    FOREACH role_name IN ARRAY ARRAY['anon', 'authenticated'] LOOP
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = role_name) THEN
            EXECUTE format('REVOKE ALL ON SCHEMA reelpin FROM %I', role_name);
            EXECUTE format('REVOKE ALL ON ALL TABLES IN SCHEMA reelpin FROM %I', role_name);
            EXECUTE format(
                'ALTER DEFAULT PRIVILEGES IN SCHEMA reelpin REVOKE ALL ON TABLES FROM %I',
                role_name
            );
        END IF;
    END LOOP;
END
$$;

-- migrate:down

-- Expand-only migrations are not rolled back in production; this exists so a
-- disposable test database can be reset.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'reels_content_version_fk') THEN
        ALTER TABLE public.reels DROP CONSTRAINT reels_content_version_fk;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'processing_jobs_run_fk') THEN
        ALTER TABLE public.processing_jobs DROP CONSTRAINT processing_jobs_run_fk;
    END IF;
END
$$;

DROP SCHEMA IF EXISTS reelpin CASCADE;

