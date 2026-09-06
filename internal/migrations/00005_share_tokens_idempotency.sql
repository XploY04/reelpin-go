-- migrate:up

-- Native share tokens: the credential a share extension presents. The token
-- value itself never lands here; only its SHA-256 does, so a database read
-- cannot impersonate a device.
CREATE TABLE reelpin.native_share_tokens (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID NOT NULL REFERENCES auth.users(id) ON DELETE CASCADE,
    token_hash  TEXT NOT NULL UNIQUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL,
    revoked_at  TIMESTAMPTZ
);

CREATE INDEX native_share_tokens_user_idx ON reelpin.native_share_tokens (user_id);

GRANT SELECT, INSERT, UPDATE ON reelpin.native_share_tokens TO reelpin_app;
GRANT SELECT, DELETE ON reelpin.native_share_tokens TO reelpin_maintenance;

-- migrate:down

DROP TABLE reelpin.native_share_tokens;
