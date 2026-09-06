-- migrate:up

-- Every provider call that costs money, priced at the moment it was made. The
-- cost gate sums the current calendar month from here, so a row that is missing
-- is spending nothing can see. It holds no user data: a provider, a model, an
-- operation and counts.
CREATE TABLE reelpin.provider_usage (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    occurred_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    provider      TEXT NOT NULL,
    model         TEXT NOT NULL,
    operation     TEXT NOT NULL,
    calls         INTEGER NOT NULL CHECK (calls > 0),
    input_tokens  BIGINT NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    output_tokens BIGINT NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    -- False when the provider reported no usage, so the row is a call count
    -- rather than a measured one.
    measured      BOOLEAN NOT NULL,
    -- Micros of one US dollar, at the rates in force when the call was made:
    -- changing a rate must not rewrite what an earlier month cost.
    cost_micros   BIGINT NOT NULL CHECK (cost_micros >= 0),
    -- False when no configured price covered the call. The row is still here,
    -- because unpriced spending is worse to lose than to see valued at zero.
    priced        BOOLEAN NOT NULL
);

-- The gate's only query is a sum over one month, on every submission.
CREATE INDEX provider_usage_occurred_at_idx ON reelpin.provider_usage (occurred_at);

-- The worker writes it, the API reads it to decide whether to accept work.
GRANT SELECT, INSERT ON reelpin.provider_usage TO reelpin_app;
GRANT SELECT, DELETE ON reelpin.provider_usage TO reelpin_maintenance;

-- migrate:down

DROP TABLE IF EXISTS reelpin.provider_usage;
