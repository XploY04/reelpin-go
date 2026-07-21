# ReelPin Go

The Go backend for ReelPin: it ingests saved Instagram reels/posts, runs them
through a processing pipeline (download → transcribe → extract → embed → save),
and serves them back to the Flutter app for browsing, map pins, and semantic
search.

The service presents a stable REST contract that the frontend consumes directly.

## Target architecture

**Modular monolith, monorepo, two binaries (API + worker), with a plugin-style
platform abstraction. Not microservices.**

- **`cmd/api`** — stateless HTTP API, N replicas, presents the public contract. Never processes reels directly.
- **`cmd/worker`** — long-running workers, M replicas, drain the queue and run the pipeline.
- **Postgres** owns state (source of truth); `pgvector` + FTS for hybrid search in one DB.
- **RabbitMQ** moves work: topic exchange, per-platform routing keys + retry + dead-letter.
- **Redis** stores fast, losable things (caches, rate limits, SSE progress). Never the source of truth.
- **Platform abstraction** (`internal/platform/`): a `Handler` interface + registry, one implementation per platform. Adding a platform = one new file. Instagram ships first; TikTok/YouTube are seams only, built when real.


## Current state

**Phase 1** (API + Postgres + job CRUD). What runs today:

- `net/http` API with a `server` struct holding its dependencies (store + DB pool).
- Reel store behind a `reelStore` interface (in-memory now, Postgres-backed next), so the swap is drop-in.
- Postgres via docker-compose, `pgx` pool created once in `main` and closed on exit.
- Sentinel + custom errors (`ErrNotFound`, `ValidationError`) mapped to 404/400 in handlers.
- `httptest` tests.

On the roadmap: goose migrations, the Postgres-backed store, RabbitMQ, the worker,
the processing pipeline, search, Redis, chi, observability.

### Endpoints

All under `/api/v1`:

| Method | Path | What |
|--------|------|------|
| `GET` | `/health/live` | liveness, always 200 |
| `GET` | `/health/ready` | readiness, pings the DB (503 if down) |
| `POST` | `/reels` | create a reel (400 on invalid JSON / validation) |
| `GET` | `/reels` | list reels |
| `GET` | `/reels/{id}` | get one (404 if missing) |

## Running it

```sh
docker compose up -d          # Postgres on :5432
go run ./cmd/api              # API on :8000
```

`DATABASE_URL` overrides the connection string; it defaults to the docker-compose
DB (`postgres://reelpin:reelpin@localhost:5432/reelpin`).

```sh
go test ./...                 # tests
go test -race ./...           # with the race detector
```

## Layout

```
cmd/api/         API binary + handler tests
internal/db/     pgx pool connect helper
internal/store/  reel store (in-memory now, interface-backed)
docker-compose.yml
```

Layout will grow toward the target shape as phases land: `internal/http`,
`queue`, `cache`, `search`, `platform`, `config`, and `migrations`.
