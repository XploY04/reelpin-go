# Architecture decision register

**Status:** decisions complete; one launch value deferred

This file is the gate before further backend or web implementation. A decision
is confirmed only after the product owner answers it explicitly. Each confirmed
decision records the choice, reason, rejected alternatives, consequences and a
condition for reconsidering it.

## Confirmed requirements

| ID | Requirement |
|----|-------------|
| R1 | ReelPin Go is a production backend built with a clean design, not a line-by-line Python translation. |
| R2 | Flutter and web may change to follow the final Go API contract. |
| R3 | The current Supabase project is production; `reelpin-go` is development. The two environments use separate infrastructure. |
| R4 | Existing production users keep their Supabase identities and must see all previously saved content on web. |
| R5 | The existing backend migration roadmap remains; authenticated web delivery is added to it. |
| R6 | The web product must let a signed-in user view saved content. |
| R7 | There is no paid plan or entitlement system in the current product. |
| R8 | RabbitMQ is the selected processing queue. |
| R9 | The two-person team does not maintain a permanent staging environment. |
| R10 | Decisions must be simple to explain and defend in an interview. |

## Decision areas

| ID | Area | What must be decided | State |
|----|------|----------------------|-------|
| A | Product boundary | Launch users, web MVP features, mobile scope and administration | Confirmed |
| B | Domain language | Names for global content, user saves, jobs, collections and public routes | Confirmed |
| C | API contract | REST shape, versioning, pagination, errors, idempotency and compatibility policy | Confirmed |
| D | Data model | Tables, keys, relationships, JSONB boundaries, constraints and indexes | Confirmed |
| E | Production migration | Baseline schema, backfill, reconciliation, dual operation, cutover and cleanup | Confirmed |
| F | Authentication | Supabase flows, sessions, JWT verification, revocation and account lifecycle | Confirmed |
| G | Authorization | Ownership, collection roles, admin access and record-not-found behavior | Confirmed |
| H | Processing | Pipeline stages, retries, leases, timeouts, idempotency and failure classes | Confirmed |
| I | Queue | RabbitMQ topology, routing, acknowledgements, prefetch, retry and dead letters | Confirmed |
| J | Providers | Gemini, Apify, Google, Firebase, download tools, quotas and fallback behavior | Confirmed |
| K | Search | Documents, embeddings, lexical search, filters, ranking, evaluation and freshness | Confirmed with evaluation gate open |
| L | Cache and limits | Redis responsibilities, cache invalidation, rate limits and abuse controls | Confirmed |
| M | Media and storage | Temporary media, thumbnails, Supabase Storage, retention and size limits | Confirmed |
| N | Web architecture | Repository, routes, SSR, browser/server split, API client and state handling | Confirmed |
| O | Web experience | Navigation, library views, detail, filters, search, map and accessibility | Confirmed |
| P | Environments | Local, dev and production resources, secrets, DNS and region placement | Confirmed |
| Q | Deployment | Containers, image promotion, migrations, traffic switching and rollback | Confirmed |
| R | Reliability | SLOs, timeouts, graceful shutdown, degradation and disaster recovery | Confirmed |
| S | Observability | Logs, metrics, traces, dashboards, alerts and audit events | Confirmed |
| T | Security and privacy | SSRF, validation, CORS, CSRF, headers, encryption, deletion and retention | Confirmed |
| U | Testing | Unit, integration, contract, end-to-end, load, migration and production smoke tests | Confirmed |
| V | Delivery workflow | Branches, PR size, CI gates, ownership, docs and release approvals | Confirmed |
| W | Scale and cost | Expected load, concurrency, provider budgets, database size and scaling triggers | Confirmed except launch cost amount |

## Accepted architecture

These recommendations are accepted. The implementation plan turns them into
bounded tasks and test gates.

| ID | Proposal |
|----|----------|
| P1 | Use PostgreSQL in Supabase as the system of record. |
| P2 | Use one Go modular monolith with API, worker and maintenance binaries. |
| P3 | Use Supabase Auth for Flutter and web; verify access tokens locally in Go with JWKS. |
| P4 | Use `content` for a global source and `user_save` for ownership; keep `reel` in the public API and Flutter. |
| P5 | Let web and Flutter access product data through Go rather than direct Supabase table queries. |
| P6 | Extend `reelpin-web`, keeping marketing at `/` and placing the product at `/library`. |
| P7 | Use `@supabase/ssr`, PKCE and cookie sessions in Next.js. |
| P8 | Use a transactional outbox and idempotent stages for at-least-once RabbitMQ delivery. |
| P9 | Use Redis only for disposable cache, rate-limit and ephemeral worker state. |
| P10 | Use pgvector, PostgreSQL full-text search and trigram matching with RRF fusion. |
| P11 | Preserve production IDs through expand, backfill, verify and switch migrations. |
| P12 | Run production Go compute on the provided Mumbai EC2 and measure its latency to Tokyo Supabase. |

