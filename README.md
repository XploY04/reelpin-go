# reelpin-go

The Go backend for ReelPin. Users share a link from Instagram, YouTube, TikTok,
X, LinkedIn, Reddit or Pinterest into the app; the backend downloads it,
transcribes it, extracts structured detail, categorises it and saves it, and the
app reads it back as a feed, a map and a search index.

**This service does not serve real users yet.** They are on `reelpin-api`
(Python/FastAPI). This is a rewrite, built one reviewable layer at a time.

## Quick start

```sh
docker compose up -d          # postgres on :5432
go run ./cmd/api              # :8000 unless PORT says otherwise
make check                    # what CI runs
```

## Where things are

- **[`AGENTS.md`](AGENTS.md)** — the short map: commands, layout, rules, traps.
  Read this first. `CLAUDE.md` is a symlink to it.
- **[`ARCHITECTURE.md`](ARCHITECTURE.md)** — the shape of the system, and which
  parts of it exist today.
- **[`docs/`](docs/index.md)** — the detail: domain model, API contract,
  operations, decision records, plans.

Nested `AGENTS.md` files under `cmd/` and `internal/` carry the rules that
differ in those directories.

## What runs today

Three health endpoints and eight authenticated read endpoints over PostgreSQL,
with Supabase JWTs verified locally. Nothing writes.
[`docs/api-contract.md`](docs/api-contract.md) is the full list.

The worker, the processing pipeline, RabbitMQ, Redis, embeddings and search are
planned and not here; [`ARCHITECTURE.md`](ARCHITECTURE.md) says what is coming
and in what shape.
