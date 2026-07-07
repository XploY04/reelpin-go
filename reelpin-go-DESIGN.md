# ReelPin Go Backend - Design Doc

Status: draft for review. Open decisions are marked **[DECIDE]**.

## Goal

Rebuild the ReelPin backend in Go as a production-shaped async media-processing
and search platform. Not a line-by-line port. The point is to learn and to be
able to defend the system in interviews: APIs, durable queues, workers, retries,
hybrid search, caching, concurrency, observability, and failure handling.

Two non-negotiables that the rewrite must get right because the current Python
service does not:

1. **Real embeddings.** Today's "semantic search" is fake. `embedder.py` builds a
   384-dim vector from a token counter plus SHA256 word hashing. Pinecone is doing
   keyword overlap, not meaning. The Go build uses a real embedding model so the
   "semantic search" claim becomes true.
2. **One platform, one queue.** The system is Instagram-only. The Python codebase
   carries dead TikTok/YouTube routing, ~24 unused cookie env vars, per-platform
   concurrency, and a `groq_whisper` reference to a service that does not exist.
   None of that gets ported.

## What we keep from the Python design

The current job model is well built. Port the ideas, not the code:

- Postgres `processing_jobs` table is the source of truth for job state.
- Worker claims rows atomically (`UPDATE ... WHERE claimed_by IS NULL`), heartbeats,
  and a maintenance loop recovers stale claims.
- A processing-cache table keyed by `(source_platform, source_content_id)` short-circuits
  download/transcribe/extract on duplicate content.
- Failure classification to a `FailureCode` enum, then a retry policy that decides
  retry vs dead-letter vs terminal based on code and attempt count.
- Source-identity normalization: every incoming URL is normalized before any DB
  lookup, and dedup checks both URL and source-identity because the same reel is
  shared under many URL shapes.

## Core principle

```
Postgres owns state.
RabbitMQ moves work.
Redis stores fast temporary things.
Workers do long-running processing.
Search is hybrid.
The API never processes reels directly.
```

## Target architecture

```
Mobile/Web App
   |
   v
Go API Server (chi)
   |
   |-- Postgres (pgx): source of truth + FTS + vectors (pgvector)
   |-- Redis (go-redis): cache, rate limits, progress stream
   |-- RabbitMQ (amqp091-go): durable async jobs
   |-- Object storage: thumbnails
   |-- Prometheus: metrics
   |
   v
Go Worker Pool
   |-- downloader -> transcriber -> extractor -> categorizer -> embedder -> notifier
```

---

## Decision 1: Queue topology

**Decision: one work queue, one retry queue, one dead-letter queue. Not per-platform.**

```
exchange: reelpin.jobs            (direct)
queues:
  reelpin.jobs.work               routing key: jobs.process
  reelpin.jobs.retry              TTL + dead-letter back to work (delayed retry)
  reelpin.jobs.dead               terminal, manual inspection
```

The system processes one source. Per-platform queues would rebuild the exact dead
multi-platform narrative the cleanup audit just flagged. Delayed retry is done with
a retry queue that has a per-message TTL and dead-letters back into the work
exchange, the standard RabbitMQ delay pattern (no plugins).

What to implement deliberately (these are the interview-relevant bits):
durable queues, publisher confirms, manual ack/nack, prefetch (QoS) to bound
in-flight work, dead-letter exchange, idempotent job creation (dedup on
source-identity), worker claim ownership in Postgres.

Note on honesty: RabbitMQ is a sideways move from the current Dramatiq + Redis
broker. It does not make the system more reliable; the existing design already has
durable queues, atomic claim, retry, and dead-letter. RabbitMQ is here to learn
exchanges/DLX/prefetch/confirms, which are common ground in interviews. Defend it
as a learning choice, not a reliability upgrade.

---

## Decision 2: Schema - reuse vs new

**Decision: new tables in the same Postgres database, `rp_` prefixed, written only by the Go service. Do not touch the tables the live app reads.**

The Flutter app and the website both read the same Supabase Postgres directly. If
the Go service rewrites the existing `reels` table, a schema change can break the
live app mid-migration. So:

- Go service owns: `rp_reels`, `rp_processing_jobs`, `rp_processing_events`,
  `rp_processing_cache`, `rp_reel_locations`, `rp_device_push_tokens`,
  `rp_service_health`.
- Migrations via `goose` or `golang-migrate`, checked into the Go repo.
- During cutover, a backfill copies/serves from the new tables. Once the app points
  at the Go API for everything, the old tables can be retired.

