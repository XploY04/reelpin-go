# ReelPin Go and web implementation plan

## Purpose

This is the execution plan for replacing the Python processing backend with Go
and launching the authenticated web application. It assumes no implementation
context beyond the repository docs.

Architecture choices are closed. The only deferred value is the monthly
provider-cost limit. That value blocks public link submission, not development.

## How to use this plan

Work in order. A task is complete only when its test gate passes and its evidence
is attached to the pull request. Do not start a task whose dependency is still
open.

For every Go task:

1. Fetch `origin/dev` and the task branch.
2. Rebase the task branch onto the current `origin/dev`. Existing feature
   branches may use `--force-with-lease`; never force-push `dev` or `main`.
3. Read the nearest `AGENTS.md` before editing.
4. Keep handlers thin, domain packages independent of HTTP and pgx, and wiring
   in `cmd/`.
5. Update the API contract and affected docs in the same pull request.
6. Run `make check` and the task's integration or end-to-end tests.
7. Push only after fetching the target branch again.
8. Wait for every hosted check and resolve every review comment before merge.

Use one migration per schema change. Migrations are forward-only. A rollback
deploys the previous application image; it does not run a destructive down
migration.

## Fixed names and contracts

- Go, ReelPin web and updated Flutter use `reel` and `/api/v2/reels`. Python
  keeps `/api/v1` for older installed Flutter versions during coexistence.
- The database uses `contents`, `content_versions` and `user_saves`.
- `user_saves.id` is the public reel ID. Existing `public.reels.id` values stay
  unchanged.
- `contents.id` is internal and global. It is never an ownership boundary.
- Authentication always comes from the verified Supabase JWT `sub` claim.
- User-owned records return `404` when the authenticated user does not own them.
- New asynchronous work returns `202 Accepted` with a processing job.
- Future breaking changes require `/api/v3`; additive `/api/v2` changes do not.
- New Go-owned UUIDs are UUIDv4 values generated with `github.com/google/uuid`.
  Do not keep a local UUID implementation.
- Dev and production use separate Supabase projects, Redis instances and
  RabbitMQ virtual hosts. No table has an `environment` column.

## Target package layout

```text
api/
  openapi.yaml                 Go-owned public contract
  spec.go                      embedded OpenAPI bytes
cmd/
  api/                         dependency wiring and HTTP lifecycle
  worker/                      RabbitMQ consumers and shutdown
  maintenance/                 backfill, replay, taxonomy, retention, eval
  load/                        API and worker load scenarios
internal/
  ai/                          Gemini client, prompts and structured schemas
  auth/                        Supabase JWT verification and user context
  collections/                 collection domain and service
  config/                      the only environment reader
  content/                     global content and immutable version domain
  db/                          connection and migration entry points
  embed/                       embedding client and indexer
  enqueue/                     submission, deduplication and subscriptions
  geo/                         coordinates and spatial value objects
  httpapi/                     routes, middleware, handlers and response shapes
  legacy/                      temporary production read/write adapter
  lifecycle/                   user deletion and global purge
  mapview/                     map and Discover queries
  media/                       bounded external command execution
  metrics/                     metrics, redaction and tracing helpers
  migrations/                  ordered embedded SQL migrations
  notify/                      FCM and notification jobs
  outbox/                      transactional event publisher
  pipeline/                    versioned stages and checkpoints
  platform/                    source-specific handlers behind one interface
  postgres/                    pgx implementations of domain ports
  queue/                       RabbitMQ topology, publisher and consumers
  ratelimit/                   Redis-backed policies
  reels/                       public reel read model
  safehttp/                    SSRF-safe bounded HTTP client
  search/                      dense, lexical and fused retrieval
  sourceidentity/              URL normalization and source identity
  storage/                     Supabase Storage port and implementation
  taxonomy/                    categories, proposals and weekly curation
  workerhealth/                Redis heartbeats and readiness view
```

`internal/legacy` is temporary. Its removal is an explicit final task, not an
informal cleanup.

## Task 1: Merge the decision and planning baseline

Branch: `codex/backend-web-roadmap`, PR #25.

Implement:

- Keep `architecture-decisions.md` as the source for settled product and
  engineering choices.
- Keep `architecture-questionnaire.md` as a short record of the one deferred
  launch gate.
- Keep `backend-and-web-roadmap.md` as the overview.
- Add this file as the exact execution order.
- Link all four files from `docs/plans/active/README.md`.
- Close the environment-column PR because separate infrastructure replaced it.

Test gate:

- `python3 scripts/check_docs.py` passes.
- `git diff --check` passes.
- PR #25 checks pass and no unresolved review thread remains.
- A reviewer can answer where a decision, overview and exact task instructions
  live without using chat history.

Stop condition: no runtime branch is changed before this task merges into
`dev`.

## Task 2: Replace Python compatibility checks with a Go-owned API contract

Branch: rework `go-migration/03-contract-ci` from current `dev`.

Files:

- `api/openapi.yaml` defines every active `/api/v2` endpoint, security scheme,
  request, response, error and cursor.
- `api/spec.go` embeds the contract for tests and client generation.
- `.github/workflows/contract.yml` publishes the exact spec from each merged
  commit as a content-addressed artifact and records its SHA-256 digest.
- `internal/httpapi/routes.go` holds one route table with method, path,
  `AuthMode` and handler.
- `internal/httpapi/contract_test.go` compares the route table with OpenAPI and
  validates representative response fixtures.
- `api/fixtures/` holds small stable examples for reels, jobs and errors.
- `.github/workflows/check.yml` validates OpenAPI and runs `make check`.
- `scripts/check_contract.py` checks duplicate operation IDs and missing route
  coverage. It does not inspect Python.

Implement:

- Move the current Go health, account, reels and processing-job reads to the
  Go-owned v2 contract before this backend serves clients.
- Define bearer-authenticated `POST /api/v2/processing-jobs/reels` with `{url,
  collection_ids?}` and `Idempotency-Key`.
- Define share-token-authenticated `POST /api/v2/native-shares/reels` with
  `{raw_payload_text, collection_ids?}` and `Idempotency-Key`. It resolves and
  enqueues in one request so a native extension cannot lose state between two
  calls.
- Keep bearer-authenticated `POST /api/v2/share/resolve` only for interactive
  URL preview. Mint and revoke native share tokens through bearer-authenticated
  v2 endpoints.
- Use one error envelope: `{"error":{"code":"...","message":"...",
  "request_id":"...","retryable":false,"details":{}}}`. Details are optional
  and never carry driver text.
- Use opaque cursor pagination only. List order is `saved_at DESC`, then
  `user_save_id DESC`; the default page size is 25 and maximum is 100.
- Define OpenAPI schemes for Supabase bearer and `X-Share-Token`. Every route
  declares `public`, `bearer`, `share-token` or `public-share`; there is no
  boolean auth shortcut. Contract tests exercise every mode.
- Describe `200` reuse, `202` active/new work, `400`, `401`, `404`, `409`, `422`,
  `429` and `503` where they apply.
- Do not retain bare aliases or deferred Python-only routes in the final spec.
- Run a pinned OpenAPI breaking-change check against the latest released v2
  artifact. Breaking v2 changes fail CI and require a new major path.

Test gate:

- Contract parsing fails on an invalid or duplicate operation.
- The published artifact digest matches the repository file byte for byte.
- Every registered route appears once in OpenAPI; every OpenAPI operation is
  registered once.
