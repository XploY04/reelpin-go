# reelpin-go

Go rewrite of the ReelPin backend. It does **not** serve the Flutter app yet:
real users are on `reelpin-api` (Python/FastAPI). This repo is being built up
one reviewable layer at a time, and the cutover is the last step.

Start here, then read the doc the task needs. `docs/index.md` is the map.

## Commands

```sh
docker compose up -d          # postgres, redis and rabbitmq
go run ./cmd/api              # :8000 unless PORT says otherwise
go run ./cmd/worker           # consumes the processing queues
go run ./cmd/maintenance      # one-off jobs; run it bare for the list
make check                    # what CI runs: fmt, vet, unit, race, docs, boundaries
make test-integration         # tagged tests; needs TEST_DATABASE_URL
```

`make check` is the one command to run before pushing. Anything it does not
catch is a gap in `make check`, not a reason to run something else by hand.

## Repository map

| Path | What lives there |
|------|------------------|
| `cmd/api/` | Wiring only: config, pool, JWKS verifier, `http.Server`, signals. No logic. |
| `cmd/worker/` | The queue consumers: processing, indexing, notifications. |
| `cmd/maintenance/` | Migrations, backfills, retention, purge, taxonomy curation, search eval, the container healthcheck. |
| `cmd/load/` | Drives `internal/load`. Never runs in CI. |
| `internal/httpapi/` | Routes, middleware, handlers, response shapes. The transport layer. |
| `internal/config/` | `Load()` and its validation. The only reader of the environment. |
| `internal/auth/`, `internal/sharetoken/` | Who is asking: Supabase JWKS verification, and the long-lived tokens the native share extensions use. |
| `internal/reels/`, `internal/jobs/`, `internal/collections/`, `internal/mapview/` | Domain types and their reader ports. No transport, no driver. |
| `internal/enqueue/`, `internal/pipeline/`, `internal/queue/`, `internal/outbox/`, `internal/lease/` | Submission through to a persisted version: idempotency and dedupe, the staged run, RabbitMQ, the transactional outbox, and the fencing leases that keep two workers off one run. |
| `internal/sourceidentity/`, `internal/platform/` | What a shared link *is*, and the per-platform handlers that read it. |
| `internal/ai/`, `internal/embed/`, `internal/search/`, `internal/taxonomy/` | Extraction and categorization, the embedding index, the three search arms and their fusion, and the category tree the prompt is built from. |
| `internal/lifecycle/`, `internal/backfill/` | Deletion, retention and purge; and the resumable read of the legacy Python tables. |
| `internal/notify/` | Push tokens and FCM delivery. |
| `internal/media/`, `internal/storage/`, `internal/providers/`, `internal/apify/`, `internal/cookies/` | Bounded downloads, the temp workspace, provider budgets and cooldowns. |
| `internal/safehttp/`, `internal/ratelimit/` | The outbound SSRF guard and the Redis limiters. |
| `internal/postgres/`, `internal/db/`, `internal/migrations/` | pgx implementations, the pool, and the embedded migrations. |
| `internal/metrics/` | The only importer of the Prometheus client: every collector, the gauge sampler, and `Hash` for log redaction. |
| `internal/load/` | The load driver's scenarios, sender and report. |
| `api/` | The OpenAPI contract, its embedded bytes, the generated route manifest and fixtures. |
| `deploy/` | `alerts.yml` (checked against the real registry by `internal/metrics/alerts_test.go`) and the host-side scripts the release workflow copies over and runs. No secrets. |
| `docs/` | Committed knowledge. See `docs/index.md`. |
| `drills/` | Standalone Go/DSA practice programs. Unrelated to the service. |

Nested `AGENTS.md` files exist where the rules differ: `cmd/`, `internal/`,
`internal/httpapi/`. The nearest one to the file you are editing wins.

## Rules

- **Match the surrounding code.** Naming, error handling and test style are
  already consistent; a change that reads differently is a change that is wrong.
- **The domain does not know about transport or drivers.** `internal/reels` and
  `internal/jobs` must never import `net/http`, `pgx`, or `internal/postgres`.
  `internal/httpapi` talks to the reader *interfaces*, never to `internal/postgres`.
  `make check` enforces this.
- **Never trust a `user_id` from a request.** It comes from the verified token's
  `sub`, always. Every query is scoped by it.
- **A GET never writes.** The readers are read-only on purpose; see
  `docs/decisions/0002-reads-never-write.md`.
- **Public errors are stable service contracts.** A driver error is logged,
  never returned in a response body. Change an error code only with the API
  contract and its tests.
- **Comments explain the non-obvious why**, never what the line does. Default to
  none.
- Every behaviour change ships a test that fails before it and passes after.

## Traps

- **`docs/` used to be entirely gitignored**, so anything you remember about it
  may predate this. Only personal calendars and scratch plans are ignored now.
- **`README.md` describes an older design** in places (an in-memory store, goose,
  chi). `docs/` is authoritative; the README is being retired into it.
- **Dev and production use separate infrastructure.** They have different
  Supabase projects, Redis instances and RabbitMQ virtual hosts. Do not add an
  `environment` column to compensate for shared infrastructure.
- **The real domain is `reelpin.in`.** Some code and docs still say `reelpin.app`.
- **Reel list queries never select `transcript`.** Only `GET /reels/{id}` does.
  Adding it to the list query is a silent payload and cost regression.
- **A test that calls `t.Fatal` while holding an open transaction hangs the whole
  package** until the timeout: the per-test database cannot be dropped while a
  connection holds it. Roll back first, then fail.
- **Never commit a credential, not even a throwaway one.** Secret scanning
  fails on a connection URI with an inline password *and* on a bare
  `POSTGRES_PASSWORD: value` in a workflow, however disposable the container is.
  It also scans every commit in a pull request rather than the final diff, so
  fixing it in a later commit does not clear the failure and the branch has to
  be rewritten. In CI, give the throwaway database
  `POSTGRES_HOST_AUTH_METHOD: trust` and no password at all.
