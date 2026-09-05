# Architecture questionnaire

This is the complete decision checklist before further backend or web feature
work. `architecture-decisions.md` records confirmed answers and reasons; this
file is only the question bank.

Option A is the recommendation unless the question says otherwise. Answer any
range at once:

```text
DEFAULT Q5-Q20
Q8 C
Q14 B
```

That means accept every recommended answer from Q5 through Q20 except the listed
changes. You may also write a custom answer.

## 1. Product and launch

Q1. First web release: A view library and details; B also submit new links; C
full mobile feature set. **Confirmed: B.**

Q2. Client rollout: A web moves to Go first; B web and Flutter together; C
Flutter first. **Confirmed: A.**

Q3. Launch access: A existing users and new sign-ups; B existing users only; C
invite only. **Confirmed: A.**

Q4. Product URL: A `reelpin.in/library`; B `app.reelpin.in`; C another URL.
**Confirmed: A.**

## 2. Domain language and behavior

Q5. Name one saved record: A library item; B content; C reel.

Q6. Internal model names: A global `content` plus private `user_save`; B one
`library_item` table; C keep `reels` everywhere.

Q7. Public item ID: A user-save ID; B global-content ID; C expose both.

Q8. Same user submits the same source again: A return the existing item or
active job; B create a duplicate; C return conflict.

Q9. Different users submit the same public source: A separate private saves,
shared global processing; B process separately; C share one public record.

Q10. Deleting an item: A remove that user's save and later purge unreferenced
global data; B delete it for everyone; C hide it forever.

Q11. Collections: A one item may belong to many collections; B exactly one; C
no collections.

Q12. User editing in the first release: A no extracted-field editing; B allow
title and category edits; C allow editing every extracted field.

## 3. API contract

Q13. API style: A REST JSON; B GraphQL; C both.

Q14. API versioning: A URL versions such as `/api/v1`; B header versioning; C no
versioning.

Q15. Saved-content route: A `/api/v1/library-items`; B `/api/v1/content`; C
`/api/v1/reels`.

Q16. Pagination: A opaque cursor; B offset; C support both permanently.

Q17. Default and maximum page sizes: A 25 and 100; B 50 and 100; C custom.

Q18. New processing submission: A return `202 Accepted` with a job resource; B
hold the request until processing finishes; C return `200` with an empty item.

Q19. Write idempotency: A require or generate an idempotency key and also enforce
database uniqueness; B database uniqueness only; C client retry creates another
operation.

Q20. Error format: A one JSON envelope with stable code, message, request ID and
retryable flag; B status code only; C endpoint-specific shapes.

Q21. API specification: A Go-owned OpenAPI checked in and used to generate the
web client; B handwritten TypeScript types; C infer types from examples.

## 4. Data model and production migration

Q22. System of record: A PostgreSQL in Supabase; B MongoDB; C split both from the
start.

Q23. Schema placement: A new private `reelpin` schema plus temporary adapters to
legacy `public` tables; B put every new table in `public`; C separate database.

Q24. New primary keys: A UUIDv7 while preserving old UUIDs; B UUIDv4; C integer
sequences.

Q25. User ID type in new tables: A UUID matching `auth.users.id`; B text; C copy
email as identity.

Q26. Global content uniqueness: A platform, content type, source ID and access
scope, with normalized URL fallback; B URL only; C no uniqueness.

Q27. User-save uniqueness: A one active save per user and content; B duplicates
allowed; C one save globally.

Q28. Extracted data storage: A typed columns for queried fields plus versioned
JSONB for the full extraction; B JSONB only; C columns only.

Q29. Content versions: A immutable version rows and a pointer to the current
version; B overwrite one row; C retain only the latest JSON.

Q30. Locations: A normalized rows with PostGIS points; B JSONB inside content; C
both as independent sources of truth.

Q31. Categories and tags: A stable category fields plus flexible tags; B fully
fixed taxonomy; C AI strings only.

Q32. Job model: A private user jobs linked to global processing runs; B one job
per user with repeated processing; C expose global runs directly.

Q33. Database access: A dedicated least-privilege Go role and no browser table
access; B service-role access everywhere; C browser RLS is the primary product
API.

Q34. Production migration: A expand, backfill, verify, switch, then clean up; B
copy everything during downtime; C start production empty.

## 5. Authentication and authorization

Q35. Identity provider: A Supabase Auth for Flutter and web; B build auth in Go;
C separate auth systems.

Q36. Web sign-in methods: A Google plus email password and password recovery for
existing users; B Google only; C magic link only.

