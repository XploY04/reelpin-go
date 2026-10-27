# 7. Use PostgreSQL as the system of record

**Status:** accepted

## Context

ReelPin already stores production users and saved content in Supabase
PostgreSQL. The product needs ownership constraints, transactions, geospatial
queries, lexical search and vector search over the same records.

## Decision

Use Supabase PostgreSQL as the only product system of record. Keep stable query
fields in typed columns and versioned provider extraction in JSONB. Use PostGIS,
PostgreSQL full-text search, `pg_trgm` and pgvector when their tasks reach their
measurement gates.

Redis contains disposable limits and health state. RabbitMQ transports work.
Neither becomes the source of truth.

## Consequences

Ownership, global deduplication, job state and outbox publication can share one
transaction. Existing production data stays where it is.

The database takes more kinds of reads than a split MongoDB and search-service
design. A separate data store is considered only after query plans, load tests
or operating cost show a measured PostgreSQL limit.
