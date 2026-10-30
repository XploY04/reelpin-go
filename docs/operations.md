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
| `SUPABASE_URL` | none | **Required** outside the `test` environment. The JWKS is fetched from it at startup. |
| `SUPABASE_JWT_AUDIENCE` | `authenticated` | Checked on every token. |
| `COST_GATE_WARN_USD` | none | Monthly warning amount, plain dollars (`12.00`). See [`cost-gate.md`](cost-gate.md). |
| `COST_GATE_STOP_USD` | none | Monthly hard stop. Must be above the warning amount. |
| `COST_GATE_STOP_ORDER` | none | Which groups stop taking new work first, in order, ending with `all`: `instagram,media,light,all`. An entry is a platform or a work class (`media`, `light`). |
| `COST_GATE_PRICES` | none | Provider prices, `provider:model:unit=usd` separated by commas. Units: `input_mtok`, `output_mtok`, `call`. The model may be `*`. |

The four cost-gate variables are all-or-nothing. Set all four and the gate runs;
set none and provider spending is measured but not limited. Set some and startup
fails, because a spending limit assembled out of defaults is a limit nobody
approved.

Startup fails loudly and immediately on a missing required variable. A service
that starts and then answers 500 is harder to diagnose than one that refuses to
start.

Copy the variable names from [`.env.example`](../.env.example) into an ignored
`.env.dev`. Store real values only in that local file and the EC2 secret store.

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

## Environments

The current Supabase project is production. Development uses a separate
Supabase project and runs on the host reached through `ssh reelpin-ec2-dev`.
Development and production also use separate Redis instances and RabbitMQ
virtual hosts. Database rows do not carry an environment discriminator.

## Health

Two endpoints, and the difference between them matters:

- **`/api/v2/health/live`** never touches the database and always answers 200.
  A failure here means the process is gone. This is the only endpoint an
  uptime monitor, a load balancer, a container health check or a process
  supervisor should call.
- **`/api/v2/health/ready`** pings the database with a 5s bound and answers 503
  when it fails. Call it by hand, or once after a deploy, never on a schedule.
  The database suspends after five idle minutes and every ping wakes it, so a
  monitor polling readiness keeps it awake around the clock and spends the
  month's compute hours on nothing. The 5s bound is there so the ping that
  wakes it does not time out.

The response reports `service: "ReelPin API"`. Readiness names its PostgreSQL
dependency `database`.

A failed database check reports `degraded`, never `error`, and every check in
one response carries the same `checked_at`.

## Shutdown

`SIGINT` and `SIGTERM` start a 10-second drain: the server stops accepting, in
flight requests finish, then the JWKS cache stops and the pool closes, in that
order. A supervisor that kills faster than 10s will cut requests off.

## Metrics collector preparation

[`deploy/metrics.alloy`](../deploy/metrics.alloy) and
[`deploy/reelpin-alloy@.service`](../deploy/reelpin-alloy@.service) are local
templates for [ADR 0014](decisions/0014-use-managed-free-observability-first.md),
not an installed collector. They scrape only loopback API 8080 and worker 9101
with `X-Admin-Key` once a minute. The built-in host exporter reads memory,
memory pressure and swap counters only. An allowlist drops other metrics and
labels; exported instances are `api`, `worker` and `host`, with a separate
`environment` label. No logs, traces, exemplars or user identifiers are
forwarded; host instance names are replaced. The existing application route
label is a mux pattern, not a request URL. Alloy's UI stays on
`127.0.0.1:12345`; usage reporting is disabled.

The installation candidate is the official Alloy v1.21.3 standalone Linux
release for the host architecture, checksum-verified and placed at
`/usr/bin/alloy`, matching the unit. No binary has been installed on a host.

After owner approval, choose one unit instance per host (`development` or
`production`). The config path is `/etc/reelpin/alloy/metrics.alloy`. Its
`/etc/reelpin/alloy/<environment>.env` contains only the Grafana-provided HTTPS
Prometheus remote-write endpoint:

```text
GRAFANA_METRICS_URL=https://REPLACE_WITH_STACK_ENDPOINT.invalid/api/prom/push
```