Q37. New email accounts: A require email verification; B allow unverified use; C
administrator approval.

Q38. Web session: A `@supabase/ssr`, PKCE and cookies; B browser local storage;
C custom Go sessions.

Q39. Go token verification: A cached asymmetric JWKS locally; B call Supabase
`getUser` on every request; C store sessions in Go.

Q40. Immediate revocation: A local verification normally, Supabase check for
sensitive account actions; B remote check every request; C accept until expiry
for every action.

Q41. Unauthorized record: A return 404 for missing and forbidden; B return 403
for forbidden; C reveal ownership.

Q42. Collection roles: A owner, editor and viewer; B owner and member; C owner
only.

Q43. Administration: A separate admin credentials and audited admin endpoints;
B user JWT with an `admin` body field; C direct database access only.

## 6. Processing, RabbitMQ and providers

Q44. Processing model: A asynchronous jobs; B synchronous HTTP processing; C
client chooses.

Q45. Pipeline stages: A prepare, transcribe, extract, categorize, persist and
index checkpoints; B one large worker function; C one service per stage.

Q46. Delivery guarantee: A at-least-once with idempotent effects; B claim
exactly-once; C at-most-once.

Q47. Database-to-queue consistency: A transactional outbox; B publish after the
transaction without an outbox; C queue first.

Q48. Stage idempotency: A unique run, stage version and input hash; B run ID
only; C rerun every stage after retry.

Q49. Retry policy: A failure classes with bounded exponential backoff and
jitter; B fixed delay forever; C never retry.

Q50. Default attempts: A three attempts with per-provider overrides; B one; C
unlimited.

Q51. Stage timeouts: A explicit timeout per stage and provider; B one timeout
for the whole job; C no worker timeout.

Q52. Job cancellation: A support cancellation before terminal state; B no
cancellation; C kill the worker process.

Q53. Reprocessing: A new processor version creates a new content version; B
overwrite old results; C never reprocess.

Q54. RabbitMQ topology: A topic exchange with queues by workload class and
platform routing keys; B one global queue; C one queue per user.

Q55. RabbitMQ queues: A durable quorum queues with persistent messages; B classic
queues; C transient queues.

Q56. Consumer behavior: A manual acknowledgment after a durable checkpoint and
bounded prefetch; B acknowledge before work; C automatic acknowledgment.

Q57. Dead letters: A dedicated DLQ with an audited replay command; B discard
terminal failures; C retry forever.

## 7. Provider, search, cache and media

Q58. Initial public ingestion platforms: A Instagram, YouTube and generic web
first; B every current Python platform; C Instagram only.

Q59. Provider fallback: A explicit fallback only where quality and cost are
measured; B silently try every provider; C no fallback.

Q60. AI extraction: A Gemini structured output decoded into typed Go structs and
validated; B accept arbitrary JSON; C parse text with regular expressions.

Q61. Transcription: A platform transcript first, then bounded media
transcription; B always download media; C transcript only.

Q62. Provider protection: A timeouts, concurrency limits, cooldowns and circuit
breakers; B rate limits only; C no shared protection.

Q63. Search launch: A ship hybrid search only after a labeled real-embedding
evaluation passes; B ship dense search immediately; C keyword search only.

Q64. Search design: A pgvector plus full-text plus trigram fused with RRF; B
pgvector only; C external search service.

Q65. Search document: A title, summary, facts, tags and bounded transcript chunks;
B full raw transcript only; C title only.

Q66. Redis role: A disposable cache, rate limits and ephemeral worker health; B
product source of truth; C do not use Redis.

Q67. Rate-limit failure: A fail closed for provider-costing writes, keep safe
reads available; B fail open everywhere; C fail closed everywhere.

Q68. Media retention: A delete temporary downloads after processing and retain
only required thumbnails/artifacts; B retain every source file; C store nothing.

## 8. Web architecture and experience

Q69. Web repository: A extend `reelpin-web`; B create another repository; C add
web files to the Go repository.

Q70. Data path: A browser talks to Next.js, which forwards the user token to Go;
B browser calls Go directly; C browser queries Supabase tables directly.

Q71. Rendering: A server-render initial authenticated pages, client components
for interaction; B client-side SPA only; C static generation.

Q72. Authenticated caching: A private no-store responses initially; B ISR; C
shared CDN cache.

Q73. Library default view: A responsive grid with an optional compact list; B
list only; C grid only.

Q74. Processing updates: A bounded polling only while jobs are active; B
WebSockets from the first release; C manual refresh.