## Decision sequence

Questions are answered in this order because later choices depend on earlier
ones:

1. Product boundary and launch sequence.
2. Domain language and API contract.
3. Data model and production migration.
4. Authentication and authorization.
5. Processing, RabbitMQ and providers.
6. Search, cache, rate limits and media.
7. Web architecture and user experience.
8. Infrastructure, deployment and reliability.
9. Observability, security, testing and delivery workflow.
10. Scale, cost and disaster recovery.

## Confirmed decisions

### D1: First web release includes link submission

**Choice:** Show the user's library and item details, and accept new links for
processing.

**Reason:** The web release must demonstrate the full product loop, not only an
archive viewer.

**Rejected:** A read-only first release and immediate full mobile feature parity.

**Consequence:** Public web launch waits for enqueue, RabbitMQ, a worker,
processing-job polling and at least one production-quality ingestion path.

**Revisit when:** The worker launch is delayed enough that a read-only private
preview would provide useful validation.

### D2: Move web to Go before Flutter

**Choice:** Production web uses Go before the production Flutter app does.

**Reason:** A Vercel deployment can be rolled back immediately and does not wait
for app-store review or installed mobile clients to update.

**Rejected:** Switching both clients together and switching Flutter first.

**Consequence:** Go must preserve production data while Python continues serving
Flutter during the web observation period.

**Revisit when:** Web and mobile deployments become operationally coupled.

### D3: Open web to existing users and new sign-ups

**Choice:** Every existing production user may sign in, and new users may create
accounts.

**Reason:** Web and mobile are two clients of one product identity.

**Rejected:** Invite-only access and blocking new registrations.

**Consequence:** Sign-up abuse controls, onboarding and an empty-library
experience are launch requirements. Email verification is not required, so IP
and submission controls carry more responsibility.

**Revisit when:** Abuse or provider cost requires a temporary registration gate.

### D4: Serve the product at reelpin.in/library

**Choice:** Keep marketing at `reelpin.in/` and serve the signed-in product at
`reelpin.in/library`.

**Reason:** One Next.js deployment and cookie domain are simpler than a separate
application subdomain.

**Rejected:** `app.reelpin.in` and a new product domain.

**Consequence:** Marketing, authentication and product routes need separate
layouts inside the same repository without sharing authenticated response
caches.

**Revisit when:** Product and marketing need independent teams, release cadence
or infrastructure.

## Question status

The complete numbered questionnaire has been resolved. Its former file now
records only the deferred launch gate. No question blocks planning or
development.

## Answer batch: Q5 through Q100

### Confirmed without clarification

| Questions | Answer |
|-----------|--------|
| Q8-Q14 | A |
| Q17 | A |
| Q19-Q23 | A |
| Q24 | B, use UUIDv4 for new records |
| Q25-Q26 | A |
| Q28 | A |
| Q30 | A |
| Q32-Q36 | A |
| Q38-Q39 | A |
| Q41-Q42 | A |
| Q43 | C, no product admin API; privileged work uses direct database access or maintenance commands |
| Q44-Q49 | A |
| Q51 | A |
| Q52 | B, no job cancellation in the initial system |
| Q56-Q57 | A |
| Q58 | B, migrate every currently supported Python platform |
| Q59 | Preserve each tested Python provider fallback, then add explicit timeouts, failure classes and cost tests |
| Q60-Q61 | A |
| Q63-Q64 | A; search remains blocked until the real-embedding evaluation passes |
| Q65 | Embed title, summary, tags and facts; do not embed transcript chunks initially |
| Q67-Q79 | A |
| Q81-Q82 | B, self-host RabbitMQ and Redis at initial production scale |
| Q83-Q98 | A |
| Q99 | Fewer than 1,000 users and 100 submissions per day |

### Confirmed interpretations

- Q10: deleting removes only the user's save. Global processed content remains
  reusable even with no current saves, subject to a later retention decision.
- Q27: the described design is option A, not C. `user_saves` references both
  `auth.users.id` and `contents.id`; `UNIQUE (user_id, content_id)` creates one
  private save per user for one global content row.
- Q50: maximum three executions applies to each failed stage. Completed stages
  are reused and are not executed again.
- Q55: a queue is still required. On one RabbitMQ node, use durable classic
  queues, persistent messages, publisher confirms and persistent disk. Quorum
  queues add useful replication only after multiple nodes exist.

### Pending clarification

- No question blocks planning or development.
- Cost controls remain deliberately deferred and block public write launch.

## Clarification batch: Q101 through Q116

### Confirmed