- Handler tests validate status, content type and JSON shape against the spec.
- `make check` passes locally and in hosted CI.

Stop condition: web client work cannot start until this contract merges.

## Task 3: Implement source identity and safe URL resolution

Branch: rework `go-migration/04-source-identity` from current `dev`.

Files:

- `internal/sourceidentity/sourceidentity.go` contains the identity value.
- `internal/sourceidentity/resolver.go` maps supported hosts and paths.
- `internal/sourceidentity/platforms.go` holds platform-specific canonical ID
  extraction.
- `internal/sourceidentity/share.go` extracts candidate URLs from native share
  text.
- `internal/safehttp/safehttp.go` owns all provider-independent outbound HTTP
  safety.

Implement:

- Accept URLs of at most 2,048 characters and only `http` or `https`.
- Lowercase scheme and host, remove default ports and fragments, normalize
  percent encoding, remove known tracking parameters and sort retained query
  parameters.
- Prefer stable platform content IDs over normalized URLs.
- Add an access-scope component so proven public content can deduplicate globally
  while private or credential-scoped content cannot cross users. Unknown and
  authenticated sources start in a user-specific scope. A worker may promote
  them only after fetching the source without user credentials proves it public.
- Resolve DNS before each connection and after every redirect. Reject loopback,
  link-local, private, multicast, unspecified and metadata-service addresses.
- Reject URL user information, IPv6 zone identifiers and non-80/443 ports unless
  a provider has an explicit reviewed allowlist. Check every resolved address,
  not only the first.
- Limit redirects to five and prevent DNS rebinding by dialing the checked IP
  while preserving TLS hostname verification.
- Disable environment proxies, follow redirects manually and strip authorization
  whenever origin changes. Derive client IP only from the socket peer or headers
  supplied by explicitly trusted proxies.
- Bound response headers and body sizes. Verify image and media signatures rather
  than trusting the `Content-Type` header alone.
- Cap decompressed bytes as well as wire bytes and set separate DNS, connect,
  response-header, idle-body and total timeouts.
- Return typed domain errors that HTTP handlers map to stable public codes.

Test gate:

- Table tests cover every supported platform and common share-text forms.
- Tests cover IPv4, IPv6, encoded hosts, redirects, mixed DNS answers, DNS
  rebinding and metadata endpoints.
- Fuzz tests never panic and never accept a private target.
- Real public fixture URLs are optional smoke tests, not unit-test dependencies.
- `make check` passes.

## Task 4: Create the canonical database schema

Branch: rework `go-migration/05-global-schema` from current `dev`.

Files:

- `internal/migrations/migrations.go` embeds, orders and applies SQL under a
  PostgreSQL advisory lock.
- `internal/migrations/00001_core.sql` creates the private `reelpin` schema and
  core tables.
- `internal/migrations/00002_processing.sql` creates jobs, runs, checkpoints,
  idempotency and outbox tables.
- `internal/migrations/00003_taxonomy.sql` creates categories and proposal audit
  tables.
- `internal/migrations/migrations_integration_test.go` tests empty and repeated
  migration runs.
- `cmd/maintenance/main.go` exposes `migrate` and reports the applied version.

Core model:

- `contents`: UUIDv4 ID, source platform, content type, normalized URL, stable
  source ID, access-scope hash, current version ID and timestamps.
- `content_versions`: UUIDv4 ID, content ID, processor, prompt, schema and model
  versions, title, summary, caption, transcript, tags, facts, raw structured
  JSONB, media metadata, extraction status and creation time. Rows are immutable.
- `user_saves`: UUIDv4 ID, Supabase user ID, content ID and saved time. Unique on
  `(user_id, content_id)`.
- `processing_runs`: one global execution for content identity plus processor
  version, with lease owner, lease expiry, monotonically increasing lease
  generation and terminal state.
- `processing_jobs`: one private user-visible job linked to a run and eventual
  user save.
- `processing_stage_results`: stage name, stage version, input hash, output
  reference, attempt count, timings and error class.
- `outbox_events`: event type, routing key, payload, availability time, publish
  attempts and published time.
- `idempotency_keys`: user ID, endpoint, key, request hash, stored response and
  expiry time.
- `account_deletion_requests`: user ID, requested time, database-cleanup state,
  identity-cleanup state, attempts and last error class.
- `categories`, `category_aliases`, `category_proposals` and `taxonomy_runs`:
  active taxonomy plus full approval and rollback history.

Constraints and indexes:

- Global public identity is unique on platform, content type, stable source ID
  and access scope. A normalized URL hash is the fallback identity. Processor
  version belongs to processing runs and content versions, not content identity.
- Foreign keys have explicit delete behavior. User-owned rows reference
  `auth.users.id`; integration setup creates a production-shaped `auth.users`.
- Tags use `text[]` with a GIN index. Stable query fields are typed columns;
  provider-specific and versioned extraction data stays in JSONB.
- Every job state and content type uses a database check constraint.
- Index every foreign key and every proven list, lease and outbox access path.
- Revoke direct public access to the private schema. The Go database role gets
  only the grants it needs.
- Enforce version immutability with grants and triggers. A composite foreign key
  ensures a content row can point only to one of its own versions. A separate
  maintenance role may delete versions only for an audited global purge.
- Generate application IDs with `github.com/google/uuid`; remove the old
  `internal/uuid` package when its branch is reworked.

Test gate:

- Migrations succeed on empty PostgreSQL 16 and the development Supabase
  project.
- Running migrations twice changes nothing.
- Constraint tests reject duplicate user saves, duplicate active runs, invalid
  states and broken ownership references.
- The application role cannot update/delete a version or point a content row at
  another content row's version.
- An application-role integration test cannot read another schema or bypass a
  user-scoped repository method.
- Schema dump review shows no `environment` column and no destructive statement.

## Task 5: Backfill and bridge existing production data

Branch: rework `go-migration/06-backfill` from current `dev`.

Files:

- `internal/backfill/backfill.go` coordinates bounded resumable batches.
- `internal/backfill/store.go` contains SQL only.
- `internal/backfill/verify.go` produces counts and mismatch samples.
- `internal/backfill/changes.go` consumes an append-only legacy change log.
- An additive migration installs narrow `AFTER INSERT OR UPDATE` and
  `BEFORE DELETE` triggers on inventoried legacy tables. Each event stores table,
  primary key, operation and the minimum old/new fields needed for replay.
- `docs/migrations/production-inventory.md` lists every production table, its
  mapping, ID rule, backfill order, coexistence rule, deletion rule and
  verification query.
- `internal/legacy/reels.go` reads the current `public.reels` shape during the
  coexistence window.
- `internal/legacy/persist.go` writes canonical and required legacy records in
  one database transaction while Flutter still uses Python.
- `cmd/maintenance/backfill.go` exposes `--dry-run`, `--batch-size`,
  `--checkpoint` and `--report`.

Implement:

- Snapshot the current production schema and classify every legacy column as
  mapped, transformed, intentionally retained only in JSONB, or rejected.
- Inventory reels, collections, collection items, members, invites, folders,
  locations, user pin state, share tokens, push tokens, notifications, jobs,
  profiles, reminders, price tracking and cache/history tables. Explicitly mark
  tables that are rebuilt or intentionally not migrated.
