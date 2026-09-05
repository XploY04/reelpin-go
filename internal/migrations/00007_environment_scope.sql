-- migrate:up

-- Dev and production share one Supabase project, separated only by a column.
-- Processing had no such column, so a dev dispatcher could claim a production
-- outbox event and publish it onto the dev broker, where a dev worker would run
-- it against production rows.
--
-- Expand-only: the column is added with a default so existing rows keep
-- working, and every claim path filters on it.

ALTER TABLE reelpin.processing_runs
    ADD COLUMN IF NOT EXISTS environment TEXT NOT NULL DEFAULT 'production';

ALTER TABLE reelpin.outbox_events
    ADD COLUMN IF NOT EXISTS environment TEXT NOT NULL DEFAULT 'production';

ALTER TABLE reelpin.provider_cooldowns
    ADD COLUMN IF NOT EXISTS environment TEXT NOT NULL DEFAULT 'production';

-- Existing rows were all written by one deployment before this column existed.
-- 'production' is the safe default: a dev process will not claim them, and a
-- production process picks up exactly what it left behind.

-- A live run is one per content, processor version AND environment. Without
-- the environment, a dev run would make production think the work was already
-- in flight, and production shares would silently attach to a dev run.
DROP INDEX IF EXISTS reelpin.processing_runs_active_key;
CREATE UNIQUE INDEX IF NOT EXISTS processing_runs_active_key
    ON reelpin.processing_runs (content_id, processor_version, environment)
    WHERE status IN ('queued', 'processing', 'retry_scheduled');

-- The claim indexes carry the environment first, because every claim filters
-- on it before anything else.
DROP INDEX IF EXISTS reelpin.processing_runs_lease_idx;
CREATE INDEX IF NOT EXISTS processing_runs_lease_idx
    ON reelpin.processing_runs (environment, lease_expires_at)
    WHERE status = 'processing';

DROP INDEX IF EXISTS reelpin.processing_runs_retry_idx;
CREATE INDEX IF NOT EXISTS processing_runs_retry_idx
    ON reelpin.processing_runs (environment, next_retry_at)
    WHERE status = 'retry_scheduled';

DROP INDEX IF EXISTS reelpin.outbox_events_pending_idx;
CREATE INDEX IF NOT EXISTS outbox_events_pending_idx
    ON reelpin.outbox_events (environment, available_at, event_id)
    WHERE published_at IS NULL;

-- A cooldown is per provider per environment: dev exhausting a test key must
-- not stop production, and production's cooldown must not silence dev.
ALTER TABLE reelpin.provider_cooldowns
    DROP CONSTRAINT IF EXISTS provider_cooldowns_pkey;
ALTER TABLE reelpin.provider_cooldowns
    ADD CONSTRAINT provider_cooldowns_pkey PRIMARY KEY (platform, environment);

-- migrate:down

-- Restores the unscoped shape. Only ever used against a disposable database:
-- in production this is corrected forward, never rolled back.
ALTER TABLE reelpin.provider_cooldowns
    DROP CONSTRAINT IF EXISTS provider_cooldowns_pkey;
ALTER TABLE reelpin.provider_cooldowns
    ADD CONSTRAINT provider_cooldowns_pkey PRIMARY KEY (platform);

DROP INDEX IF EXISTS reelpin.processing_runs_active_key;
CREATE UNIQUE INDEX IF NOT EXISTS processing_runs_active_key
    ON reelpin.processing_runs (content_id, processor_version)
    WHERE status IN ('queued', 'processing', 'retry_scheduled');

DROP INDEX IF EXISTS reelpin.processing_runs_lease_idx;
CREATE INDEX IF NOT EXISTS processing_runs_lease_idx
    ON reelpin.processing_runs (lease_expires_at)
    WHERE status = 'processing';

DROP INDEX IF EXISTS reelpin.processing_runs_retry_idx;
CREATE INDEX IF NOT EXISTS processing_runs_retry_idx
    ON reelpin.processing_runs (next_retry_at)
    WHERE status = 'retry_scheduled';

DROP INDEX IF EXISTS reelpin.outbox_events_pending_idx;
CREATE INDEX IF NOT EXISTS outbox_events_pending_idx
    ON reelpin.outbox_events (available_at, event_id)
    WHERE published_at IS NULL;

ALTER TABLE reelpin.processing_runs DROP COLUMN IF EXISTS environment;
ALTER TABLE reelpin.outbox_events DROP COLUMN IF EXISTS environment;
ALTER TABLE reelpin.provider_cooldowns DROP COLUMN IF EXISTS environment;