The root-only config and three root-only files under
`/etc/reelpin/alloy/<environment>/` are loaded by systemd `LoadCredential`:
`metrics-config` (the config above), `metrics-admin-key` (the matching Go `ADMIN_KEY`),
`metrics-username` (Grafana's metrics tenant ID), and `metrics-token` (a token
scoped to writing metrics to that stack). No credential values belong in the
unit, environment file, command line or repository. The sandboxed service reads
the config through `%d/metrics-config`, so `/etc/reelpin` can stay root-only.
Restart the unit after changing the source config or credential files so
systemd reloads them. The admin-key file must contain the exact header bytes,
with no trailing newline;
it stays typed as a secret through the scrape header.

The Ubuntu 24.04 manual regression ran a bounded `DynamicUser` service against
the same config under a mode-0700 `/etc/reelpin`: direct readability failed
with exit 1, while the `LoadCredential` copy was readable with exit 0. The
parent directory permissions stayed unchanged. `systemd-run` escapes literal
`%d` command arguments, so that transient check used `CREDENTIALS_DIRECTORY`;
the installed unit uses systemd's `%d` specifier.

The initial hard limits are 64 MiB memory, no swap and 0.05 CPU; a 40 MiB Go
runtime target is not a memory guarantee. The queue has one shard and 1,000
samples; each app scrape fails above 2,000 samples or 1 MiB. WAL samples expire
after 15 minutes, with cleanup every five minutes. This is time retention,
not a filesystem byte quota. Interrupted delivery can lose expired samples.

Before installation, validate with `alloy fmt --test deploy/metrics.alloy` and
`alloy validate deploy/metrics.alloy`, supplying the two non-secret environment
variables `REELPIN_ENVIRONMENT` and `GRAFANA_METRICS_URL`, plus
`CREDENTIALS_DIRECTORY` for the credential paths. Neither command starts
scraping or remote writes. Format and configuration validation passed with
the checksum-verified Alloy v1.21.3 Darwin ARM64 release. A network-isolated
Linux ARM64 baseline ran for 90 seconds and returned readiness 200 at the
requested limits: 46.3 MiB cgroup peak, no swap, OOM events or restarts. It used
synthetic host files and unreachable scrape/write endpoints, not a source-load
test. The harness killed it after ten seconds of shutdown wait; clean shutdown is not
proven. Linux unit validation and a host trial have not run. Approval still
needs a stack/region and token scope, series/ingestion measurements and the
80-percent free-tier alert from ADR 0014, disk headroom or a WAL quota, and a
bounded host memory/CPU trial under these limits. Collector syntax validation
does not prove this budget fits. `deploy/alerts.yml` remains unevaluated until
rules scoped to the intended environment and a contact point are installed
and tested in Grafana Cloud; its `autorollback` labels do not execute a rollback.

## Authentication in practice

Tokens are verified locally against the Supabase JWKS. It is fetched once at
startup with a 5s timeout and cached for at most 10 minutes. An unknown `kid`
can force an immediate refresh, limited to one refresh per five-second window.

The consequences worth knowing:

- **Startup requires Supabase to be reachable.** It is not lazy. A JWKS failure
  is a startup failure.
- **There is no per-request network call**, so Supabase being slow does not make
  the API slow.
- **A rotated signing key is picked up by an unknown-`kid` refresh.** Repeated
  unknown keys cannot force an outbound request on every authentication attempt.
- Only ES256 is accepted, and the algorithm is checked in the protected header
  before verification, not after.

## When something is wrong

- **401 on every request**: check `SUPABASE_URL` points at the right project and
  that the token's audience matches `SUPABASE_JWT_AUDIENCE`. The verification
  error is in the logs, never in the response.
- **503 from readiness**: the database ping failed within 5s. The driver error
  is logged; the response deliberately says nothing about it.
- **A job the app polls forever**: look for `status = completed` with a null
  `result_reel_id`. This service presents it as failed and does not repair the
  row.
- **Empty results for a user who has reels**: the token's `sub` is the user id
  used for every query. A token from the wrong Supabase project verifies against
  the wrong JWKS and would not get this far, but a token for a different user
  will return an honest empty list.
