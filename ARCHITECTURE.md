# Architecture

What the service is, the shape it is being built into, and which parts of that
shape exist today.

## The system this belongs to

ReelPin's users share a link from Instagram, YouTube, TikTok, X, LinkedIn,
Reddit or Pinterest into the Flutter app. A backend downloads it, transcribes
it, extracts structured detail, categorises it and saves it, and the app reads
it back as a feed, a map, and a search index.

That backend is `reelpin-api` (Python/FastAPI) and it serves every real user
today. This repository is a rewrite of it in Go. **Nothing here is in the
request path of a real user.**

Both services talk to the same Supabase project, and dev and production share
that one project, separated by an `environment` column. Anything that claims
work has to respect that column.

## Shape

A modular monolith in one repository, built as two binaries, with the domain in
the middle and everything replaceable at the edges.

```
                    cmd/api                     cmd/worker
                       │                            │
              ┌────────┴────────┐                   │      (later)
              │ internal/httpapi│                   │
              └────────┬────────┘                   │
                       │  reader ports              │
        ┌──────────────┴──────────────┐             │
        │ internal/reels  internal/jobs│  ← domain: no transport, no driver
        └──────────────┬──────────────┘             │
                       │                            │
              ┌────────┴────────┐                   │
              │ internal/postgres│  ← the only place SQL lives
              └────────┬────────┘                   │
                       │                            │
                   PostgreSQL ─────────────────────┘
```

The rule that keeps this honest: **dependencies point inward.** `internal/reels`
and `internal/jobs` define what they need as interfaces (`ReelReader`,
`JobReader`); `internal/postgres` implements them; `internal/httpapi` consumes
the interfaces and has never heard of pgx. `cmd/api` is the only place that
knows all three exist, and it does nothing but wire them together.

`make check` enforces the direction, so this diagram cannot quietly stop being
true. See [`docs/decisions/0001-layered-packages.md`](docs/decisions/0001-layered-packages.md).

## What exists today

- `cmd/api`, serving three health endpoints and eight authenticated read
  endpoints. See [`docs/api-contract.md`](docs/api-contract.md).
- Supabase JWT verification against the project's JWKS, done locally in
  middleware. No network call per request.
- PostgreSQL reads through pgx, scoped by the token's user id, with no writes at
  all.

## What is planned, and is not here

Named so that nobody looks for them: `cmd/worker`, the processing pipeline,
RabbitMQ and the transactional outbox, Redis rate limits and caches, the global
content model that lets two users share one download, embeddings and hybrid
search, and the production cutover. Each arrives as its own reviewed layer.

Where a planned piece changes a rule in this document, the document changes in
the same pull request.

## Why Go, and why a rewrite

The Python service works. The rewrite exists to get a typed, compiled service
with a real queue, one download per piece of content instead of one per user,
and search that is actually semantic rather than a hashed bag of words. Those
are the four things the current design cannot reach without being rebuilt
anyway.