- Preserve `public.reels.id` as `user_saves.id` and `user_id` unchanged.
- Preflight every legacy user ID. Separate Auth-matching UUIDs, orphan UUIDs and
  non-UUID values. Quarantine invalid rows; never cast blindly or attach them to
  another account.
- Derive source identities with the exact Task 3 resolver version.
- Create one content row per public identity and one content version for each
  distinct legacy extraction. Never merge private or credential-scoped data
  across users.
- Keep the production read path on the legacy adapter during web-first
  coexistence. New Go processing writes canonical data and the legacy reel row
  atomically so Python-era Flutter can still read it.
- Keep a mutation matrix for Python and Go insert, update and delete behavior.
  Every Go mutation during coexistence changes canonical and legacy rows in one
  transaction. Python may continue changing legacy rows, so canonical reads stay
  disabled until Python writes are fenced and a final reconciliation passes.
- Consume trigger events continuously and idempotently so Go deduplication sees
  Python-created content during coexistence. Encrypt or avoid sensitive values,
  purge consumed events on schedule and include pending events in account
  deletion.
- Make each batch commit independently. Persist the last key and every rejected
  row so reruns do not repeat completed work.
- Store checkpoints in PostgreSQL, capture a fixed high-water mark, rescan rows
  changed during each pass and perform a write-fenced final pass. Produce exact
  before/after counts, duplicates, unmapped rows, foreign-key failures,
  deterministic hashes for every migrated row and 100 API-level comparisons.
- Do not delete or rename a production table or column in this task.

Test gate:

- Run against empty dev, a production-shaped fixture and an anonymized schema
  copy.
- Kill the command mid-batch, restart it and prove the final result matches one
  uninterrupted run.
- Run twice and prove row counts and version counts do not increase.
- Concurrently insert, update and delete legacy records while backfill runs,
  drain the change log and prove canonical hashes match the final legacy state.
- Compare old and canonical feed/detail output for 100 deterministic IDs.
- The report has zero unexplained mismatch and zero rejected authenticated-user
  row before production use. Malformed rows remain quarantined with an owner.

## Task 6: Add Redis rate limits and ephemeral worker state

Branch: rework `go-migration/07-redis-controls` from current `dev`.

Files:

- `internal/ratelimit/ratelimit.go` implements atomic Redis token buckets.
- `internal/ratelimit/policies.go` names endpoint policies.
- `internal/workerhealth/workerhealth.go` writes and reads expiring heartbeats.
- `internal/providers/cooldown.go` stores provider cooldowns.
- `internal/config/config.go` validates Redis settings and timeouts.

Implement:

- Submission: 5 per authenticated user per hour and 20 per IP per hour. Redis
  owns these rate windows. The enqueue PostgreSQL transaction enforces at most 2
  active jobs under a per-user advisory lock.
- Search: 30 per user and 90 per IP per minute.
- Apply CAPTCHA only after suspicious behavior. Verify its result server-side.
- Hash user IDs and IPs in Redis keys and logs with a rotated secret salt.
- Worker heartbeat interval is 15 seconds; readiness treats 90 seconds as stale.
- Provider cooldowns and rate state may disappear when Redis restarts. Durable
  jobs, checkpoints and outbox events never live only in Redis.
- Do not add response caching in this task.
- Fail closed for provider-costing submissions when Redis is unavailable. Keep
  authenticated reads available and report degraded readiness.

Test gate:

- Integration tests use a real Redis instance and concurrent callers.
- Boundary tests cover last allowed request, first rejected request and expiry.
- Redis restart loses only disposable state; PostgreSQL work remains replayable.
- Logs and Redis keys contain no raw user ID, IP, token or URL.

## Task 7: Build the RabbitMQ worker foundation

Branch: rework `go-migration/08-rabbit-worker` from current `dev`.

Files:

- `internal/queue/topology.go` declares exchanges, queues and bindings.
- `internal/queue/message.go` defines versioned event envelopes containing only
  event ID, schema version, event type, run ID, dispatch generation, creation
  time and trace context. Workers load URLs and provider state from PostgreSQL.
- `internal/queue/publisher.go` uses publisher confirms and mandatory routing.
- `internal/queue/consumer.go` owns manual acknowledgement and shutdown.
- `internal/outbox/outbox.go` claims and publishes committed events.
- `internal/lease/lease.go` owns PostgreSQL run leases.
- `cmd/worker/main.go` wires one process with bounded consumers.
- `cmd/maintenance/rebuild_queue.go` reconstructs unfinished broker work from
  PostgreSQL after broker loss.

Topology:

- Topic exchange `reelpin.processing`.
- Durable classic queues `reelpin.processing.media` and
  `reelpin.processing.light`.
- Retry queues for 30 seconds and 5 minutes using TTL and dead-letter routing.
- Media and light have separate retry queues for each delay so one workload
  cannot block the other or lose its return route.
- Separate durable notification queue and notification retry queues.
- Class-specific dead-letter queues for malformed envelopes, unknown message
  versions and failures that prevent a durable state update. Normal terminal
  business failures update the private job and are acknowledged.
- Persistent messages, publisher confirms, mandatory publishing and manual
  acknowledgements.

Worker behavior:

- One Go process. Development starts one media, one light and one notification
  consumer.
- Each workload uses its own AMQP channel and prefetch setting. A busy media
  channel cannot consume the light queue's credit.
- Initial consumer QoS is one message per channel. Supervised channels reconnect
  with bounded exponential backoff and jitter, then redeclare topology.
- Routing is deterministic. Platform and source metadata select media or light;
  Gemini never selects a queue.
- Unknown generic URLs start in light inspection. If media is found, commit the
  run transition and new outbox event before acknowledging the light message.
- Prefetch equals consumer count. A consumer obtains a database lease before a
  stage and renews it while work continues.
- Claiming a lease increments its generation using PostgreSQL time. Renewal and
  every state commit match run ID, owner and generation. A zero-row update means
  the worker cancels its work and discards the stale result. A worker never
  releases a lease while its goroutine or child process may still run.
- Acknowledge only after the durable effect commits. Requeue only through the
  retry topology, never a tight nack loop.
- On shutdown, stop deliveries, wait for the current bounded grace period,
  release or expire leases and close broker connections.
- Claim outbox rows in bounded batches with `FOR UPDATE SKIP LOCKED` and an
  expiry. Multiple publishers may run, and duplicate publication remains safe.
- Database stage-attempt state, not broker delivery headers, decides whether the
  next failure retries or becomes terminal.
- Put the outbox event ID in AMQP `MessageId`. Mark it published only after a
  broker acknowledgement and no mandatory return. An unknown confirm after
  connection loss remains unpublished and is safe to resend.
- Cap message size and reject unknown versions. Never place a URL, token, cookie,
  prompt or provider response in RabbitMQ.

Test gate:

- Docker-backed integration tests prove declare, publish, confirm, consume,
  retry, dead-letter and reconnect behavior.
- Kill the worker before and after a database commit and prove one committed
  local effect. Repeated external calls may occur when a provider has no
  idempotency support; record provider call IDs and cost.
- Stop RabbitMQ after the database commit and prove the outbox publishes after
  recovery.
- A long media fixture does not prevent a light fixture from completing.
- RabbitMQ data survives a container restart on its persistent volume.
- Destroy the RabbitMQ volume, start an empty broker and run
  `rebuild-queue --broker-empty`. Reconstruct every unfinished run, index and
  notification effect as a uniquely keyed outbox event without replaying raw
  dead-letter payloads.