- Q101 B: Flutter and the public API keep the word `reel`; the database uses
  global `contents` and private `user_saves`. Generic content types remain valid
  even though the client-facing historical name is narrower.
- Q102 A: expose `user_saves.id`; keep `contents.id` internal. Existing
  production reel IDs become user-save IDs.
- Q103 A: cursor pagination only in the final contract.
- Q104 A: a new submission returns `202 Accepted` with a job. The clients render
  the job as a loading card.
- Q105 A: content versions are immutable. Reprocessing is deliberate and happens
  only for a new prompt, schema or model version; there is no periodic content
  reprocessing.
- Q106 custom: active global categories are injected into the prompt. Gemini may
  propose a new category. A weekly scheduled curator uses a lightweight Gemini
  model and structured output to approve, merge or reject proposals. The
  inferred value batch below sets its thresholds, limits and rollback rules.
- Q107 B: new email accounts may use the product without verifying email.
  Stronger sign-up, IP and submission abuse controls are therefore required.
- Q108 A: normal requests use local JWT verification; sensitive identity and
  account actions also validate with Supabase.
- Q109 A: a failed stage may execute at most three times. Completed checkpoints
  are not repeated.
- Q111 A: initial RabbitMQ uses durable classic queues on one node, persistent
  messages, publisher confirms, manual acknowledgements and persistent disk.
- Q112 A: providers have timeouts, concurrency caps and cooldowns in addition to
  request rate limits. A generic circuit-breaker framework is deferred.
- Q113 A: no product-response cache at launch. Redis initially owns rate limits,
  provider cooldowns and worker heartbeats.
- Q114 custom: production uses the provided Mumbai EC2 host. The database remains
  in Tokyo, so cross-region database latency is accepted and must be measured in
  the production rehearsal.
- Q115 A: recovery objective is RPO one hour and RTO four hours.
- Q116 deferred: there is no cost alert or hard stop yet. This must be decided
  before public provider-costing submissions are enabled.

### Q110 confirmed worker design

The Python development worker has one process and two effective Dramatiq worker
threads. It consumes `processing_jobs` and `notifications` from Redis. All
processing platforms share one queue. Effective platform limits are Instagram
2, YouTube 1, TikTok 1 and web 1. A blocked platform job is delayed 15 seconds
and sent back to the same queue. Two long media jobs can therefore occupy both
threads and make a short HTML job wait.

The EC2 development host is a `t3.micro` with two vCPUs, 911 MB RAM and about
306 MB currently available. Two concurrent heavy media jobs are not safe there.

Confirmed Go design:

- One worker binary and one OS process initially. Concurrency uses goroutines,
  not a process per platform.
- A topic exchange receives deterministic routing keys.
- `processing.media` handles work requiring download, transcription or vision.
- `processing.light` handles HTML, oEmbed, text APIs and place metadata.
- The URL/source resolver selects the route without AI. Unknown generic URLs go
  to light inspection first; if media is discovered, a database transaction and
  outbox event move the run to the media queue before the light message is
  acknowledged.
- Development runs one media consumer and one light consumer. This reserves one
  slot for short work and fits the current host better than two media consumers.
- Production may run two media consumers and one light consumer only after the
  provided Mumbai host passes CPU, memory and processing-time load tests.
- Platform handlers remain inside the worker registry. Adding a platform does
  not create another always-running process.
- Queue prefetch equals the consumer count. A database lease and checkpoint make
  redelivery safe.

Gemini is not used for queue routing. The current GA Flash-Lite model is useful
for the weekly taxonomy curator, where an occasional failure can wait until the
next scheduled run.

## Inferred implementation values

The product owner asked to accept the engineering recommendation wherever prior
answers already determine the result. These values are now confirmed:

- Submission accepts `url` and optional `collection_ids`; native share text is
  resolved and enqueued atomically by a share-token endpoint.
- Go owns `/api/v2`. Python keeps serving installed `/api/v1` mobile clients
  through the measured support window because the new cursor, error and job
  contracts are breaking changes.
- Only completed saves appear as reels. Active jobs are separate loading cards.
- Cursors order by `(saved_at DESC, user_save_id DESC)` and are opaque.
- Idempotency request records live 24 hours; content uniqueness is permanent.
- Clients poll after 2 seconds, back off to 10 seconds, and stop on a terminal
  state or after 30 minutes.
- HTTP limits remain 5s header, 15s read, 15s write, 60s idle and 1 MiB headers.
- Each API instance uses a PostgreSQL pool with minimum 2, maximum 10,
  30-minute connection lifetime and 5-minute idle lifetime.
- Normal database queries time out after 5 seconds; readiness after 2 seconds.
- Submission limits are 5 per user per hour, 20 per IP per hour and 2 active
  jobs per user. Search limits are 30 per user and 90 per IP per minute.
