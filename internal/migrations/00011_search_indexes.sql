-- migrate:up

-- Trigram matching for the fuzzy arm. The extension goes in public: an
-- extension owned by the reelpin schema makes that schema undroppable, which
-- breaks every per-test database teardown.
CREATE EXTENSION IF NOT EXISTS pg_trgm SCHEMA public;

-- The lexical arm's document, weighted so a title match outranks a summary
-- match and a summary match outranks a caption match.
--
-- The transcript is left out on purpose: it matches nearly every query and
-- ranks none of them. Tags and key facts are left out for a different reason,
-- which is that array_to_string is only STABLE and a generated column needs an
-- IMMUTABLE expression. For the same reason the text search configuration is
-- cast rather than named: to_tsvector(regconfig, text) is immutable while
-- to_tsvector(text, text) is not.
ALTER TABLE reelpin.content_versions
    ADD COLUMN search_document tsvector
    GENERATED ALWAYS AS (
        setweight(to_tsvector('english'::regconfig, coalesce(title, '')), 'A') ||
        setweight(to_tsvector('english'::regconfig, coalesce(summary, '')), 'B') ||
        setweight(to_tsvector('english'::regconfig, coalesce(caption, '')), 'C')
    ) STORED;

CREATE INDEX content_versions_search_idx
    ON reelpin.content_versions USING GIN (search_document);

-- migrate:down

DROP INDEX IF EXISTS reelpin.content_versions_search_idx;
ALTER TABLE reelpin.content_versions DROP COLUMN search_document;