Q75. First-release filters: A platform, category, subcategory, date and sort; B
platform only; C no filters.

Q76. First-release routes: A login, library, item detail and submit; B also map,
search and collections; C dashboard only.

## 9. Infrastructure, deployment and reliability

Q77. Local development: A Docker Compose for PostgreSQL extensions, Redis and
RabbitMQ; B install services directly; C use production resources locally.

Q78. Environment isolation: A separate Supabase, Redis and RabbitMQ resources;
B shared resources with row flags; C one environment. **Requirement already
confirmed as A.**

Q79. Development host: A current `reelpin-ec2-dev`; B local only; C new cloud
host. **Current direction: A.**

Q80. Production compute: A small Tokyo EC2 deployment first, with a measured
scale-out trigger; B ECS/Fargate now; C keep compute in Mumbai.

Q81. Production RabbitMQ: A managed RabbitMQ; B self-hosted container on the API
host; C Amazon SQS instead.

Q82. Production Redis: A managed Redis; B self-hosted beside the API; C remove
Redis.

Q83. Container registry: A GHCR images pinned by digest; B Docker Hub tags; C
copy binaries with SSH.

Q84. Development deployment: A automatic after `dev` checks pass; B manual; C
deploy every feature branch.

Q85. Production deployment: A manually promote the exact tested development
digest; B rebuild from `main`; C deploy mutable `latest`.

Q86. Migrations: A pre-deploy command with an advisory lock and forward fixes; B
run from every API replica; C manual SQL in the dashboard.

Q87. Availability target at launch: A one API and worker instance with tested
restart and restore, then add replicas from measurements; B multi-region active
active; C no stated target.

Q88. Recovery targets: A daily backups plus point-in-time recovery where the
Supabase plan supports it, with documented RPO and RTO; B backups without a
restore test; C no separate recovery plan.

## 10. Security, observability, testing and delivery

Q89. User-supplied URLs: A HTTP/S allowlist rules, DNS and redirect SSRF checks,
size limits and MIME signature validation; B URL syntax only; C worker fetches
anything.

Q90. Web mutation protection: A same-site cookies, origin checks and CSRF-safe
server actions; B CORS only; C browser service key.

Q91. Browser security headers: A CSP, HSTS, frame restrictions, MIME sniffing
protection and referrer policy; B framework defaults only; C none.

Q92. Logging: A structured logs with request IDs, route patterns and hashed user
IDs; B raw request data; C text logs.

Q93. Metrics and alerts: A API, queue, worker, provider and database signals with
tested thresholds; B host CPU only; C logs only.

Q94. Tracing: A add OpenTelemetry before production cutover; B add after a
measured debugging need; C never trace. Recommendation: B for this team.

Q95. Test gate: A format, vet, unit, race, integration, contract, migration and
container checks; B unit tests only; C manual testing.

Q96. Browser gate: A Playwright login, submit, processing, library, detail and
two-user isolation tests; B screenshots only; C manual browser checks.

Q97. Load testing: A measure expected traffic plus a safety margin before
production; B wait for an incident; C synthetic benchmark only.

Q98. Pull-request policy: A focused PRs, required green checks and one human
review for production-impacting work; B merge any green PR; C large phase PRs.

Q99. Initial scale assumption: A fewer than 10,000 users and 1,000 submissions
per day; B 10,000 to 100,000 users; C custom numbers.

Q100. Monthly infrastructure and provider budget: A define a hard amount before
write launch; B monitor without a limit; C unlimited. Recommendation: A, amount
must be supplied.

## Answer status

- Confirmed: Q1 through Q4.
- Answered and recorded: Q8-Q14, Q17, Q19-Q28 except Q27's option label was
  corrected to A, Q30, Q32-Q36, Q38-Q39, Q41-Q49, Q51-Q52, Q56-Q61, Q63-Q79,
  Q81-Q99.
- Needs clarification: Q5-Q7, Q15-Q16, Q18, Q29, Q31, Q37, Q40, Q50,
  Q53-Q55, Q62, Q66, Q80, Q88 and Q100.

## 11. Clarifications from the first complete answer batch

Q101. Naming across layers: A UI and API say `library item`, database uses
`contents` and `user_saves`; B Flutter and API keep `reel`, database uses
`contents` and `user_saves`; C use `reel` everywhere. Recommendation: B if
minimizing Flutter changes is the deciding condition; A if clean domain language
is more important. Never use C because a place or generic URL is not a reel.