- Unverified-email abuse uses IP/device limits and CAPTCHA only after suspicious
  behavior, not on every request.
- URLs are at most 2,048 characters, HTTP/S only, with at most 5 redirects and
  DNS safety checked before each connection.
- Fetch limits are HTML 5 MiB, images 20 MiB each, media 500 MiB total and video
  duration 30 minutes.
- Temporary storage is 1 GiB per job. Media admission stops at 80% disk and all
  temporary files are removed after every terminal outcome.
- Unreferenced public global content and all immutable content versions are
  retained for deduplication. Privacy, legal, private-source and blocklist purge
  paths override retention.
- Taxonomy has one global category, optional subcategory and many tags. The
  curator runs Sunday at 02:00 UTC, requires 3 distinct-content proposals and
  confidence at least 0.90, adds at most 5 categories, and records rollback data.
- The weekly taxonomy curator uses stable model `gemini-3.5-flash-lite`.
- Search starts its measured evaluation with stable model
  `gemini-embedding-2` at 768 output dimensions. It becomes active only if the
  recorded quality gate passes.
- RabbitMQ uses exchange `reelpin.processing`, durable queues
  `reelpin.processing.media`, `reelpin.processing.light`, retry queues, a
  notifications queue and class-specific dead-letter queues for poison
  messages. Expected terminal business failures are acknowledged after their
  database state is committed.
- Failed-stage retries wait 30 seconds and then 5 minutes; the third failed
  execution is terminal.
- Stage timeouts are prepare 30s, download 180s, transcribe 300s, extract 90s,
  categorize 45s, persist 30s and index 60s. Whole-run timeout is 30 minutes.
- Workers heartbeat every 15 seconds and become stale after 90 seconds.
- Development concurrency is media 1 and light 1. Gemini permits 2 concurrent
  calls, each Apify actor/account 1, and light HTTP 4 within those worker limits.
- Notifications use a separate durable queue and one light consumer in the same
  worker binary.
- Search launch requires at least 50 real labeled queries, Recall@10 at least
  0.85, nDCG@10 at least 0.75, explicit zero-result tests and p95 below 1.5s.
  Completed saves become searchable within 60 seconds.
- API launch target is 99.5% monthly availability and read p95 below 800ms.
  Processing SLOs are set only after every platform is measured.
- Alerts fire for 5xx above 2% for 5 minutes, oldest ready job above 10 minutes,
  any dead letter, worker stale at 90 seconds, disk above 80% and 5 provider
  failures in the alert window.
- Send OpenTelemetry signals to Grafana Cloud Free rather than self-hosting an
  observability database on EC2. Raw metrics, logs and traces use its 14-day
  retention. Store small daily SLO summaries for 90 days. Trace sampling is 10%
  normally and 100% for errors, with sensitive fields removed.
- Backups/PITR must support RPO one hour and RTO four hours. A restore drill runs
  monthly.
- Production migration proof includes exact counts, every failed row, duplicate
  and foreign-key reports, and 100 deterministic old-record comparisons through
  Python and Go.
- Account deletion removes user saves, collections, tokens and profile
  immediately. It retains globally reusable public content without a user link.
- Browsers call Next.js, so Go exposes no browser CORS allowlist initially.
- Application secrets rotate every 90 days and immediately after suspected
  exposure.
- `dev` deploys automatically after checks. Production manually promotes the
  exact tested digest.
- Production launch runs internal smoke tests, automatic rollback thresholds
  and a 24-hour close-watch period before normal operation.
- Mumbai compute is reconsidered if read p95 exceeds 800ms or database network
  time exceeds 150ms for 15 minutes.
- RabbitMQ uses persistent disk, but PostgreSQL run/outbox state is the disaster
  recovery source. Redis is disposable. A new broker replays unfinished work
  instead of restoring a live broker filesystem snapshot.
- Global content supports blocklist and maintenance purge by source identity,
  including derived text, embeddings and media.
- Unknown or authenticated sources start in a user-specific access scope. They
  become globally reusable only after a worker proves the source is public
  without user credentials.
- Support the current and previous two Flutter releases. During cutover, keep
  Python writes for at least 90 days and until older clients are below 1% of
  active devices for 30 consecutive days, whichever is later.
- Development proceeds without a cost budget. Public link submission remains
  disabled until a measured alert and hard limit replace the deferred choice.

## Decision log

| Date | Decisions |
|------|-----------|
| 2026-09-05 | D1 through D4 confirmed by the product owner. |
| 2026-09-05 | Q5 through Q100 answered; unambiguous choices recorded and fourteen clarification groups opened. |
| 2026-09-06 | Q101-Q115 confirmed, including the media/light worker design. Remaining engineering defaults were inferred from accepted recommendations. Only the cost amount is deferred until before public writes. |
