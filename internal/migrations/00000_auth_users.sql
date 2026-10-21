-- migrate:up

-- A mirror of the identity provider's users, keyed by the Supabase user id, so
-- every user-owned table has a foreign key parent on a database that is not
-- Supabase. On Supabase the table already exists and this only grants.
CREATE SCHEMA IF NOT EXISTS auth;

CREATE TABLE IF NOT EXISTS auth.users (
    id         UUID PRIMARY KEY,
    email      TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

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

-- The app materialises a user on their first request; only account deletion,
-- a maintenance operation, removes one.
GRANT USAGE ON SCHEMA auth TO reelpin_app, reelpin_maintenance;
GRANT SELECT, INSERT, UPDATE ON auth.users TO reelpin_app;
GRANT SELECT, DELETE ON auth.users TO reelpin_maintenance;

-- migrate:down

-- Nothing is dropped: on Supabase the schema and the table belong to Supabase,
-- and this migration cannot tell which database it is running on.
