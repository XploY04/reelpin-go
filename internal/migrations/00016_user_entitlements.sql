-- migrate:up

-- A user's Pro grant. There is no subscriptions system yet: Pro is granted by
-- hand (store = 'manual'), and the API treats a row whose pro_expires_at is in
-- the future as Pro. One row per user; a renewal overwrites it, and deleting
-- the identity deletes the row.
CREATE TABLE reelpin.user_entitlements (
    user_id        UUID PRIMARY KEY REFERENCES auth.users(id) ON DELETE CASCADE,
    pro_expires_at TIMESTAMPTZ,
    product_id     TEXT,
    store          TEXT,
    is_sandbox     BOOLEAN NOT NULL DEFAULT FALSE,
    synced_at      TIMESTAMPTZ,
    note           TEXT
);

-- The read path wants only the rows still in force, ordered by nothing: the
-- cache loads the whole active set at once.
CREATE INDEX idx_user_entitlements_pro_expires_at
    ON reelpin.user_entitlements (pro_expires_at);

GRANT SELECT, INSERT, UPDATE, DELETE ON reelpin.user_entitlements TO reelpin_app;
GRANT SELECT, DELETE ON reelpin.user_entitlements TO reelpin_maintenance;

-- migrate:down

DROP TABLE IF EXISTS reelpin.user_entitlements;