- Expire a live lease and prove the sweeper creates one resume outbox event while
  the fenced old worker cannot commit.

## Task 8: Implement enqueue, idempotency and global deduplication

Branch: rework `go-migration/09-enqueue-dedup` from current `dev`.

Files:

- `internal/enqueue/service.go` contains the submission use case.
- `internal/enqueue/store.go` defines the transaction port.
- `internal/postgres/enqueue.go` implements the transaction.
- `internal/httpapi/enqueue.go` decodes, validates and presents results.
- `internal/httpapi/share.go` exposes share-text resolution without processing.
- `internal/httpapi/native_share.go` resolves and enqueues native shared text
  atomically under `X-Share-Token` authentication.

Transaction behavior:

- Take a transaction-scoped advisory lock for the authenticated user, count
  active private jobs and reject the third with `429 active_job_limit`.
- Validate the idempotency key and compare its stored request hash. Reusing a key
  for a different body returns `409`.
- Normalize the URL and calculate global identity plus access scope.
- Treat unresolved and credential-backed sources as user-scoped. If a worker
  later proves public access, promotion or merge runs in a transaction and
  preserves both users' private saves.
- If this user already saved completed content, return that reel without a new
  run.
- If globally completed public content exists for another user, create this
  user's save and a completed private job without reprocessing.
- If an active global run exists, create or reuse this user's private job linked
  to that run. Do not publish a second processing event.
- Otherwise create content, run, private job and outbox event in one transaction.
- Store idempotency results for 24 hours. Content uniqueness is permanent.
- Filing into optional collections occurs in the same transaction when the
  collections task is available; until then the contract rejects that field as
  unsupported rather than silently dropping it.

Test gate:

- Concurrent submissions from one user create one save, one job and one run.
- Concurrent submissions from two users create two private jobs and one global
  run for public content.
- Private access scopes never deduplicate across users.
- Two users submitting an unknown private URL cannot see or reuse one another's
  extracted result.
- A completed unreferenced public content row is reused without provider calls.
- Transaction rollback leaves no content, job or outbox fragment.
- Handler tests cover `200`, `202`, `409`, `422`, `429` and dependency failure.
- Native-share tests cover token mint, expiry, revocation, raw text resolution,
  collection filing and retry with the same idempotency key.

## Task 9: Build the versioned processing pipeline

Branch: rework `go-migration/10-pipeline-core` from current `dev`.

Files:

- `internal/pipeline/pipeline.go` defines stage order and orchestration.
- `internal/pipeline/checkpoint.go` validates reusable stage results.
- `internal/pipeline/failure.go` classifies retryable and terminal errors.
- `internal/pipeline/persist.go` commits content version, user saves and outbox
  notifications.
- `internal/ai/gemini.go` calls Gemini with structured response schemas.
- `internal/ai/schema.go` defines versioned extraction structs and validators.
- `internal/ai/prompts.go` stores prompt versions and active taxonomy input.

Stages and limits:

1. Prepare, 30 seconds.
2. Download when required, 180 seconds.
3. Transcribe when required, 300 seconds.
4. Extract structured content, 90 seconds.
5. Categorize against active taxonomy, 45 seconds.
6. Persist immutable content version and private saves, 30 seconds.

Indexing is separate durable light work with a 60-second attempt timeout. A
search-index failure cannot change a completed save into a failed job.

Implement:

- The whole run expires after 30 minutes.
- A stage result is reusable only when stage version and input hash match.
- A failed stage executes at most three times, after 30 seconds and then 5
  minutes. Completed earlier stages are not repeated.
- On a retryable stage failure, update the PostgreSQL attempt and insert a
  delayed outbox event in one transaction. Set its availability to the maximum
  of normal backoff, provider `Retry-After` and shared cooldown, bounded by the
  run deadline. RabbitMQ TTL retry queues handle only transport failures that
  happen before durable failure state can be written.
- Validate Gemini structured output again in Go. Provider schema enforcement
  reduces malformed output; it does not replace domain validation.
- Preserve raw provider output only in restricted diagnostic storage with a
  retention limit. Public responses use validated fields.
- Persist the final version, set `contents.current_version_id`, create subscriber
  saves, complete private jobs and write notification/index events atomically.
- Terminal failures complete each subscriber job with a stable public error and
  retain internal failure details only in logs and stage rows.

Test gate:

- Unit tests cover every state transition and error class.
- Integration tests crash and resume after each stage boundary.
- Invalid structured output cannot create a content version.
- Reprocessing under a new prompt/model/schema creates a new immutable version;
  normal reads stay on the prior version until the new one completes.
- Two consumers cannot hold the same active lease.

## Task 10: Add bounded media and provider infrastructure

Branch: start `go-migration/10a-provider-infrastructure` from current `dev`.

Files:

- `internal/media/command.go` runs `yt-dlp` and `ffmpeg` without a shell.
- `internal/media/tools.go` verifies binaries and versions at startup.
- `internal/storage/storage.go` owns temporary and Supabase Storage operations.
- `internal/apify/apify.go` wraps actor calls with limits and typed errors.
- `internal/providers/limits.go` applies concurrency caps and cooldowns.

Implement:

- Use argument arrays, timeouts, process groups and bounded stdout/stderr.
- Allow `yt-dlp` only for explicit supported platform hosts and place its process
  behind an egress boundary that blocks all non-public destinations. Generic
  media downloads use `safehttp`; `ffmpeg` accepts local files only. Disable
  environment proxies and reject URL credentials.
- Limit HTML to 5 MiB, images to 20 MiB each, media to 500 MiB total, duration
  to 30 minutes and temporary disk to 1 GiB per job.
- Stop media admission at 80% disk. Always clean temporary files after success,
  terminal failure, cancellation and process crash recovery.
- Permit two concurrent Gemini calls, one call per Apify actor/account and four
  light HTTP requests within the outer worker limits.
- Port only Python fallbacks proven by fixtures or production evidence. Record
  timeout, expected provider cost class and failure mapping for each fallback.
- Pin and checksum external binaries in the container build.

Test gate:

- Tests prove cancellation kills child processes and cleanup removes all files.
- Oversized, long-duration and false-MIME fixtures fail before provider work.
- External-tool tests cover redirects, DNS rebinding and metadata/private
  targets through the same egress boundary.
- Provider concurrency never exceeds configured caps under race tests.
- A missing or wrong binary version fails worker readiness with a clear reason.

## Task 11: Implement Instagram first

Branch: rework `go-migration/11-instagram` from current `dev`.

Files:

- `internal/platform/platform.go` defines the handler registry and result type.
- `internal/platform/instagram/instagram.go` selects tested fallbacks.
- `internal/platform/instagram/page.go` parses bounded page metadata.
- `internal/platform/instagram/testdata/` contains sanitized reel, carousel,
  post, login-wall and unavailable fixtures.

Implement:

- Support every Instagram content type accepted by the Python dev backend.
- Keep provider selection inside the Instagram handler. Return prepared text,
  media references and provenance to the common pipeline.
- Classify login wall, removed content, private content, rate limiting, provider
  outage and malformed response separately.
- Never log cookies, provider tokens, full signed media URLs or raw user URLs.

Test gate:

- Recorded fixtures cover the successful and failure paths without network.
- One opt-in development smoke test processes a real permitted URL end to end.
- Two users submitting that URL produce one content version and two saves.
- Queue retry and terminal failure behavior match Task 9.

