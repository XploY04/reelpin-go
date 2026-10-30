# Cutover runbook: Python to Go

The owner approved gated execution of this production plan on 2026-10-28.
Every gate below still applies; no production mutation had run at approval.
Go serves development at `/api/v2`; production still serves
Python. The production switch keeps `/api/v1` on Python and adds `/api/v2` on Go
for updated clients.

## Current production baseline

Read-only measurements on 2026-10-28, before adding Go:

| Resource | Observed |
|---|---|
| Host | `ssh reelpin-ec2`, 2 vCPU, 911 MiB RAM |
| Memory | 596 MiB used, 315 MiB available, 74 MiB free |
| Swap | 240 MiB used of 2047 MiB |
| Disk | 21 GB free of 29 GB |
| Python API | 124.4 MiB current, 134.0 MiB peak, port 8000 |
| Python worker | 179.8 MiB current, 290.3 MiB peak |
| Redis | 3.6 MiB current, loopback port 6379 |
| Go prerequisites | Docker absent, RabbitMQ inactive, port 8080 free |
| Routing | nginx sends all API traffic to Python on 8000 |
| GitHub | `production` environment exists; its secrets list is empty |

These are measurements, not a capacity test. Re-read them before an approved
trial, including Python's queues, active jobs and response latency.

Production Go uses Neon project `misty-scene-89733521`, branch `production`.
Development stays in separate project `muddy-glade-39541440`. Confirm the
production branch is still empty before the first migration: the 2026-10-28
read-only check found no `reelpin`, `legacy` or `auth` schema, no non-system
base tables and no `public.schema_migrations`. Stop and reconcile any new data
instead of dropping it. Supabase
`rdzykswefpgeolbamend` remains the shared Auth and Storage service. Python keeps
its Supabase database; Go owns Neon `reelpin.*` and the `auth.users` mirror.
There is no `environment` column in Go tables.

## Gates before production work

1. Obtain owner approval for the concrete trial below, including Docker,
   RabbitMQ, host files, Neon writes, credentials and subsequent routing. A
   merged documentation PR is not that approval. Infrastructure spending stays
   at $0: no host resize, paid plan or extra server. Approved Gemini and Maps
   calls do not authorize infrastructure charges.
2. Use the tested release workflow and its deployment scripts. The smoke check
   calls Go on 8080 and requires its service identity and full image revision,
   not Python's port 8000. The migration wrapper caps its container at 128 MiB,
   no extra swap and 0.25 CPU. For production it refuses active Go units or
   containers, undrained RabbitMQ work and unfinished canonical database work.
   It also requires `reelpin-production.slice` active with `MemorySwapMax=0`
   and places the maintenance container under that slice.
   A fresh broker with no queues is allowed only against an uninitialized
   database; an empty broker on an initialized database fails closed.
   Drain and stop the Go API and worker before dispatch; the wrapper never
   stops active work for you. Python keeps running. Test the limits, identity
   checks and fail-closed gate locally before production dispatch.
3. Complete dev CI, local integration tests and the rehearsal gates in
   [rehearsal.md](rehearsal.md). Do not run integration suites against either
   live Neon project. The 2026-10-27 snapshot and `backfill-verify --sample 100`
   passed on Neon dev; deltas, recovery drills and capacity gates still need
   evidence.
4. Calibrate dense search on carried `gemini-embedding-001` vectors and record
   `SEARCH_DENSE_MAX_DISTANCE`. The 0.40 default was measured on
   `gemini-embedding-2`; it is not evidence for the carried model.
5. Finish the website's signed-in flows, share-link destination, production
   Vercel environment and Supabase redirect URLs. The web server uses the
   production `API_BASE_URL`; browser credentials stay server-side.
6. Finish the MCP consent page and Supabase OAuth server/client setup before
   exposing MCP. Keep its nginx routes closed until then; see
   [mcp.md](mcp.md). Do not treat a working REST login as OAuth consent proof.
