-- migrate:up

-- One row per user, keyed by the auth user id so the id a client already has is
-- the stable id of its profile. The app used to upsert this straight into
-- Supabase; the API owns the write now.
CREATE TABLE reelpin.profiles (
    user_id    UUID PRIMARY KEY REFERENCES auth.users(id) ON DELETE CASCADE,
    email      TEXT,
    full_name  TEXT,
    avatar_url TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

GRANT SELECT, INSERT, UPDATE ON reelpin.profiles TO reelpin_app;
GRANT SELECT, DELETE ON reelpin.profiles TO reelpin_maintenance;

-- migrate:down

DROP TABLE reelpin.profiles;
