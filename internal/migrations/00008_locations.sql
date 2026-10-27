-- migrate:up

-- Explicitly in public: an extension follows search_path, and PostGIS landing
-- inside the reelpin schema makes that schema undroppable in a disposable
-- database and couples an extension's lifetime to ours.
CREATE EXTENSION IF NOT EXISTS postgis SCHEMA public;

-- Where a piece of content is, extracted once and shared by everyone who saved
-- it. The point is geography, not geometry: distances are metres on a sphere,
-- which is what a map query actually asks for.
CREATE TABLE reelpin.content_locations (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    content_version_id UUID NOT NULL REFERENCES reelpin.content_versions(id) ON DELETE CASCADE,
    ordinal            INTEGER NOT NULL,
    name               TEXT NOT NULL,
    address            TEXT,
    locality           TEXT,
    country            TEXT,
    point              public.geography(Point, 4326) NOT NULL,
    source             TEXT NOT NULL
        CONSTRAINT content_locations_source_check CHECK (source IN
            ('extraction', 'provider', 'manual')),
    -- How much to trust it. An extraction guess and a geocoded address are not
    -- the same claim, and the map may want to show only the confident ones.
    confidence         REAL NOT NULL DEFAULT 0.5
        CONSTRAINT content_locations_confidence_check CHECK (confidence BETWEEN 0 AND 1),
    provider_place_id  TEXT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT content_locations_ordinal_key UNIQUE (content_version_id, ordinal)
);

CREATE INDEX content_locations_point_idx ON reelpin.content_locations USING GIST (point);
CREATE INDEX content_locations_version_idx ON reelpin.content_locations (content_version_id);

-- A user's own choices about pins. Hiding a pin is personal: it must never
-- affect what another user sees of the same content.
CREATE TABLE reelpin.user_pin_preferences (
    user_id     UUID NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
    location_id UUID NOT NULL REFERENCES reelpin.content_locations(id) ON DELETE CASCADE,
    hidden      BOOLEAN NOT NULL DEFAULT false,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT user_pin_preferences_pkey PRIMARY KEY (user_id, location_id)
);

-- A pin the user dropped themselves, owned by them and shared with nobody.
CREATE TABLE reelpin.user_manual_pins (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id           UUID NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
    name              TEXT NOT NULL,
    address           TEXT,
    point             public.geography(Point, 4326) NOT NULL,
    provider_place_id TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX user_manual_pins_user_idx ON reelpin.user_manual_pins (user_id);
CREATE INDEX user_manual_pins_point_idx ON reelpin.user_manual_pins USING GIST (point);

-- Provider place lookups, cached because they cost money and never contain
-- user data: a place is a fact about the world, not about who searched for it.
CREATE TABLE reelpin.place_lookups (
    query_hash  TEXT PRIMARY KEY,
    response    JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL
);

CREATE INDEX place_lookups_expiry_idx ON reelpin.place_lookups (expires_at);

GRANT SELECT, INSERT, UPDATE, DELETE ON reelpin.content_locations TO reelpin_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON reelpin.user_pin_preferences TO reelpin_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON reelpin.user_manual_pins TO reelpin_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON reelpin.place_lookups TO reelpin_app;
GRANT SELECT, DELETE ON reelpin.content_locations TO reelpin_maintenance;
GRANT SELECT, DELETE ON reelpin.user_pin_preferences TO reelpin_maintenance;
GRANT SELECT, DELETE ON reelpin.user_manual_pins TO reelpin_maintenance;
GRANT SELECT, DELETE ON reelpin.place_lookups TO reelpin_maintenance;

-- migrate:down

DROP TABLE reelpin.place_lookups;
DROP TABLE reelpin.user_manual_pins;
DROP TABLE reelpin.user_pin_preferences;
DROP TABLE reelpin.content_locations;
