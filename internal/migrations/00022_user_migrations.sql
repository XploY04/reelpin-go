-- migrate:up

-- The users the Go API has served. During the dual run every user lives on
-- exactly one backend: the old app talks to Python, the updated app to Go. A
-- user's first authenticated request here moves them, and from then on the
-- legacy backfill's deltas leave their rows alone, so a Python worker touching
-- their frozen rows can never overwrite what they changed in Go.
CREATE TABLE reelpin.user_migrations (
    user_id       UUID PRIMARY KEY REFERENCES auth.users(id) ON DELETE CASCADE,
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

GRANT SELECT, INSERT ON reelpin.user_migrations TO reelpin_app;
GRANT SELECT ON reelpin.user_migrations TO reelpin_maintenance;

-- migrate:down

DROP TABLE reelpin.user_migrations;
