# reelpin-go

Go rewrite of the ReelPin backend. It does **not** serve the Flutter app yet:
real users are on `reelpin-api` (Python/FastAPI). This repo is being built up
one reviewable layer at a time, and the cutover is the last step.

Start here, then read the doc the task needs. `docs/index.md` is the map.

## Commands

```sh
docker compose up -d          # postgres on :5432
go run ./cmd/api              # :8000 unless PORT says otherwise
make check                    # what CI runs: fmt, vet, unit, race, docs, boundaries
make test-integration         # tagged tests; needs TEST_DATABASE_URL
```

`make check` is the one command to run before pushing. Anything it does not
catch is a gap in `make check`, not a reason to run something else by hand.

## Repository map

| Path | What lives there |
|------|------------------|
| `cmd/api/` | Wiring only: config, pool, JWKS verifier, `http.Server`, signals. No logic. |
| `internal/config/` | `Load()` and its validation. The only reader of the environment. |
| `internal/httpapi/` | Routes, middleware, handlers, response shapes. The transport layer. |
| `internal/auth/` | Supabase JWKS verification and the user-id context key. |
| `internal/reels/` | Reel domain types, the `ReelReader` port, display and filter builders. |
| `internal/jobs/` | Job domain types, the `JobReader` port, status presentation. |
| `internal/postgres/` | pgx implementations of both reader ports. Reads only. |
| `internal/db/` | `Connect(ctx, url)`: a pgx pool and a 5s ping. Nothing else. |
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
- **Errors keep the Python error codes.** The Flutter app matches on them.
  A driver error is logged, never returned in a response body.
- **Comments explain the non-obvious why**, never what the line does. Default to
  none.
- Every behaviour change ships a test that fails before it and passes after.

## Traps

- **`docs/` used to be entirely gitignored**, so anything you remember about it
  may predate this. Only personal calendars and scratch plans are ignored now.
- **`README.md` describes an older design** in places (an in-memory store, goose,
  chi). `docs/` is authoritative; the README is being retired into it.
- **The Python service calls itself ReelMind internally** and the health response
  still says `service: "ReelMind API"` with a legacy `supabase` check key. That is
  compatibility, not a mistake. Do not tidy it.
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