## Task 12: Implement video, place and generic web sources

Branch: rework `go-migration/12-video-web-platforms` from current `dev`.

Files:

- `internal/platform/youtube/`, `tiktok/` and `pinterest/` contain separate
  handlers and fixtures.
- `internal/platform/web/` contains metadata, article and media-page handling.
- `internal/platform/places/` contains provider-backed place metadata.

Implement:

- Route YouTube and TikTok to media unless metadata proves no media step is
  required.
- Route Pinterest, place URLs and normal HTML to light.
- A generic light handler may escalate to media only by committing a transition
  and outbox event before acknowledging its message.
- Extract canonical URLs, titles, descriptions, images and text with bounded
  parsers. Respect Task 3 network rules for every follow-up request.
- Keep platform-specific response structs private to their package.

Test gate:

- Every handler has recorded success, private/unavailable, throttled and
  malformed fixtures.
- A long media job and short article job run together; the article completes
  without waiting for media.
- Escalation survives a worker crash without losing or duplicating the run.
- Opt-in dev smoke tests record provider call count, duration and result size.

## Task 13: Implement X, LinkedIn and Reddit

Branch: rework `go-migration/13-social-platforms` from current `dev`.

Files:

- `internal/platform/social/x.go`, `linkedin.go` and `reddit.go` contain isolated
  handlers.
- Each handler has local recorded fixtures and no cross-platform conditionals.

Implement:

- Match currently supported Python content forms and fallbacks.
- Use light routing for text/API results and media routing only when a download
  or transcription is actually needed.
- Track provenance so a result shows which fallback produced each field.
- Apply provider timeout, cooldown and concurrency behavior from Tasks 6 and 10.

Test gate:

- Fixture, retry, fallback and terminal-error tests pass for each platform.
- The registry rejects duplicate platform registration.
- All currently supported Python platforms appear in a parity matrix with a
  passing fixture and an opt-in dev smoke result.

## Task 14: Add evolving taxonomy and weekly curation

Branch: start `go-migration/13a-taxonomy-curator` from current `dev`.

Files:

- `internal/taxonomy/service.go` reads the active category tree.
- `internal/taxonomy/proposals.go` records proposed categories.
- `internal/taxonomy/curator.go` applies the weekly policy with stable model
  `gemini-3.5-flash-lite`.
- `cmd/maintenance/taxonomy.go` provides `curate-taxonomy --dry-run` and
  `rollback-taxonomy --run-id`.
- `deploy/reelpin-taxonomy.service` and `.timer` run Sunday at 02:00 UTC.

Implement:

- Inject active category IDs, names and descriptions into the extraction prompt.
- Require one category, permit one subcategory and many free-form tags.
- If nothing fits, Gemini emits a proposal separately from the selected fallback
  category. A normal processing job cannot insert an active category.
- Weekly curation uses the selected lightweight Gemini model and structured
  output. Auto-approval requires at least three distinct content proposals,
  confidence at least 0.90 and a maximum of five additions per run.
- Detect normalized-name duplicates and ask the model to merge or alias rather
  than add.
- Store the input set, model, prompt version, decision, before/after state and
  inverse rollback action for every change.
- A failed scheduled run changes nothing and can wait one week.

Test gate:

- Dry-run never mutates rows.
- Below-threshold and duplicate proposals are not activated.
- Structured-output or model failure leaves the active taxonomy unchanged.
- Rollback restores the prior active tree and keeps the audit history.
- Pipeline categorization remains deterministic against a pinned taxonomy
  version during a run.

## Task 15: Implement collections and sharing

Branch: rework `go-migration/14-collections` from current `dev`.

Files:

- `internal/collections/` owns collections, membership, invites and share links.
- `internal/postgres/collections.go` contains user-scoped SQL.
- `internal/httpapi/collections.go` exposes the OpenAPI operations.

Implement:

- A collection has one owner and may have editor or viewer members. Item
  membership references `user_saves.id`, not global content.
- Add, list, rename and delete collections; add and remove reel membership.
- Create unguessable expiring share tokens and explicit read-only shared views.
- Owners manage members and invites; editors manage items; viewers only read.
  Public share links remain read-only and do not create collection membership.
- Apply collection IDs during enqueue in the same transaction as the eventual
  user save subscription.
- Return `404` for another user's collection, save or invite.

Test gate:

- Two-user isolation tests cover every query and mutation.
- Duplicate membership is idempotent.
- Expired, revoked and malformed share tokens reveal no collection metadata.
- Enqueue plus collection filing survives duplicate delivery.

## Task 16: Implement map and Discover

Branch: rework `go-migration/15-map-discover` from current `dev`.

Files:

- A migration enables PostGIS and creates normalized `content_locations` plus
  user-owned pin preferences.
- `internal/geo/` owns coordinate validation.
- `internal/mapview/` owns map bounds, Discover and place search.
- `internal/httpapi/map.go` exposes contract handlers.

Implement:

- Store points as PostGIS geography with source, confidence and normalized place
  fields.
- Query only locations reachable through the authenticated user's saves.
- Use bounding-box and distance queries with spatial indexes.
- Keep manual pins and hidden-pin choices user-specific.
- Bound external place search and cache only provider results that contain no
  user data.

Test gate:

- Tests cover invalid coordinates, antimeridian bounds and distance ordering.
- Query plans use the spatial index on a production-shaped data set.
- Two-user tests prove one user's hidden/manual state does not affect another.

## Task 17: Implement notifications

Branch: rework `go-migration/16-notifications` from current `dev`.

Files:

- `internal/notify/` owns device tokens, FCM requests and delivery records.
- `internal/httpapi/notifications.go` owns user token operations.
- `cmd/maintenance/notifications.go` owns privileged campaign sends.

Implement:

- Use the separate durable notification queue and one light consumer.
- Create completion/failure notifications from outbox events after job state
  commits.
- Store device tokens encrypted or in the existing protected form; never log
  them.
- Remove invalid tokens on permanent FCM errors and retry transient failures.
- Keep campaign administration out of the public product API.

Test gate:

- Duplicate events produce one logical user notification.
- Provider retry does not change job state.
- Another user cannot list or delete a device token.
- FCM credentials and tokens never appear in logs or fixtures.

## Task 18: Implement deletion, retention and global purge

Branch: rework `go-migration/17-lifecycle` from current `dev`.

Files:

- `internal/lifecycle/service.go` owns reel and account deletion.
- `internal/lifecycle/retention.go` owns temporary-data cleanup.
- `internal/lifecycle/purge.go` owns source blocklist and global removal.
- `cmd/maintenance/retention.go` and `purge.go` expose privileged commands.

Implement:

- Reel deletion removes only that user's save and memberships. The global public
  content and versions remain reusable.
- Account deletion remotely validates the Supabase session, removes user saves,
  collections, memberships, invites, device tokens, profile, jobs, idempotency
  responses, notifications, user-bearing outbox payloads and private processing
  data, then deletes the Supabase identity.
- Record a durable deletion request before work begins. Block that subject while
  deletion is pending and retry Supabase identity deletion until it is confirmed;
  database cleanup and Auth deletion cannot be one transaction.
- Remove every user link from retained global public content.
- Purge private content when its final owning user is deleted.
- Purge by source identity removes derived text, embeddings, stored media and
  all saves when privacy, legal, private-source or blocklist policy requires it.
- Temporary media cleanup runs after every terminal job and a scheduled sweep
  removes abandoned directories.

