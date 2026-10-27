-- migrate:up

-- A device token is a credential: it is stored here, never logged, never
-- returned by an API, and never placed in an error message.
CREATE TABLE reelpin.device_push_tokens (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      UUID NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
    fcm_token    TEXT NOT NULL,
    platform     TEXT NOT NULL
        CONSTRAINT device_push_tokens_platform_check CHECK (platform IN ('ios', 'android', 'web')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT device_push_tokens_token_key UNIQUE (fcm_token)
);

CREATE INDEX device_push_tokens_user_idx ON reelpin.device_push_tokens (user_id);

-- One row per logical notification. event_key is what makes a redelivered
-- outbox message or a retried request produce one buzz rather than two: the
-- unique constraint is the deduplication, not application logic.
CREATE TABLE reelpin.notifications (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
    event_key       TEXT NOT NULL,
    kind            TEXT NOT NULL,
    title           TEXT NOT NULL,
    body            TEXT NOT NULL,
    data            JSONB NOT NULL DEFAULT '{}'::jsonb,
    status          TEXT NOT NULL DEFAULT 'pending'
        CONSTRAINT notifications_status_check CHECK (status IN
            ('pending', 'sent', 'failed', 'no_devices')),
    provider_message_id TEXT,
    failure_reason  TEXT,
    opened_at       TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT notifications_event_key UNIQUE (event_key)
);

CREATE INDEX notifications_user_idx ON reelpin.notifications (user_id, created_at DESC);

GRANT SELECT, INSERT, UPDATE, DELETE ON reelpin.device_push_tokens TO reelpin_app;
GRANT SELECT, INSERT, UPDATE ON reelpin.notifications TO reelpin_app;
GRANT SELECT, DELETE ON reelpin.device_push_tokens TO reelpin_maintenance;
GRANT SELECT, DELETE ON reelpin.notifications TO reelpin_maintenance;

-- migrate:down

DROP TABLE reelpin.notifications;
DROP TABLE reelpin.device_push_tokens;