7. Prepare the Android app promotion and release built with
   `REMOTE_CONFIG_URL=https://reelpin.in/config.json`. Verify native share,
   share-token enqueue, job placeholders, collections and FCM on a device.
   Android compilation alone does not prove these flows. iOS remains a separate
   gate until its Share Extension compiles and passes device testing. Existing
   clients continue on Python; this migration requires a client release.

## Trial budget on the existing host

Keep the Python API, worker, Redis and nginx units running with their current
limits. Proposed additional caps, to be tested before any user routing:

| Go service | Memory / total memory-plus-swap | CPU cap | Initial runtime settings |
|---|---|---|---|
| API | 96 / 96 MiB | 0.25 CPU | `GOMEMLIMIT=72MiB`, 128 pids |
| Worker | 256 / 256 MiB | 0.75 CPU | `GOMEMLIMIT=160MiB`, 256 pids |
| RabbitMQ | 192 / 192 MiB | 0.40 CPU | Absolute memory watermark 128 MiB, 256 pids |

The memory-plus-swap limits equal RAM limits. On systemd cgroup v2 hosts, the
[runtime's zero-swap property bug](https://github.com/opencontainers/cgroups/issues/74)
can let `systemctl daemon-reload` reset the container's swap limit despite those flags.
Install `/etc/systemd/system/reelpin-production.slice` with:

```ini
[Unit]
Description=ReelPin production container limits

[Slice]
MemorySwapMax=0
```

Start the slice before its containers. Pass
`--cgroup-parent reelpin-production.slice` to the Go API, worker, broker and
every production maintenance container, keeping their individual caps above.
Make the three service units require and start after this slice. Its persistent
ancestor limit survives container recreation and daemon reloads; a limit on
the service running the Docker client does not cover Docker's sibling scope.
Verify the slice's `MemorySwapMax=0` and actual `memory.swap.max=0`, parentage,
and unchanged container caps after both operations before admitting traffic.
Keep Python and other containers outside it. Do not clear or resize the host's
existing swap. Go's heap
limit is not a process limit: the worker's cap includes `ffmpeg` and `yt-dlp`.
A permitted 500 MiB media input can still exceed the worker's 256 MiB cap.

Allow an estimated 64 MiB for added Docker/containerd overhead; measure it after
installation. Dev idle samples were about 36 MiB API, 31 MiB worker and 143 MiB
RabbitMQ. Those plus the overhead estimate total 274 MiB, leaving only 41 MiB
of the measured 315 MiB available. The container caps total 544 MiB; with
64 MiB overhead that is 608 MiB, exceeding available memory by 293 MiB before
Python grows toward its recorded peaks. Idle may fit. Busy coexistence is
unproven and may fail.

There is no worker-concurrency flag to turn down. `cmd/worker` starts one
consumer each for media, light, notifications and scheduled work, each with
prefetch 1, plus the outbox, sweeper and reminder dispatcher. Scheduled price
checks fetch four pages concurrently. One worker process can therefore do
several kinds of work at once; a CPU cap does not serialize it.

Run one worker process, with disk-backed `/var/lib/reelpin/work` owned by uid
10001, never a media tmpfs. Use the application's disk admission limit, bounded
Docker logs (10 MiB, three files per container), and a broker disk-free limit
of 1 GiB. Re-read host disk before retaining old images and dumps; keep the
previous digest for rollback.

The $0 fallback is to keep Python in production and Go in development. Stop
only the added trial services if they fail before Go accepts work. Do not
raise caps, resize the host or retire Python to make a failed trial fit. Return
the measured failure to the owner for a capacity decision.

## Approved preparation sequence

Execute this section only after the approval gate above. Record the tested
commit, immutable image digest, initial measurements and recovery points.

1. Test the deployment prerequisite fix locally, run `make check` and the local
   integration suite, then deploy the chosen digest to dev. Record the running
   build and acceptance results. Reserve that same
   `ghcr.io/xploy04/reelpin-go@sha256:...` digest for production promotion at
   step 11, after host and data preparation. Do not rebuild it or dispatch
   production with an empty `digest` input.
2. Verify production backups and a timed restore for PostgreSQL, Auth, Storage
   and required keys. Keep the Python service definitions, nginx configuration,
   current images and queue state recoverable. Record recovery limits before
   any production data copy.
    Before creating a persistent source backup login, obtain specific owner
    approval to apply [the source RPC access script](../deploy/supabase-source-rpc-access.sql)
    to Supabase with `psql -X --set ON_ERROR_STOP=1 --file` using the temporary
    CLI login described in [rehearsal.md](rehearsal.md#connecting-to-supabase).
    This is an exception to Supabase read-only access, never a Neon migration
    or a release-workflow step. It restricts the account-deletion and two
    notification claim RPCs to their backend callers; Python binds deletion to
    the authenticated user and calls these RPCs with its service-role client.
    The transaction guards the source signatures, owners and definer status,
    then checks effective caller access before commit. Other explicit roles
    and the new-user trigger remain unchanged. This creates no backup login
    or backup service; those require separate approval.
    Local approval proof uses synthetic PostgreSQL 16: client and standalone
    SELECT-role RPC writes fail after the change, service-role calls and other
    grants survive, and contract/postcondition failures leave ACLs unchanged.
3. Install Docker only after confirming its network/firewall changes preserve
   Python's existing access. Pin a locally tested RabbitMQ image by digest,
   without the management plugin. Create `reelpin-rabbitmq-production`, a durable
   volume and vhost/user dedicated to Go production. Bind AMQP only to
   `127.0.0.1:5672`; expose no management UI. Apply the caps and absolute
   watermark above. Install and start the zero-swap slice before RabbitMQ,
   then start RabbitMQ alone and measure its idle impact first.
4. Create `/etc/reelpin/production.env` and the separate RabbitMQ env file with
   root ownership and mode `0600`. Populate them through a protected input/file
   transfer, never terminal output, chat, shell tracing, command arguments,
   source control or CI logs. Do not alter Python's env file. Set:

   | Setting | Production value or source |
   |---|---|
   | `ENVIRONMENT`, `WORKER_ID` | `production`, unique production worker id; image binaries use their baked commit revision, overriding a stale `APP_VERSION` |
   | `PORT`, `METRICS_PORT` | 8080, 9101 |
   | `DATABASE_URL` | Direct TLS URL for Neon `misty-scene-89733521`, branch `production` |
   | `SUPABASE_URL`, `SUPABASE_JWT_AUDIENCE` | `https://rdzykswefpgeolbamend.supabase.co`, `authenticated` |
   | `SUPABASE_SERVICE_ROLE_KEY` | Shared project's existing protected value, needed for Storage and identity deletion |
   | `REDIS_URL`, `REDIS_KEY_PREFIX` | Existing loopback Redis, a checked-unused logical database, prefix `reelpin-go-production` |
   | `RATE_LIMIT_SALT`, `ADMIN_KEY` | Newly generated protected values |
   | `RABBITMQ_URL` | Loopback Go production vhost and dedicated credentials |
   | `SHARE_BASE_URL`, `PUBLIC_WEB_BASE_URL` | `https://reelpin.in`, with the web share-route gate resolved |
   | `PUBLIC_API_BASE_URL` | `https://api.reelpin.in`; nginx keeps MCP closed until OAuth acceptance |
   | `REELPIN_IP_BUCKET_SECRET` | Same protected value as the production Next.js server |
   | `GEMINI_API_KEY`, `GOOGLE_MAPS_API_KEY` | Approved production provider credentials |
   | `EMBEDDING_MODEL`, `SEARCH_DENSE_MAX_DISTANCE` | `gemini-embedding-001`, measured dev value |
   | `INSTAGRAM_ACTIVE_COOKIE_DATA_BASE64`, `INSTAGRAM_BACKUP_COOKIE_DATA_BASE64`, `INSTAGRAM_TERTIARY_COOKIE_DATA_BASE64` | The same Python production cookie slots, privately mapped to these Go names as approved by the owner |
   | `FIREBASE_PROJECT_ID`, `FIREBASE_CREDENTIALS_JSON` | Protected production FCM configuration |
   | `REEL_THUMBNAIL_BUCKET` | Existing `reel-thumbnails` bucket |

   Preserve the approved cookie-reuse decision; do not substitute development
   cookies. Inventory `APIFY_API_TOKEN`, `APIFY_ACTORS`, Reddit credentials and
   any non-default binary paths privately. Leave paid fallbacks disabled unless
   their use is separately authorized. Set all four `COST_GATE_*` values from
   approved prices/thresholds or leave all four unset; the latter records usage
   without restricting spending. Neither choice increases provider approval.
   Confirm the chosen Redis database/prefix leaves Python's keys untouched;
   never flush Redis. Keep maintenance on the direct Neon URL because its
   migration lock needs a session. If a search path is required, pass it through
   `options=-c search_path=...` rather than a dropped proxy startup parameter.
5. Add GitHub environment secrets `DEPLOY_HOST`, `DEPLOY_USER` and
   `DEPLOY_SSH_KEY` to `production`, using the existing host and a dedicated
   deployment key. Verify environment branch restrictions and owner approval
   protection. The workflow needs privilege to run Docker and the deployment
   script; review that access before installing the key. Keep database/provider
   values in the root-only host files. Never copy dev deployment secrets.
6. Confirm the Neon project/branch identity and empty application schema again.
   Pull the tested digest and run its maintenance entrypoint against production
   with Go services stopped: `migrate`, then `migrate-status`. Apply every
   embedded migration, currently `00000` through `00024`; verify the actual
   list for the promoted digest. Do not skip the auth mirror migration. Use a
   bounded one-off container (initially 128 MiB, no extra swap), run sequentially
   with other preparation jobs, and verify each exit status. Stop on a failure;
   do not rerun with a different database or roll migrations back.
7. Take a fresh read-only Supabase dump, limited users export and source-clock
   `DUMPED_AT`, then restore to Neon production `legacy`, following
   [rehearsal.md](rehearsal.md) exactly. Run staging/restore tooling off the
   constrained EC2 host. With Go API and worker still stopped, run the dry
   snapshot, inspect skips and counts, then `backfill-legacy --execute` and
   `backfill-verify --sample 100` without `--allow-drift`. Require zero unexplained
   rows, missing carried rows and field mismatches. Read `compared` and
   `text_not_comparable`; check maps and embedding model counts. Save the
   reported `next_since` for the first delta. Retain `legacy` through the final
   delta. This copies data into Neon, not back into Supabase.
8. Install new units and containers named `reelpin-api-production` and
   `reelpin-worker-production`; never reuse the Python unit names. Match
   `deploy/reelpin-deploy.sh`'s image files
   `/etc/reelpin/reelpin-{api,worker}-production.image`, mode `0600`. Use the
   tested digest, read-only root filesystems, uid 10001 and the trial limits.
   Override the API's image health URL to
   `http://127.0.0.1:8080/api/v2/health/live`; disable that API healthcheck on the
   worker. Use disk-backed worker scratch, graceful stop timeouts and separate
   restart policies. Go currently binds all interfaces on its configured
   ports; firewall/security-group rules must deny external access to 8080 and
   9101. Only nginx reaches the API and approved internal scrapers reach metrics.
9. Start the API alone, prove its 8080 identity/build and readiness directly,
   and verify Python 8000 remains healthy. Measure added RAM, Docker overhead,
   swap activity and Python p95. Start the worker only with its production queue
   empty; it immediately seeds reminders and schedules daily price checks, so
   this start can send notifications and fetch product pages from carried data.
   If due reminders or price tracks exist, review them and obtain approval for
   those side effects before starting it. Verify heartbeats and queue consumers.
10. Exercise one approved processing job while observing Python and Go together,
    then a bounded mix covering the worker's concurrent consumers. Paid calls
    must stay inside the approved Gemini/Maps scope. Require no OOM/restart,
    sustained swap growth or broker memory alarm, and no material Python
    latency/error regression; record limits and peaks, not just idle RSS.
    Require read p95 below 800 ms and database network time below 150 ms.
    A failed trial blocks routing. Apply the $0 fallback above.
11. Before any client reaches Go, refresh `legacy` and apply a verified delta
    using the saved mark and the new source-clock dump time. Read back all
    environment/port/image settings without exposing secrets. Pause new trial
    submissions, wait for every Go processing run and broker delivery to finish,
    then stop the Go API and worker. Confirm their units are inactive and their
    containers are stopped. Dispatch the release workflow only with
    `environment=production` and the reserved tested digest. Its migration
    must run under the 128 MiB, no-extra-swap, 0.25 CPU limits before the
    deployment restarts the worker. Confirm the smoke targets 8080 and the
    actual build. The worker deploys first, then API deployment runs the final
    smoke and restores the previous API digest on failure, verifying its
    identity and readiness. This order requires the new worker to remain
    compatible with the previous API and the expand-only schema. API rollback
    leaves the worker draining; a same-digest redeploy offers no distinct
    fallback image.
12. Back up the nginx site, add `location ^~ /api/v2/` proxying to
    `http://127.0.0.1:8080` while preserving the URI and forwarding its normal
    host/client headers. Keep `/api/v1/` and the existing fallback on Python
    8000. Run `nginx -t` before reloading. Check v1 and v2 readiness and
    authenticated reads with an approved test account independently through
    TLS. Its first Go request records that account as migrated. Do not mirror writes.
    Add exact `/mcp` and metadata routes only after the separate OAuth gate
    passes. Record the config and verification time.
13. Release the approved Android build through the app's promotion/store flow.
    Keep `config.json` migration gates off until the owner approves a staged
    cohort; increase cohorts only after authenticated library, enqueue,
    search, share-extension and FCM checks pass on production. Website
    production switches to the Go origin only after its separate acceptance.

## Dual run and rollback

Python and Go have different databases. A proxy change cannot undo Go writes
or copy them to Supabase. Keep each user on one backend: their first served Go
request records `user_migrations`, after which legacy backfills leave that
user's rows alone. Old-app writes between the last dump and that first request
can be lost; the gap and manual Pro-grant procedure are documented in
[rehearsal.md](rehearsal.md). Deltas are not bidirectional synchronization.

Run the documented nightly dump/delta/verify sequence and retain each
`next_since`. Do not restart a Go worker after every import without considering
that it re-seeds reminders. Before forcing an Android minimum build through
`config.json`, obtain owner approval, check adoption and drain Python work.
Then freeze Python writes, take the final dump, dry-run and execute the final
`--propagate-deletes` delta, and verify without `--allow-drift`. Keep the
production `legacy` copy until acceptance; its eventual removal is a separate
approved destructive step.

Before Go serves real users, revert the new nginx routes and stop only the Go
trial if needed. Once real users reach Go, their migration markers change what
later deltas copy, even if they only read. After Go accepts mutations, freeze
new Go enqueue and leave workers draining RabbitMQ; Python cannot consume those
jobs. Keep migrated
users on their Go data, restore a previous compatible Go digest, or use an
owner-approved recovery/reconciliation plan. Routing those users back to Python
would show stale data and can lose changes. Never roll back the database or
silently run a reverse copy. Account deletion uses shared Supabase Auth, so
identity deletion is not isolated to a backend and cannot be reversed by nginx.

## Retiring Python and completion

Keep the Python deployment recoverable for at least 14 days after the final
switch. Retirement requires zero Python API traffic, zero Redis processing
messages and active Python jobs, no rollback event, and 30 days of route-use
evidence before route removal. Only then propose stopping its units, removing
owned secrets/deploy jobs and legacy queue keys. Treat cleanup migrations,
legacy-schema removal and any Pinecone deletion as separate approved changes.

The cutover is complete when released Android clients and native share paths
pass against Go, the web and OAuth gates are satisfied for the surfaces being
launched, the final delta verifies, recovery/alert drills pass and the measured
host load fits its caps. iOS is not declared ready from Android evidence.