Test gate:

- Two users sharing content: deleting one save does not affect the other.
- Deleting the last save leaves reusable public content with no user reference.
- Account deletion leaves no user-owned row and reports identity deletion
  truthfully.
- Crash tests at every deletion step resume from the durable request and do not
  restore access for the pending subject.
- Purge removes every derivative and prevents immediate reingestion.

## Task 19: Add versioned embeddings

Branch: rework `go-migration/18-embeddings` from current `dev`.

Files:

- A migration enables `vector`, adds embedding model/version/dimension columns
  and creates the matching vector index.
- `internal/embed/document.go` builds text from title, summary, tags and facts.
- `internal/embed/gemini.go` calls the configured embedding model.
- `internal/embed/indexer.go` consumes durable index events idempotently.
- `cmd/maintenance/embeddings.go` backfills by model/version.

Implement:

- Do not embed transcripts in the first version.
- Use one fixed `vector(768)` column and index for the active set. Start
  evaluation with stable model `gemini-embedding-2` and 768 output
  dimensions. The model and dimension are configuration validated at startup,
  not scattered string literals.
- Pin model name, requested output dimension and document-builder version in each
  row. Never compare vectors from different model/version/dimension sets.
- Build the document in a fixed labeled order and hash it. Skip provider calls
  when the same hash and model version already exist.
- Index asynchronously after content persistence. Search freshness target is 60
  seconds.
- Keep the prior embedding set until the new set passes evaluation and switches
  atomically.
- A future dimension change creates a new column or table and index. Different
  dimensions never share one indexed column.

Test gate:

- Unit tests make document text and hashes deterministic.
- Integration tests prove duplicate index events do not call the provider twice.
- Backfill resumes from a checkpoint and reports success, failure and cost.
- The chosen model and dimension pass a development retrieval evaluation before
  becoming active.

## Task 20: Implement measured hybrid search

Branch: rework `go-migration/19-hybrid-search` from current `dev`.

Files:

- A migration enables `pg_trgm`, adds generated full-text columns and indexes.
- `internal/search/search.go` defines filters and ranked results.
- `internal/search/service.go` obtains query embeddings and degrades to lexical.
- `internal/search/eval.go` calculates Recall@10, nDCG@10, zero-result quality and
  latency.
- `api/eval/search-v1.json` holds at least 50 labeled real queries.
- `api/eval/REPORT.md` records the exact model, data and measured result.

Implement:

- Dense search uses the active pgvector embedding set.
- Full-text search handles words and phrases; trigram handles partial and typo
  matches.
- Apply authenticated user ownership and all filters inside each SQL arm before
  ranking.
- Fuse ranked lists with reciprocal rank fusion using `k=60`.
- Use an evaluation-selected relevance threshold. Do not copy an unmeasured
  threshold from the old branch.
- If query embedding fails, return lexical results and mark the degradation in
  metrics, not in private response data.

Test gate:

- At least 50 real labeled queries reach Recall@10 0.85 and nDCG@10 0.75.
- Explicit unrelated queries return zero results.
- Development p95 is below 1.5 seconds with production-shaped row counts.
- Two-user and filter tests prove no cross-user candidate enters any search arm.
- `api/eval/REPORT.md` is generated from the checked-in set, not written from
  memory.

## Task 21: Add observability, readiness and load evidence

Branch: rework `go-migration/20-operations` from current `dev`.

Files:

- `internal/metrics/` owns route-shaped metrics, redaction and trace sampling.
- `internal/httpapi/metrics.go` instruments HTTP without raw path IDs.
- `cmd/worker/metrics.go` instruments queue age, stages and providers.
- `cmd/load/` runs named read, search, enqueue and mixed scenarios.
- `deploy/alerts.yml` contains tested alert rules.
- `deploy/alloy/` contains the Grafana Alloy collection configuration.
- `docs/operations.md` contains runbooks for each alert.

Implement:

- Metrics cover request rate, errors, latency, pool saturation, outbox age,
  ready-job age, dead letters, stage duration, provider failures and worker
  heartbeat age.
- Logs use request, job, run and trace IDs. Hash user IDs and remove tokens,
  cookies, full URLs, prompt bodies and provider responses.
- Send Prometheus and OpenTelemetry data through Grafana Alloy to Grafana Cloud
  Free. Trace 10 percent of normal requests and all errors. Keep raw metrics,
  logs and traces for the available 14 days, and retain small daily SLO summary
  rows for 90 days. Alert at 80 percent of any free-tier ingestion or series
  limit rather than silently creating paid usage.
- API readiness fails only when the core read path, chiefly PostgreSQL, cannot be
  served. Redis, RabbitMQ, outbox age and worker freshness are reported as
  degraded capabilities and metrics; provider-costing endpoints return stable
  `503 processing_unavailable` while their safety dependencies are unavailable.
  Liveness checks only the process.
- Initial API SLO is 99.5 percent monthly availability and read p95 below 800ms.
  Measure processing per platform before setting processing SLOs.
- Test alerts for 5xx above 2 percent with at least 100 requests in 5 minutes,
  oldest ready job above 10 minutes, any dead letter, worker stale at 90
  seconds, disk above 80 percent and 5 provider failures in 5 minutes.
- Queue metrics are split by media, light and notification for ready, unacked,
  retry, redelivery and dead-letter counts. Also record oldest runnable database
  work, lease-renewal failures, fencing rejections, provider semaphore
  saturation, provider calls/cost and safe-HTTP rejection reasons.

Test gate:

- Metric labels remain bounded under random routes, users and URLs.
- Secret fixtures never appear in captured logs or traces.
- A telemetry outage does not stop API reads or worker checkpoints; buffered
  telemetry is bounded and drops rather than filling application disk.
- Every alert is triggered and cleared in development; record screenshots or
  query output.
- Load report records p50, p95, p99, errors, CPU, memory and database network
  time for the target concurrency.

## Task 22: Build and deploy one tested image to development

Branch: rework `go-migration/22-cutover` from current `dev`.

Files:

- `Dockerfile` uses digest-pinned build and runtime images.
- `docker-compose.yml` runs API, worker, Redis and RabbitMQ with separate health
  checks and persistent RabbitMQ storage.
- `.github/workflows/release.yml` builds once, scans, signs and records the image
  digest and SBOM.
- `deploy/` contains systemd units, Nginx config templates, deploy, smoke and
  rollback scripts.
- `deploy/CUTOVER.md` records exact operator commands and expected output.

Implement:

- Run as a non-root user with a read-only root filesystem and bounded writable
  temporary volume.
- Verify `yt-dlp` and `ffmpeg` downloads by checksum. Fail build on critical or
  high fixable vulnerabilities.
- Apply migrations under an advisory lock before switching the API process.
- Automatically deploy checked `dev` images to the dev host by digest.
- Run API and worker as separate services from the same image.
- Expose `api-dev.reelpin.in` through TLS only after private smoke tests pass.
- Use dedicated development Redis and RabbitMQ credentials and virtual host.
- Production Nginx routes `/api/v1` to Python and `/api/v2` to Go during
  coexistence. The deploy script changes one versioned upstream at a time and
  records the previous target for rollback.

Test gate:

- Fresh EC2 deployment requires only documented secrets and commands.
- Health, JWT read, enqueue, light processing, media processing, retry and
  dead-letter smoke tests pass on dev.
