-- migrate:up

CREATE EXTENSION IF NOT EXISTS vector SCHEMA public;

-- Embeddings live beside content versions rather than on them.
--
-- The plan asks for embedding columns on content_versions, but those rows are
-- immutable by trigger and by grant, and an embedding is derived data written
-- minutes later by a different worker. Weakening immutability to admit "just
-- these columns" is the kind of exception that grows; a sidecar keeps the
-- guarantee absolute and makes a future dimension change a new table rather
-- than a new column on a frozen row.
CREATE TABLE reelpin.content_embeddings (
    content_version_id UUID PRIMARY KEY
        REFERENCES reelpin.content_versions(id) ON DELETE CASCADE,
    -- A vector is only comparable with others made the same way, so how it was
    -- made is pinned on the row. Nothing lets a query mix two sets by accident.
    embedding          public.vector(768),
    model              TEXT NOT NULL,
    dimension          INTEGER NOT NULL
        CONSTRAINT content_embeddings_dimension_check CHECK (dimension = 768),
    document_version   TEXT NOT NULL,
    -- The hash of the exact text embedded. Re-running with the same document,
    -- model and dimension is free: the hash matches and no provider is called.
    document_hash      TEXT NOT NULL,
    embedded_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX content_embeddings_vector_idx
    ON reelpin.content_embeddings
    USING hnsw (embedding public.vector_cosine_ops)
    WHERE embedding IS NOT NULL;

-- What still needs embedding, cheap for the backfill to scan.
CREATE INDEX content_embeddings_model_idx
    ON reelpin.content_embeddings (model, document_version);

GRANT SELECT, INSERT, UPDATE, DELETE ON reelpin.content_embeddings TO reelpin_app;
GRANT SELECT, DELETE ON reelpin.content_embeddings TO reelpin_maintenance;

-- migrate:down

DROP TABLE reelpin.content_embeddings;