Job states (one enum, stored as text):
```
queued | processing | completed | failed | retry_scheduled | dead_lettered | cancelled
```

`pgvector` adds a `vector` column on `rp_reels` for embeddings, plus a `tsvector`
generated column for FTS. See Decision 4.

---

## Decision 3: Auth

**Decision: validate Supabase JWTs in Go middleware. Do not build a new auth system.**

The app and website already authenticate users with Supabase. Tokens are Supabase
JWTs. The Go API verifies them:

- Supabase signs with HS256 using the project JWT secret (legacy) or asymmetric keys
  via JWKS (newer projects). **[DECIDE]** confirm which this project uses, then either
  hold the shared secret or fetch+cache the JWKS.
- Middleware extracts `sub` (the user id) and rejects expired/invalid tokens with 401.
- The service-role key is used only for the Go service's own privileged DB access via
  `pgx`, never exposed to clients.

This keeps a single identity across app, web, and the new backend.

---

## Decision 4: Embeddings and the vector store

**Decision: real embeddings via Gemini, stored in pgvector inside the same Postgres. Drop Pinecone.**

Why pgvector over Pinecone:
- The DB is already Postgres. Hybrid search (FTS + vector) runs in a single SQL
  query with reciprocal-rank fusion. With Pinecone you query two systems and merge
  in app code.
- One fewer external service to run, key, and pay for.
- "Hybrid search in Postgres with pgvector + tsvector and RRF" is a stronger, more
  specific thing to defend than "we called Pinecone."

Tradeoff to be honest about: Pinecone is a recognizable resume keyword and scales
past a single Postgres box. At ReelPin's size that scaling does not matter yet.
**[DECIDE]** If the Pinecone keyword matters to you for the resume, keep it; the
search code is structured the same either way (embed, upsert, query top-k). My
recommendation is pgvector.

Embedding model: use Gemini embeddings (the project already has a Gemini key and
uses `google-genai`). **[DECIDE + VERIFY]** confirm the current model id and output
dimension against the live google-genai docs at build time (do not hardcode from
memory). Whatever it is, it will not be 384, so this requires a fresh index/column,
not an in-place change.

Backfill: every existing reel's vector is currently the fake hashed-lexical one.
A one-shot backfill job must re-embed the whole corpus with the real model before
search is trustworthy. Plan this as an explicit migration step, not an afterthought.

---

## Decision 5: Search

Make search the flagship. Hybrid retrieval in one place:

```
parse query -> extract category/location/entities -> expand synonyms
  -> FTS candidates (Postgres tsvector)
  -> vector candidates (pgvector cosine)
  -> fuse (reciprocal rank fusion)
  -> apply structured boosts (category, location, recency)
  -> rerank
  -> return results WITH match_reasons
```