- Restart API, worker, Redis and RabbitMQ separately and record expected
  degradation and recovery.
- Roll back to the prior digest without reversing a database migration.
- Before deploy, run the previous released image against the newly migrated
  schema. Additive SQL is accepted only when that compatibility test passes.

## Task 23: Add web authentication

Repository: `reelpin-web`. Branch from current `origin/main`. Depends on Task 2
for the v2 security contract and Task 22 for the deployed dev origin.

Files:

- `src/lib/supabase/client.ts` creates the browser client.
- `src/lib/supabase/server.ts` creates the request-bound server client.
- `src/proxy.ts` refreshes sessions for Next.js 16.
- `src/app/login/` contains email and Google sign-in.
- `src/app/auth/callback/route.ts` exchanges PKCE codes.
- `src/app/(product)/layout.tsx` protects product routes on the server.
- `src/lib/auth/require-user.ts` is the server-only authorization helper.
- `src/lib/security/origin.ts` validates same-site mutations.

Implement:

- Use `@supabase/ssr` and cookie sessions with the same Supabase identity used by
  Flutter. Support email/password, password recovery, Google PKCE and Apple PKCE
  so every existing sign-in method can reach the web product.
- Allow unverified email accounts, as selected. Apply configured Supabase abuse
  controls and suspicious-traffic CAPTCHA.
- Keep marketing routes public. Proxy refreshes cookies and may redirect early,
  but is not the authorization check. A server-only `requireUser()` runs in
  every protected page load, Server Action and Route Handler; Go remains the
  final product authorization boundary.
- Put dev Supabase values only in local and Vercel preview environments;
  production values only in Vercel production.
- Never expose service-role credentials.
- Install and configure Vitest, Testing Library, Playwright and axe in this task
  so later web tasks inherit one test command and hosted CI gate.

Test gate:

- Playwright covers email sign-up, email login, password recovery, Google and
  Apple callback fixtures, refresh, sign-out, expired session and malicious
  return paths.
- Authenticated pages are dynamic and carry private/no-store caching behavior.
- A preview login uses only the dev Supabase project.

## Task 24: Generate and wrap the web API client

Repository: `reelpin-web`. Branch after Task 23. Depends on Task 2's published
v2 contract artifact.

Files:

- `api/reelpin-v2.openapi.yaml` vendors the exact released Go contract with its
  upstream commit SHA and SHA-256 digest.
- `src/lib/reelpin-api/generated.ts` is generated from that file and never hand
  edited.
- `src/lib/reelpin-api/client.ts` forwards the current access token, applies a
  timeout and maps service errors.
- `src/lib/reelpin-api/types.ts` holds only web-specific view types.
- `scripts/generate-api-client.*` pins the generator and input contract digest.

Implement:

- Publish the Go contract as an immutable release artifact. Vendor that exact
  artifact in the web repository; sibling checkouts are never a CI dependency.
- Make server-side calls from Next.js. Do not call Go directly from browser
  components, so Go needs no browser CORS configuration.
- Forward only the bearer access token and request ID. Never forward Supabase
  refresh tokens or cookies to Go.
- Mark every authenticated request `no-store` and bound it with a timeout.
- Map stable Go error codes to typed outcomes; do not match error text.
- Unknown enum values and additive response fields degrade to an explicit
  fallback state instead of crashing the client.

Test gate:

- CI fails when the generated client differs from the current OpenAPI file.
- Tests prove two sessions produce separate headers and no shared cache entry.
- Error, timeout, malformed response and token refresh paths are covered.

## Task 25: Build the web library and reel detail

Repository: `reelpin-web`. Branch after Task 24. Depends on Task 22's deployed
dev read API and Task 5's legacy reader for old production-shaped data.

Files:

- `src/app/(product)/library/page.tsx` loads the first reel page.
- `src/app/(product)/library/loading.tsx` and `error.tsx` own route states.
- `src/app/(product)/library/[id]/page.tsx` loads one owned reel.
- `src/components/library/` contains filters, list/grid and reel presentation.

Implement:

- Keep marketing at `/`; product lives at `/library`.
- Use a quiet product layout with compact navigation and predictable filters.
- List completed saves only. Apply platform, category, subcategory and saved-date
  filters through URL query parameters.
- Paginate with the opaque cursor. The UI never constructs or decodes it.
- Detail shows title, summary, facts, tags, transcript and locations when present.
- Add empty, loading, retry, offline and unauthorized states.
- Keep list payloads free of transcript; only detail requests it.

Test gate:

- Component tests cover empty, one, many, partial metadata and errors.
- Playwright checks an existing dev user, cursor paging, filters and owned detail.
- Two-user test proves guessing another reel ID returns the not-found experience.
- Browser screenshots pass at phone, tablet and desktop widths with no overlap.
- Keyboard flow and automated AA checks pass.

## Task 26: Add web submission and processing cards

Repository: `reelpin-web`. Branch after Task 25 and backend Tasks 8 through 13.

Files:

- `src/app/(product)/library/actions.ts` submits links server-side.
- `src/components/library/submit-link.*` owns input and submission state.
- `src/components/library/processing-card.*` presents jobs.
- `src/lib/jobs/poller.ts` owns bounded polling.
- `src/app/api/jobs/[id]/route.ts` is the same-origin polling boundary.

Implement:

- Generate one idempotency UUID in client submission state, pass it as a hidden
  field to the Server Action and reuse it for network retries of that attempt.
  A new deliberate submission creates a new key.
- Every submission calls `requireUser()` and the Task 23 same-site origin helper
  before forwarding work to Go.
- Render `202` jobs as loading cards separate from completed reels.
- Poll after 2 seconds, back off to 10 seconds and stop on terminal state or 30
  minutes. Pause while the tab is hidden and refresh immediately when visible.
- Poll the same-origin Next.js route. It calls `requireUser()`, forwards the
  access token to Go and returns `Cache-Control: private, no-store` without a
  refresh token.
- Replace a completed job card with its returned reel without duplicating the
  list entry.
- Explain stable validation and provider errors in user language without showing
  internal details.
- Keep submission disabled in public production until the Task 31 cost gate is
  set; dev remains enabled.

Test gate:

- Tests cover `200` reuse, `202` new/active work, duplicate clicks, timeout,
  terminal failure, reconnect and page refresh.
- End-to-end dev tests process one light and one media URL into visible reels.
- A second user reuses public processed content without a second provider run.

## Task 27: Finish the first web release security and browser tests

Repository: `reelpin-web`, with backend fixes in their owning branches.

Implement:

- Preserve `/privacy`, `/feedback`, `/c/*` and `/go/*` behavior.
- Add same-site checks and CSRF protection to Next.js mutations.
- Enforce web IP limits at the trusted Next.js boundary and forward only an
  HMAC-signed IP bucket to Go. Go verifies the signature; it never trusts a
  client-controlled forwarding header.
- Set CSP, frame, content-type, referrer and permissions headers compatible with
  Supabase Auth and required media.
- Validate all redirect and URL query inputs.
- Test loading, retry and sign-out under slow and failed API responses.

Test gate:

- Full Playwright path: sign up, sign in, empty library, submit, processing card,
  completed reel, detail, sign out.
- Existing-user path: sign in, compare expected library count, open old records.
- Security tests cover CSRF, open redirect, cross-user IDs, cache isolation and
  leaked secrets in browser bundles.
- Vitest, Testing Library, Playwright and axe checks run in hosted CI.
- Mobile, tablet and desktop screenshots are reviewed, not only generated.

