-- migrate:up

-- A manual pin may be backed by a Google place the user picked from search.
-- The place id is what makes pinning idempotent: pinning the same place twice
-- returns the first pin instead of dropping a second one on top of it. Pins
-- dropped by hand carry no place id, and NULLs never collide in the unique
-- constraint, so they are untouched by it.
ALTER TABLE reelpin.user_manual_pins
    ADD COLUMN google_place_id      TEXT,
    ADD COLUMN google_maps_url      TEXT,
    ADD COLUMN category             TEXT,
    ADD COLUMN subcategory          TEXT,
    ADD COLUMN secondary_categories TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN place_types          TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN metadata             JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD CONSTRAINT user_manual_pins_google_place_key UNIQUE (user_id, google_place_id);

-- migrate:down

ALTER TABLE reelpin.user_manual_pins
    DROP CONSTRAINT IF EXISTS user_manual_pins_google_place_key,
    DROP COLUMN IF EXISTS metadata,
    DROP COLUMN IF EXISTS place_types,
    DROP COLUMN IF EXISTS secondary_categories,
    DROP COLUMN IF EXISTS subcategory,
    DROP COLUMN IF EXISTS category,
    DROP COLUMN IF EXISTS google_maps_url,
    DROP COLUMN IF EXISTS google_place_id;