`match_reasons` in the response ("matched on location: Bengaluru", "semantic match
on transcript") makes the system debuggable and demos well.

Worked example, "restaurants in Bangalore":
- category = food/restaurant
- location = Bengaluru / Bangalore / BLR / known neighborhoods
- semantic = places to eat

### Search evaluation (do not skip)

This is the differentiator. Most side projects cannot measure search quality.

- Hand-label a small set: `{ "query": "...", "expected_reel_ids": [...] }`.
- Measure Precision@5, Recall@10, MRR, latency p95.
- Check it in and run it on every search change so quality is a number, not a vibe.

This is worth more on the resume than RabbitMQ. It backs the bullet
"relevance evaluation with Precision@5 and Recall@10" with something real.

---

## Decision 6: SSE progress streaming

Today the app polls `GET /processing-jobs/{id}`. Replace with a push stream:

```
worker -> writes progress event to a Redis stream
API    -> GET /api/v1/processing-jobs/{id}/events  (SSE, reads the Redis stream)
client -> receives {"status","step","progress"} events live
```

Events: `checking_cache 8`, `downloading 20`, `transcribing 40`, `extracting 60`,
`categorizing 70`, `saving 80`, `embedding 90`, `completed 100`. Store per-step
durations on the job row for debugging and for honest resume claims.

---

## Decision 7: Redis scope

Redis is for fast, losable things only. Never source of truth.

- rate limits (per-user, per-ip)
- recent progress events / SSE fanout (Redis streams)
- idempotency keys
- short-lived search cache
- worker heartbeat cache

---

## Decision 8: Observability

Prometheus metrics:
```
reelpin_jobs_submitted_total          reelpin_jobs_processing
reelpin_jobs_completed_total          reelpin_job_duration_seconds
reelpin_jobs_failed_total             reelpin_queue_depth
reelpin_jobs_dead_lettered_total      reelpin_search_latency_seconds
reelpin_search_requests_total         reelpin_external_api_failures_total
```

Structured logs (slog) with: `job_id, user_id, source_platform, step, attempt,
failure_code, duration_ms`. OpenTelemetry traces later:
`API enqueue -> RabbitMQ -> worker -> external APIs -> DB writes -> embedding`.

---

## Decision 9: Reliability checklist

Implement deliberately, each is interview material:
timeouts on every external call, retry only transient failures, dead-letter
permanent ones, provider cooldowns, per-platform concurrency limit (one platform,
so one limit), duplicate-source protection, graceful worker shutdown (drain
in-flight on SIGTERM), health checks (`/live`, `/ready`), rate limits.

---

## API surface (Phase 1 target)

```
POST   /api/v1/processing-jobs/reels
GET    /api/v1/processing-jobs/{id}
GET    /api/v1/processing-jobs
GET    /api/v1/processing-jobs/{id}/events   (SSE, Phase 5)
GET    /api/v1/reels
GET    /api/v1/reels/{id}
DELETE /api/v1/reels/{id}
POST   /api/v1/search
GET    /api/v1/health/live
GET    /api/v1/health/ready
GET    /metrics
```

## Tech stack

```
Go
chi              HTTP router
pgx              Postgres
goose            migrations
amqp091-go       RabbitMQ
go-redis         Redis
client_golang    Prometheus
slog             structured logs
testcontainers-go integration tests
pgvector         vector search (Decision 4)
```

Keep the framework count low. The point is to understand the system, not to wire
libraries.

## Pipeline (worker)

```
resolve source identity
check duplicate reel
check processing cache
download (Instagram fallback chain + cookie slots + Apify fallback)
transcribe (ffmpeg extract -> Gemini)
extract structured metadata (Gemini JSON)
categorize (Gemini, per-user taxonomy)
extract locations/entities (+ geocode, cached)
save reel
embed search document (real model -> pgvector)
send notification (FCM)
mark job completed
```

The operationally hard part is the Instagram download chain (cookie rotation,
getting IP-blocked, Apify fallback), not the Go. Budget real time there.

---

## Cutover plan

The app is live with real users. No big-bang switch.

1. Stand up the Go API alongside the Python service, new `rp_` tables.
2. Backfill real embeddings into pgvector for the existing corpus.
3. Move endpoints one at a time. Either version the API or route specific paths to
   Go at the load balancer. Start with read-only (`GET /reels`, `/search`), then the
   write path (`POST /processing-jobs/reels` + worker).
4. Run both pipelines in parallel briefly, compare outputs on the same reels.
5. Once the app talks only to Go, retire the Python service and old tables.

---

## Suggested timeline (adjust to reality)

```
Week 1  Go project, API skeleton, Postgres schema, job create/status/list, health
Week 2  RabbitMQ, worker service, job claim ownership, retry/dead-letter
Week 3  pipeline steps, external API integrations, processing cache, progress
Week 4  real embeddings + pgvector + hybrid search + eval harness   <-- pulled early
Week 5  Redis rate limits, SSE progress streaming, worker heartbeat
Week 6  Prometheus, Grafana, search eval suite expansion, load test, arch docs
```

Search is pulled to Week 4 (from the original Week 5) because it is the highest-value
and highest-risk piece and needs iteration time.

---

## Open decisions to confirm before coding

- **[DECIDE]** Supabase JWT verification: shared HS256 secret or JWKS? (Decision 3)
- **[DECIDE]** pgvector vs keep Pinecone for the resume keyword. (Decision 4)
- **[DECIDE + VERIFY]** Gemini embedding model id and output dimension, confirmed
  against current docs. (Decision 4)
- **[DECIDE]** New repo `reelpin-go/` as a workspace sibling, or replace `reelpin-api/`?

## Resume positioning (the payoff)

```
Rewrote ReelPin backend in Go as an async media-processing platform with RabbitMQ
durable queues, Postgres job state, Redis-backed SSE progress streaming, worker
claim ownership, retry/dead-letter handling, hybrid semantic + full-text search,
and Prometheus observability.

Built hybrid reel search with real transcript embeddings (pgvector), PostgreSQL
full-text search, structured entity/location metadata, query expansion, custom
reranking, and relevance evaluation with Precision@5 and Recall@10.
```

Both bullets become true only after the work above. The second one is currently
false of the Python service (fake embeddings); the rewrite is what earns it.
