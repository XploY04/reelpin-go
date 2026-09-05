# Operations

Running it, configuring it, and what to look at when it misbehaves.

## Configuration

`internal/config` is the only package that reads the environment. Everything
else takes what it needs as an argument, which is why the whole service is
testable without setting a variable.

| Variable | Default | Notes |
|----------|---------|-------|
| `ENVIRONMENT` | `development` | One of `development`, `test`, `production`. Anything else fails at startup. |
| `PORT` | `8000` | Must parse and be 1..65535. |
| `APP_VERSION` | `dev` | Reported in the health response. |
| `DATABASE_URL` | compose DSN | **Required** in production; outside it, falls back to the local compose database. |
| `SUPABASE_URL` | — | **Required** outside the `test` environment. The JWKS is fetched from it at startup. |
| `SUPABASE_JWT_AUDIENCE` | `authenticated` | Checked on every token. |

Startup fails loudly and immediately on a missing required variable. A service
that starts and then answers 500 is harder to diagnose than one that refuses to
start.

## Running locally

```sh
docker compose up -d      # postgres on :5432
go run ./cmd/api
```

The compose database is `reelpin` on `localhost:5432`, with the user and
password `docker-compose.yml` sets. `internal/config` builds that fallback DSN
itself, so `DATABASE_URL` can stay unset locally.

Do not write a full connection URI with inline credentials into a committed
file, even a throwaway local one: secret scanning fails the build on the
pattern, and it scans every commit in a pull request rather than the final
diff, so a later fix does not clear it.

It is local only. Production uses managed PostgreSQL.

## Health

Three endpoints, and the difference between them matters:

- **`/api/v1/health/live`** never touches the database and always answers 200.
  A failure here means the process is gone. This is what a container health
  check and a process supervisor should call.
- **`/api/v1/health/ready`** pings the database with a 2s bound and answers 503
  when it fails. This is what a load balancer should call, because a replica
  that cannot reach the database should stop receiving traffic.
- **`/api/v1/health`** returns the readiness body with a 200 no matter what.
  It exists only because the Python service behaved that way. Do not use it to
  decide anything.

The response keeps `service: "ReelMind API"` and a check named `supabase` that
is really this service's own PostgreSQL. Both are compatibility with the shipped
app. Renaming them is a breaking change.

A failed database check reports `degraded`, never `error`, and every check in
one response carries the same `checked_at`.

## Shutdown

`SIGINT` and `SIGTERM` start a 10-second drain: the server stops accepting, in
flight requests finish, then the JWKS cache stops and the pool closes, in that
order. A supervisor that kills faster than 10s will cut requests off.

## Authentication in practice

Tokens are verified locally against the Supabase JWKS. It is fetched once at
startup with a 5s timeout, cached for at most 10 minutes, and refreshed once
when a token arrives with an unknown `kid`.

The consequences worth knowing:

- **Startup requires Supabase to be reachable.** It is not lazy. A JWKS failure
  is a startup failure.
- **There is no per-request network call**, so Supabase being slow does not make
  the API slow.
- **A rotated signing key is picked up within one unknown-`kid` refresh**, not
  on a timer.
- Only ES256 is accepted, and the algorithm is checked in the protected header
  before verification, not after.

## When something is wrong

- **401 on every request**: check `SUPABASE_URL` points at the right project and
  that the token's audience matches `SUPABASE_JWT_AUDIENCE`. The verification
  error is in the logs, never in the response.
- **503 from readiness**: the database ping failed within 2s. The driver error
  is logged; the response deliberately says nothing about it.
- **A job the app polls forever**: look for `status = completed` with a null
  `result_reel_id`. This service presents it as failed and does not repair the
  row.
- **Empty results for a user who has reels**: the token's `sub` is the user id
  used for every query. A token from the wrong Supabase project verifies against
  the wrong JWKS and would not get this far, but a token for a different user
  will return an honest empty list.
