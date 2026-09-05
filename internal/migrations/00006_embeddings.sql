-- migrate:up

CREATE EXTENSION IF NOT EXISTS vector;

-- Embeddings are versioned by what produced them. A vector is only reusable
-- while the model, the dimension and the document shape all still match, so
-- those three travel with it and a change to any of them means re-embed.
ALTER TABLE reelpin.content_versions
    ADD COLUMN IF NOT EXISTS embedding vector(768),
    ADD COLUMN IF NOT EXISTS embedding_model TEXT,
    ADD COLUMN IF NOT EXISTS embedding_dimension INTEGER,
    ADD COLUMN IF NOT EXISTS embedding_document_version TEXT,
    ADD COLUMN IF NOT EXISTS embedding_content_hash TEXT,
    ADD COLUMN IF NOT EXISTS embedded_at TIMESTAMPTZ;

ALTER TABLE reelpin.content_chunks
    ADD COLUMN IF NOT EXISTS embedding vector(768),
    ADD COLUMN IF NOT EXISTS embedding_model TEXT,
    ADD COLUMN IF NOT EXISTS embedding_dimension INTEGER,
    ADD COLUMN IF NOT EXISTS embedding_document_version TEXT,
    ADD COLUMN IF NOT EXISTS embedded_at TIMESTAMPTZ;

-- Finding what still needs embedding must not scan the whole table.
CREATE INDEX IF NOT EXISTS content_versions_pending_embedding_idx
    ON reelpin.content_versions (updated_at)
    WHERE embedding IS NULL;

CREATE INDEX IF NOT EXISTS content_chunks_pending_embedding_idx
    ON reelpin.content_chunks (content_version_id)
    WHERE embedding IS NULL;

-- Lexical and fuzzy search read the same rows as vector search, so their
-- indexes live here too. Together they are the three arms of hybrid search.
CREATE INDEX IF NOT EXISTS content_versions_fts_idx
    ON reelpin.content_versions
    USING GIN (to_tsvector('english', coalesce(title, '') || ' ' || coalesce(summary, '')));

CREATE INDEX IF NOT EXISTS content_versions_title_trgm_idx
    ON reelpin.content_versions USING GIN (title gin_trgm_ops);

CREATE INDEX IF NOT EXISTS content_locations_name_trgm_idx
    ON reelpin.content_locations USING GIN (name gin_trgm_ops);

-- No HNSW index yet. Exact search is correct at this size, and an approximate
-- index is a tuning decision to make against measured latency, not upfront.

-- migrate:down

DROP INDEX IF EXISTS reelpin.content_versions_pending_embedding_idx;
DROP INDEX IF EXISTS reelpin.content_chunks_pending_embedding_idx;
DROP INDEX IF EXISTS reelpin.content_versions_fts_idx;
DROP INDEX IF EXISTS reelpin.content_versions_title_trgm_idx;
DROP INDEX IF EXISTS reelpin.content_locations_name_trgm_idx;

ALTER TABLE reelpin.content_versions
    DROP COLUMN IF EXISTS embedding,
    DROP COLUMN IF EXISTS embedding_model,
    DROP COLUMN IF EXISTS embedding_dimension,
    DROP COLUMN IF EXISTS embedding_document_version,
    DROP COLUMN IF EXISTS embedding_content_hash,
    DROP COLUMN IF EXISTS embedded_at;

ALTER TABLE reelpin.content_chunks
    DROP COLUMN IF EXISTS embedding,
    DROP COLUMN IF EXISTS embedding_model,
    DROP COLUMN IF EXISTS embedding_dimension,
    DROP COLUMN IF EXISTS embedding_document_version,
    DROP COLUMN IF EXISTS embedded_at;