Q102. Public ID after introducing `user_saves`: A expose `user_saves.id` and keep
`contents.id` internal; B expose `contents.id` and resolve the current user's save
on every operation; C expose both. Recommendation: A. Existing production reel
IDs become user-save IDs, collection membership stays user-specific, and global
content identity cannot leak through the public contract.

Q103. Pagination: A cursor only; B offset only; C cursor and offset permanently.
Recommendation: A. Cursor pagination remains stable while new items arrive;
supporting both doubles contract and test behavior. Flutter can be changed once.

Q104. Submission response and loading card: A return `202 Accepted` with a job;
the web and Flutter render that job as a loading card; B return `200` with an
incomplete content object; C block until complete. Recommendation: A. The loading
card is a UI decision and does not require pretending unfinished content exists.

Q105. Versions and reprocessing: A keep immutable `content_versions`; reprocess
only deliberately when prompt, schema or model version changes; B overwrite one
result row; C never reprocess for any reason. Recommendation: A. A failed new
version leaves the previous version live and makes AI changes auditable.

Q106. Category evolution: A inject active global categories into the prompt;
Gemini selects one or returns `Other` plus a proposed category, which stays
pending until a human approves it; B immediately make every AI suggestion a
global category; C let each user have an automatically evolving taxonomy.
Recommendation: A. Tags remain flexible multi-value descriptors. Example:
category `Food`, subcategory `Recipes`, tags `vegan`, `high-protein`, `20-minute`.

Q107. New email registration: A require email verification before product use; B
allow unverified accounts to submit provider-costing work; C allow browsing but
block submission until verification. Recommendation: A. It gives reliable
ownership, recovery and an abuse boundary.

Q108. Sensitive-action revocation: A verify JWTs locally normally, but ask
Supabase to validate the user for account deletion, credential changes and admin
operations; B trust local JWT validity until expiry for every action; C call
Supabase on every request. Recommendation: A.

Q109. Three attempts means: A at most three executions of the failed stage;
completed checkpoints are reused; B three complete pipeline executions; C three
HTTP attempts inside every provider call plus three pipeline attempts.
Recommendation: A. It bounds cost without repeating successful stages.

Q110. Low-cost worker topology: A one worker process with two consumers and total
concurrency two: one media queue and one light-web queue; routing keys select a
platform handler inside the process; B one always-running process per platform;
C one queue and one serial consumer for everything. Recommendation: A.

The media queue binds Instagram, YouTube, TikTok and Pinterest. The light-web
queue binds X, LinkedIn, Reddit, place and generic-web jobs. A shared limit keeps
only two jobs active, and the media limit stays one. More processes can run the
same binary later without changing messages or handlers.

Q111. Single-node RabbitMQ queue type: A durable classic queues, durable exchange,
persistent messages, publisher confirms, manual acknowledgements and a persistent
volume; B quorum queues on one node; C transient queues. Recommendation: A.
Quorum queues become the choice only with a multi-node RabbitMQ cluster.

Q112. Provider protection: A rate limits, timeouts, per-provider concurrency caps
and cooldown after 429 or credential failures, without a generic circuit-breaker
framework initially; B rate limits and timeouts only; C add a full circuit breaker
for every provider now. Recommendation: A. A rate limit controls request volume;
it does not stop simultaneous downloads or repeated calls during an outage.

Q113. Redis response cache at launch: A no product response cache initially;
Redis still owns rate limits, provider cooldowns and worker heartbeats; add
cache-aside only after metrics show a slow repeated read; B cache filter facets
for 60 seconds and invalidate after a save; C cache library pages and details.
Recommendation: A at fewer than 1,000 users. If B is selected, PostgreSQL remains
the truth, every entry has a TTL, and writes delete the affected cache key.

Q114. Production compute: A defer the exact instance until load and memory tests,
but require it in `ap-northeast-1` before production; B select one Tokyo EC2
instance now; C use ECS/Fargate now. Recommendation: A. This defers capacity, not
architecture, and blocks only production deployment.

Q115. Recovery objectives: A RPO one hour and RTO four hours; B RPO 24 hours and
RTO eight hours; C provide custom values. Recommendation: A. RPO is the maximum
acceptable data loss; RTO is the maximum acceptable restoration time.

Q116. Monthly cost guardrail: A alert at INR 5,000 and stop new provider-costing
submissions at INR 10,000 while reads continue; B monitor cost without a hard
limit; C provide custom alert and hard-limit amounts. Recommendation: A until real
cost per processed item is measured.

After Q101 through Q116 are resolved, one final value questionnaire will set
exact timeouts, retention periods, rate limits, queue names, health windows and
release thresholds. Only then is the implementation plan written.
