-- migrate:up

-- One global execution of one content identity under one processor version.
-- Two users sharing the same public link share this row; their private jobs
-- point at it.
CREATE TABLE reelpin.processing_runs (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    content_id        UUID NOT NULL REFERENCES reelpin.contents(id) ON DELETE CASCADE,
    processor_version TEXT NOT NULL,
    status            TEXT NOT NULL DEFAULT 'queued'
        CONSTRAINT processing_runs_status_check CHECK (status IN
            ('queued', 'processing', 'retry_scheduled', 'completed', 'failed')),
    stage             TEXT NOT NULL DEFAULT 'prepare',
    attempt_count     INTEGER NOT NULL DEFAULT 0,
    max_attempts      INTEGER NOT NULL DEFAULT 3,
    -- The lease. The generation only ever grows: a worker that lost its lease
    -- carries a stale generation, and every state commit matches on it, so the
    -- fenced worker's writes touch zero rows.
    lease_owner       TEXT,
    lease_expires_at  TIMESTAMPTZ,
    lease_generation  BIGINT NOT NULL DEFAULT 0,
    failure_code      TEXT,
    failure_message   TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- At most one live run per content and processor version: this is what stops
-- two users from downloading the same video twice.
CREATE UNIQUE INDEX processing_runs_active_key
    ON reelpin.processing_runs (content_id, processor_version)
    WHERE status IN ('queued', 'processing', 'retry_scheduled');

CREATE INDEX processing_runs_content_idx ON reelpin.processing_runs (content_id);
CREATE INDEX processing_runs_lease_idx
    ON reelpin.processing_runs (lease_expires_at)
    WHERE status = 'processing';

-- One private, user-visible job. It is what the app polls; it never carries
-- another user's information.
CREATE TABLE reelpin.processing_jobs (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id          UUID NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
    run_id           UUID REFERENCES reelpin.processing_runs(id) ON DELETE SET NULL,
    user_save_id     UUID REFERENCES reelpin.user_saves(id) ON DELETE SET NULL,
    url              TEXT NOT NULL,
    normalized_url   TEXT NOT NULL,
    source_platform  TEXT,
    status           TEXT NOT NULL DEFAULT 'queued'
        CONSTRAINT processing_jobs_status_check CHECK (status IN
            ('queued', 'processing', 'completed', 'failed', 'dead_lettered')),
    current_step     TEXT,
    progress_percent INTEGER NOT NULL DEFAULT 0
        CONSTRAINT processing_jobs_progress_check CHECK (progress_percent BETWEEN 0 AND 100),
    failure_code     TEXT,
    collection_ids   UUID[] NOT NULL DEFAULT '{}',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at     TIMESTAMPTZ
);

CREATE INDEX processing_jobs_user_idx ON reelpin.processing_jobs (user_id, created_at DESC);
CREATE INDEX processing_jobs_run_idx ON reelpin.processing_jobs (run_id);
CREATE INDEX processing_jobs_save_idx ON reelpin.processing_jobs (user_save_id);
-- The active-job cap counts these under a per-user advisory lock.
CREATE INDEX processing_jobs_active_idx
    ON reelpin.processing_jobs (user_id)
    WHERE status IN ('queued', 'processing');

-- One stage attempt's durable result. A finished stage is not repeated on
-- redelivery while its version and input hash still match.
CREATE TABLE reelpin.processing_stage_results (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id        UUID NOT NULL REFERENCES reelpin.processing_runs(id) ON DELETE CASCADE,
    stage         TEXT NOT NULL,
    stage_version TEXT NOT NULL,
    input_hash    TEXT NOT NULL,
    output_ref    TEXT NOT NULL DEFAULT '',
    attempt_count INTEGER NOT NULL DEFAULT 1,
    started_at    TIMESTAMPTZ,
    finished_at   TIMESTAMPTZ,
    error_class   TEXT
        CONSTRAINT stage_results_error_class_check CHECK (error_class IS NULL OR error_class IN
            ('transient', 'provider_exhausted', 'content_terminal', 'internal')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT stage_results_key UNIQUE (run_id, stage, stage_version)
);

-- Work is published from the same transaction that wrote the row it describes.
CREATE TABLE reelpin.outbox_events (
    event_id       UUID PRIMARY KEY,
    event_type     TEXT NOT NULL,
    routing_key    TEXT NOT NULL,
    schema_version INTEGER NOT NULL DEFAULT 1,
    payload        JSONB NOT NULL,
    available_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    attempts       INTEGER NOT NULL DEFAULT 0,
    published_at   TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX outbox_events_pending_idx
    ON reelpin.outbox_events (available_at, event_id)
    WHERE published_at IS NULL;

-- One submission attempt's stored outcome. Retrying with the same key returns
-- the same answer; the request hash is what turns key reuse into a 409.
CREATE TABLE reelpin.idempotency_keys (
    user_id         UUID NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
    endpoint        TEXT NOT NULL,
    idempotency_key UUID NOT NULL,
    request_hash    TEXT NOT NULL,
    response_status INTEGER,
    response_body   JSONB,
    expires_at      TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT idempotency_keys_pkey PRIMARY KEY (user_id, endpoint, idempotency_key)
);

CREATE INDEX idempotency_keys_expiry_idx ON reelpin.idempotency_keys (expires_at);

-- Deliberately no foreign key to auth.users: this row must outlive the
-- identity it is deleting, and it is the durable record that deletion was
-- requested if anything crashes mid-way.
CREATE TABLE reelpin.account_deletion_requests (
    user_id                UUID PRIMARY KEY,
    requested_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    database_cleanup_state TEXT NOT NULL DEFAULT 'pending'
        CONSTRAINT deletion_db_state_check CHECK (database_cleanup_state IN
            ('pending', 'running', 'done')),
    identity_cleanup_state TEXT NOT NULL DEFAULT 'pending'
        CONSTRAINT deletion_identity_state_check CHECK (identity_cleanup_state IN
            ('pending', 'done')),
    attempts               INTEGER NOT NULL DEFAULT 0,
    last_error_class       TEXT,
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);

GRANT SELECT, INSERT, UPDATE ON reelpin.processing_runs TO reelpin_app;
GRANT SELECT, INSERT, UPDATE ON reelpin.processing_jobs TO reelpin_app;
GRANT SELECT, INSERT, UPDATE ON reelpin.processing_stage_results TO reelpin_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON reelpin.outbox_events TO reelpin_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON reelpin.idempotency_keys TO reelpin_app;
GRANT SELECT, INSERT, UPDATE ON reelpin.account_deletion_requests TO reelpin_app;
GRANT SELECT, DELETE ON reelpin.processing_runs TO reelpin_maintenance;
GRANT SELECT, DELETE ON reelpin.processing_jobs TO reelpin_maintenance;
GRANT SELECT, DELETE ON reelpin.processing_stage_results TO reelpin_maintenance;
GRANT SELECT, DELETE ON reelpin.outbox_events TO reelpin_maintenance;
GRANT SELECT, DELETE ON reelpin.idempotency_keys TO reelpin_maintenance;
GRANT SELECT, UPDATE, DELETE ON reelpin.account_deletion_requests TO reelpin_maintenance;

-- migrate:down

DROP TABLE IF EXISTS reelpin.account_deletion_requests;
DROP TABLE IF EXISTS reelpin.idempotency_keys;
DROP TABLE IF EXISTS reelpin.outbox_events;
DROP TABLE IF EXISTS reelpin.processing_stage_results;
DROP TABLE IF EXISTS reelpin.processing_jobs;
DROP TABLE IF EXISTS reelpin.processing_runs;
