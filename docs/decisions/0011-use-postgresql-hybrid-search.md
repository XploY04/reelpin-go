# 11. Use measured PostgreSQL hybrid search

**Status:** accepted

## Context

Semantic vectors find related meaning, full-text search finds words and phrases,
and trigram search handles partial terms and spelling errors. ReelPin needs all
three, but its initial scale does not justify another search service.

## Decision

Store real Gemini embeddings in pgvector and combine dense, full-text and
trigram rankings with reciprocal rank fusion. Apply ownership and filters inside
every SQL search arm before ranking.

Embed title, summary, tags and facts, not transcripts, in the first version.
Search launches only after a labeled real-query set passes the recorded quality
and latency gates.

## Consequences

Search remains transactional with product data and needs one backup path. A
query-embedding failure can still return lexical results.

Relevance thresholds are measured, not copied from an old branch. A separate
search system is considered only after measured scale, relevance or database
load requires one.
