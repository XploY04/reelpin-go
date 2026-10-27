-- migrate:up

-- A price watch on a product page. A daily maintenance run re-reads the page's
-- structured data and pushes once when the price reaches the target;
-- notified_at is what keeps it to once.
CREATE TABLE reelpin.price_tracks (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
    save_id         UUID REFERENCES reelpin.user_saves(id) ON DELETE CASCADE,
    url             TEXT NOT NULL,
    title           TEXT NOT NULL DEFAULT '',
    target_price    NUMERIC NOT NULL
        CONSTRAINT price_tracks_target_price_check CHECK (target_price > 0),
    currency        TEXT NOT NULL DEFAULT '',
    last_price      NUMERIC,
    active          BOOLEAN NOT NULL DEFAULT true,
    last_checked_at TIMESTAMPTZ,
    notified_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX price_tracks_user_idx ON reelpin.price_tracks (user_id, created_at DESC);
CREATE INDEX price_tracks_check_idx ON reelpin.price_tracks (last_checked_at NULLS FIRST) WHERE active;

GRANT SELECT, INSERT, UPDATE, DELETE ON reelpin.price_tracks TO reelpin_app;
GRANT SELECT, UPDATE, DELETE ON reelpin.price_tracks TO reelpin_maintenance;

-- migrate:down

DROP TABLE reelpin.price_tracks;