## Task 28: Rehearse production data and infrastructure

Repositories: `reelpin-go` and operational configuration. No public traffic.

Implement:

- Restore an anonymized production database copy into a disposable rehearsal
  project and apply all migrations plus backfill.
- Run the 100-record comparison and exact count report from Task 5.
- Deploy the tested digest on the provided Mumbai production EC2 beside Python.
- Configure production RabbitMQ, Redis, persistent volumes, TLS, backups,
  monitoring and secret rotation without changing traffic.
- Measure Tokyo database network time, read p95, worker CPU, memory, disk and
  provider concurrency.
- Verify backups or PITR satisfy RPO one hour; run and time a restore to prove
  RTO four hours. Schedule a monthly restore drill.
- Cover PostgreSQL, Supabase Auth, Supabase Storage objects and required
  encryption keys in the recovery inventory. Take a verified pre-migration
  recovery point, then compare restored row hashes and object checksums.

Test gate:

- Zero unexplained migration mismatch.
- Read p95 is below 800ms and database network time stays below 150ms for the
  test window. Otherwise move compute before launch.
- The host supports one media plus one light consumer. Enable two media plus one
  light only if the recorded load test stays within CPU, memory and disk limits.
- All alert and rollback drills pass on production-shaped infrastructure.

## Task 29: Validate the web privately against production Go

Dependencies: Tasks 21 through 28.

Implement:

- Deploy Go by the exact digest tested in development and rehearsal.
- Keep the legacy reel reader active while Python still serves Flutter.
- Shadow safe production reads and compare status, count, ownership and selected
  fields without exposing shadow output to users.
- Point an access-controlled Vercel preview's server client to the production Go
  origin. Do not expose the product routes publicly yet.
- Run internal smoke tests with an existing account and a new account.
- Observe for 24 hours with automatic rollback on error rate, latency, auth leak
  or data mismatch thresholds.
- Automatically roll back when v2 has at least 100 requests and 5xx exceeds 2
  percent for 5 minutes, or read p95 exceeds 800ms for 15 minutes. Any auth or
  data mismatch is a zero-tolerance alert followed by immediate operator
  rollback; do not automate a state-changing decision from a shadow mismatch.

Test gate:

- Existing production user sees the expected full library and old detail data.
- New user sign-up and empty-library flow work.
- No cross-user, cache or authentication mismatch is found.
- Preview rollback returns to the previous Vercel deployment; Go rollback returns to
  the previous image digest without database rollback.

## Task 30: Move Flutter dev to the Go contract

Repository: `reelpin-app`, branch from updated `dev`.

Implement:

- Vendor the immutable v2 OpenAPI artifact and generate or update the Dart
  client from it. Record the Go commit and artifact digest.
- Change only mappings that the Go contract intentionally changed: cursors,
  error envelope, submission idempotency and processing job states.
- Keep `reel` and reel IDs at the Flutter boundary; updated clients call
  `/api/v2/reels`.
- Point Flutter dev and both native share extensions to `api-dev.reelpin.in`.
- Update Android and iOS share extensions to send raw shared text, collection
  IDs and a stable retry idempotency key to the atomic v2 native-share endpoint.
- Preserve token refresh and long-lived native share-token behavior.

Test gate:

- Android and iOS unit/widget tests pass.
- Physical or simulator tests cover sign-in, feed, detail, filters, submission,
  background polling, native share, collections, map, search and deletion.
- Two-user and expired-token tests pass against Go dev.
- No production endpoint changes in this task.

## Task 31: Set the cost gate and enable production writes

This is the only task that requires a new product value.

Before implementation, calculate from dev and rehearsal evidence:

- Provider cost per successful and failed job by platform.
- P50 and p95 daily submissions at the expected fewer than 100 per day.
- A monthly warning amount and a hard-stop amount.
- Which providers or platforms stop first when the hard limit is reached.

Ask the product owner to approve those two amounts and the stop order. Then:

- Add provider usage counters from actual responses, not estimates where the
  provider supplies usage.
- Alert at the approved warning amount.
- At the hard stop, reject new provider-costing submissions with a stable `503`
  code while reads and already committed work remain available.
- Add an authenticated operational runbook for reopening submissions.
- Publish the authenticated web product only after the warning, hard stop and
  complete submission flow pass. This is the first public web release; it is not
  a read-only launch.

Test gate:

- Synthetic usage triggers the warning and hard stop at the exact values.
- Hard stop makes no new provider call, keeps reads healthy and handles already
  committed jobs according to the approved stop order.
- Public production submission remains disabled until this gate passes.

## Task 32: Cut processing and Flutter production to Go

Dependencies: Task 31 and the Flutter release candidate.

Implement:

- Keep Python `/api/v1` accepting older-client submissions throughout the
  support window. Continuously synchronize its committed legacy mutations into
  canonical data.
- Enable Go enqueue and RabbitMQ processing for production web.
- Release Flutter against Go and keep the Python read path available during the
  mobile adoption window.
- Route `/api/v1` to Python and `/api/v2` to Go. The web server calls v2. Route
  selection never replaces JWT authorization.
- Monitor job age, failures, provider behavior, duplicate runs and saved-content
  counts. Reconcile daily during the observation period.
- Support the current and previous two Flutter releases. Keep the old Python
  write path for at least 90 days after the Go-enabled release and until older
  clients represent less than 1% of active devices for 30 consecutive days,
  whichever is later.

Test gate:

- Web and store-release Flutter submissions complete on Go.
- Native Android and iOS share paths complete on Go.
- Global duplicate test across two users produces one processing run.
- Rollback stops new Go enqueue, drains safe work and restores the prior read
  path without losing committed jobs.

## Task 33: Switch canonical reads and retire Python

Dependencies: stable production observation and measured zero Python writes.

Implement:

- Run the final legacy-to-canonical reconciliation.
- Change the Go reel reader from `internal/legacy` to canonical
  `user_saves`/`contents` queries and deploy by digest.
- Observe both web and supported Flutter versions through the agreed mobile
  adoption window.
- Remove Python traffic, workers and Redis queues only after traffic and queue
  telemetry reach zero.
- Stop new Python v1 writes only after the support window and adoption gate pass;
  drain its Redis jobs to zero and record final counts. Keep a stable retired-v1
  response during the final notice period.
- Remove `internal/legacy` in a separate reviewed pull request.
- Keep legacy production tables read-only for the retention window. Destructive
  schema cleanup requires a new decision and backup proof.
- Retire Pinecone only after pgvector search passes production evaluation and no
  supported client depends on it.

Test gate:

- Canonical read counts match the final reconciliation exactly.
- Existing and new production accounts pass library, detail, search, collection,
  map, submission and deletion smoke tests.
- Python receives no traffic and has no queued or scheduled work.
- Restore and rollback drills still meet RPO one hour and RTO four hours.

## Cross-task evidence ledger

Keep one pull-request checklist for each task with:

- commit SHA and image digest when applicable;
- local and hosted check links;
- migration version and row-count report;
- fixtures or external smoke inputs used;
- p50, p95, error rate, CPU, memory and provider-call count when applicable;
- security or two-user isolation result;
- rollback command tested and its result;
- remaining known risk with owner and revisit trigger.

Do not call a task complete from a local test alone. Local checks, hosted CI,
development deployment, production rehearsal and production launch are separate
claims with separate evidence.
