-- migrate:up

-- A reminder a user set, on one of their saves or on nothing at all. The app
-- computes fire_at as a UTC instant. This table is the record; when a reminder
-- is due is answered by a Redis sorted set the API writes on create, so no
-- process sweeps this table on a timer. The partial index serves the one read
-- that needs every pending row: a worker re-seeding Redis when it starts.
CREATE TABLE reelpin.reminders (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
    -- The public reel id is the save id, so a reminder goes when its save does.
    save_id    UUID REFERENCES reelpin.user_saves(id) ON DELETE CASCADE,
    title      TEXT NOT NULL,
    body       TEXT NOT NULL DEFAULT '',
    fire_at    TIMESTAMPTZ NOT NULL,
    status     TEXT NOT NULL DEFAULT 'pending'
        CONSTRAINT reminders_status_check CHECK (status IN ('pending', 'sent')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at    TIMESTAMPTZ
);

CREATE INDEX reminders_user_idx ON reelpin.reminders (user_id, created_at);
CREATE INDEX reminders_pending_fire_idx ON reelpin.reminders (fire_at) WHERE status = 'pending';

GRANT SELECT, INSERT, UPDATE, DELETE ON reelpin.reminders TO reelpin_app;
GRANT SELECT, DELETE ON reelpin.reminders TO reelpin_maintenance;

-- migrate:down

DROP TABLE reelpin.reminders;
