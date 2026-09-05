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

## Clarification status

- Confirmed: Q101-Q109 and Q111-Q115.
- Under discussion: Q110. The evidence-backed recommendation is one Go process,
  one media consumer and one reserved light consumer in development, with
  deterministic routing and durable escalation from light to media.
- Deferred but still blocking public writes: Q116.

## 12. Final implementation values

This is the last question batch before the detailed implementation plan. Option
A is the recommendation unless stated otherwise.

Q117. Resolve Q110 worker allocation: A deterministic media/light routing, one
media plus one light consumer on dev, production may use two media plus one light
only after its load test; B use Gemini to classify every job and always run two
media plus one light; C keep Python's one shared queue. Recommendation: A.

Q118. Submission request: A `{url, collection_ids?}` for web plus a separate
native share-resolution endpoint; B accept arbitrary provider payloads in one
body; C URL only forever.

Q119. Library contents: A only completed saves appear in the library and active
jobs appear as separate loading cards; B store incomplete rows in the library;
C hide all processing state.

Q120. Cursor order: A newest first by `(saved_at, user_save_id)` with an opaque
signed or encoded cursor; B ID only; C client timestamp.

Q121. Idempotency-key lifetime: A retain request keys for 24 hours while content
uniqueness remains permanent; B seven days; C no expiry.

Q122. Job polling: A 2 seconds initially, back off to 10 seconds, stop at a
terminal state or after 30 minutes; B fixed 1 second; C fixed 15 seconds.

Q123. HTTP server limits: A 5-second header, 15-second read, 15-second write,
60-second idle and 1 MiB headers; B custom values; C framework defaults.

Q124. Database pool per API instance: A maximum 10, minimum 2, 30-minute
connection lifetime and 5-minute idle time; B maximum 25; C driver defaults.

Q125. Database query timeout: A 5 seconds normally and 2 seconds for readiness;
B 15 seconds; C no query timeout.

Q126. Submission limits before a cost budget exists: A 5 per user per hour, 20
per IP per hour and 2 active jobs per user; B current Python limits of 10, 30 and
4; C custom values.

Q127. Search limits: A 30 requests per user per minute and 90 per IP; B 10 and
30; C custom values.

Q128. Unverified email abuse: A CAPTCHA after suspicious behavior plus IP/device
limits; B CAPTCHA on every sign-up and submission; C no CAPTCHA. Recommendation:
A. Google and email sign-in remain available.

Q129. URL input: A maximum 2,048 characters, HTTP/S only, maximum 5 redirects,
and DNS safety checked before every connection; B 10,000 characters and 10
redirects; C custom values.

Q130. Fetch limits: A HTML 5 MiB, images 20 MiB each, total media 500 MiB and
video duration 30 minutes; B smaller limits; C custom values.

Q131. Temporary storage: A 1 GiB per job, stop accepting media work at 80% disk,
and delete temporary files after every terminal outcome; B 2 GiB and 90%; C
custom values.

Q132. Unreferenced global content: A retain indefinitely for deduplication unless
the source is private, deleted, legally removed or a user deletion requires
purge; B purge after 90 days; C purge immediately. Your earlier answer implies A.

Q133. Content-version retention: A retain every immutable version; B retain the
current and previous two; C current only.

Q134. Weekly category curator: A Sunday 02:00 UTC, require proposals from 3
distinct contents, confidence at least 0.90, add at most 5 categories per run,
and retain an audit/rollback record; B approve every proposal; C custom values.

Q135. Taxonomy shape: A one global category, optional subcategory and many tags;
B categories only; C unlimited category depth.

Q136. RabbitMQ names: A exchange `reelpin.processing`, queues
`reelpin.processing.media`, `reelpin.processing.light`, retry queues and one
dead-letter queue; B shorter names; C custom names.

Q137. Retry delays for three failed-stage executions: A 30 seconds then 5
minutes; the third failure becomes terminal; B 15 seconds then 5 minutes; C
custom values.

