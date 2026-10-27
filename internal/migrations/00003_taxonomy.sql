-- migrate:up

-- The active category tree the extraction prompt is built from. Subcategories
-- are rows whose parent is a top-level category.
CREATE TABLE reelpin.categories (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    parent_id       UUID REFERENCES reelpin.categories(id) ON DELETE RESTRICT,
    name            TEXT NOT NULL,
    normalized_name TEXT NOT NULL,
    description     TEXT NOT NULL DEFAULT '',
    active          BOOLEAN NOT NULL DEFAULT true,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT categories_normalized_key UNIQUE (normalized_name)
);

CREATE INDEX categories_parent_idx ON reelpin.categories (parent_id);

-- A merged or renamed category leaves its old names behind as aliases, so
-- existing extractions keep resolving.
CREATE TABLE reelpin.category_aliases (
    normalized_alias TEXT PRIMARY KEY,
    category_id      UUID NOT NULL REFERENCES reelpin.categories(id) ON DELETE CASCADE,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX category_aliases_category_idx ON reelpin.category_aliases (category_id);

-- What the model wanted and could not have. A processing job proposes; only
-- the weekly curator can activate.
CREATE TABLE reelpin.category_proposals (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    proposed_name   TEXT NOT NULL,
    normalized_name TEXT NOT NULL,
    description     TEXT NOT NULL DEFAULT '',
    source_run_id   UUID REFERENCES reelpin.processing_runs(id) ON DELETE SET NULL,
    status          TEXT NOT NULL DEFAULT 'pending'
        CONSTRAINT category_proposals_status_check CHECK (status IN
            ('pending', 'approved', 'rejected', 'merged')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX category_proposals_status_idx ON reelpin.category_proposals (status, normalized_name);
CREATE INDEX category_proposals_run_idx ON reelpin.category_proposals (source_run_id);

-- One weekly curation run: its exact input, decision and the inverse action
-- that rolls it back. History is append-only.
CREATE TABLE reelpin.taxonomy_runs (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model          TEXT NOT NULL,
    prompt_version TEXT NOT NULL,
    input          JSONB NOT NULL,
    decision       JSONB NOT NULL,
    rollback       JSONB NOT NULL,
    applied        BOOLEAN NOT NULL DEFAULT false,
    ran_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

GRANT SELECT, INSERT ON reelpin.category_proposals TO reelpin_app;
GRANT SELECT ON reelpin.categories TO reelpin_app;
GRANT SELECT ON reelpin.category_aliases TO reelpin_app;
GRANT SELECT ON reelpin.taxonomy_runs TO reelpin_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON reelpin.categories TO reelpin_maintenance;
GRANT SELECT, INSERT, UPDATE, DELETE ON reelpin.category_aliases TO reelpin_maintenance;
GRANT SELECT, UPDATE, DELETE ON reelpin.category_proposals TO reelpin_maintenance;
GRANT SELECT, INSERT, UPDATE ON reelpin.taxonomy_runs TO reelpin_maintenance;

-- migrate:down

DROP TABLE IF EXISTS reelpin.taxonomy_runs;
DROP TABLE IF EXISTS reelpin.category_proposals;
DROP TABLE IF EXISTS reelpin.category_aliases;
DROP TABLE IF EXISTS reelpin.categories;
