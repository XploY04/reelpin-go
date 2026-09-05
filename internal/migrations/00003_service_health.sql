-- migrate:up

-- One low-frequency row per component. Redis holds the per-worker heartbeats
-- with a TTL; this is the fallback a readiness check can read when Redis is the
-- thing that is down.
CREATE TABLE IF NOT EXISTS reelpin.service_health (
    component  TEXT PRIMARY KEY,
    healthy    BOOLEAN NOT NULL DEFAULT true,
    detail     JSONB NOT NULL DEFAULT '{}'::jsonb,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- migrate:down

DROP TABLE IF EXISTS reelpin.service_health;