Q138. Stage timeouts: A prepare 30s, download 180s, transcribe 300s, extract 90s,
categorize 45s, persist 30s and index 60s; B use one 30-minute timeout; C custom.

Q139. Whole-run safety timeout: A 30 minutes even when stage limits would permit
more; B 60 minutes; C no whole-run limit.

Q140. Worker health: A heartbeat every 15 seconds and stale after 90 seconds; B
30 and 120; C custom.

Q141. Provider concurrency inside the dev worker: A media total 1, Gemini 2,
Apify actor/account 1 and light HTTP 4, all still bounded by the media/light
consumer counts; B one for every provider; C custom values.

Q142. Notifications: A separate durable queue consumed by the same worker binary
with one lightweight consumer; B share the light-processing queue; C defer all
notifications.

Q143. Search evaluation gate: A at least 50 labeled real queries, Recall@10 at
least 0.85, nDCG@10 at least 0.75, zero-result cases tested and p95 below 1.5s;
B quality review only; C custom thresholds.

Q144. Search freshness: A searchable within 60 seconds after a save completes;
B 5 minutes; C only after a nightly index.

Q145. API launch SLO: A 99.5% monthly availability and p95 under 800ms for read
routes, excluding provider-backed processing; B 99.9% and 500ms; C custom.

Q146. Processing SLO: A p95 light jobs under 30 seconds and media jobs under 10
minutes; B 60 seconds and 5 minutes; C measure first and set before production.
Recommendation: C because every platform is launching and real timings are not
known yet.

Q147. Alert thresholds: A 5xx above 2% for 5 minutes, oldest ready job above 10
minutes, any dead-letter, worker stale 90 seconds, disk above 80% and provider
failure burst of 5; B custom; C alerts after launch.

Q148. Observability retention: A logs 30 days, metrics 90 days, traces 7 days;
B 7, 30 and 3 days; C custom.

Q149. Trace sampling: A 10% of normal requests and 100% of errors, with no raw
tokens, URLs or user IDs; B trace every request; C custom.

Q150. Backup verification: A automated daily backups/PITR supporting the one-hour
RPO, plus a restore drill every month; B quarterly restore drill; C custom.

Q151. Production migration proof: A exact table counts, all failed rows listed,
duplicate report, foreign-key checks and a deterministic sample of 100 old
records compared through Python and Go; B counts only; C custom.

Q152. Account deletion: A immediately remove user saves, collections, tokens and
profile; retain only globally reusable public content with no user link; B also
purge all global content once unreferenced; C custom.

Q153. API browser access: A web browser calls Next.js only, so Go has no browser
CORS allowlist initially; Flutter calls Go as a native client; B allow direct
browser calls from `reelpin.in`; C allow all origins.

Q154. Secret rotation: A rotate application secrets every 90 days and immediately
after suspected exposure; B yearly; C only after exposure.

Q155. Development deployment: A automatically deploy `dev` after all checks;
production remains a manual digest promotion; B both manual; C both automatic.

Q156. Production rollout: A internal smoke test, then web launch to all users
with automatic rollback thresholds and a 24-hour close-watch period; B selected
users first; C immediate launch without observation.

Q157. Production database latency trigger: A move compute near Supabase if read
p95 exceeds 800ms or database network time exceeds 150ms for 15 minutes; B accept
any Mumbai-to-Tokyo latency; C custom.

Q158. Self-hosted RabbitMQ/Redis backup: A persist RabbitMQ disk, snapshot it
daily, and accept Redis loss because its state is disposable; B back up both; C
back up neither.

Q159. Global-content legal/privacy purge: A support a blocklist and maintenance
purge keyed by source identity, removing derived text, embeddings and media; B
manual SQL only; C never purge global content.

Q160. Deferred cost decision gate: A allow development testing without a budget,
but block public link submission until Q116 is replaced with an alert and hard
limit; B launch publicly without a budget; C block all further work.

Answer with `DEFAULT Q117-Q160` and list exceptions. Q146 defaults to C; all
other questions default to A.
