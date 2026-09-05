# Backend and authenticated web roadmap

## Outcome

Build ReelPin Go as the backend for both Flutter and an authenticated web
application. A production user signs into the web app with the same Supabase
account and sees all content already saved by that user.

This plan keeps the audited backend migration work. It adds a web delivery
track and changes two assumptions:

- Development and production use separate Supabase, Redis and RabbitMQ
  resources. Tables do not use an `environment` discriminator.
- Go defines the API contract. Flutter dev and the web client follow it after
  the backend behavior is correct and tested.

## Current evidence

- `dev` contains PR #2 and PR #24 at merge commit `cfc6d79`.
- PR #2 passed unit, race, vet, real Supabase and EC2 end-to-end tests.
- The development Supabase project is `reelpin-go` in `ap-northeast-1`. Its 22
  tracked production-schema migrations are applied and its JWKS uses ES256.
- The development project was left with zero test users, reels and jobs after
  the end-to-end test.
- `ssh reelpin-ec2-dev` reaches Ubuntu 24.04.4 in `ap-south-1`. Docker Engine
  29.8.0 and Compose 5.5.1 are installed. Python remains on localhost:8000 and
  Go PR #2 is running privately on localhost:8080. A real readiness ping to the
  Tokyo development database took about 265 ms.
- `reelpin-web` `origin/main` is `1fb89dd`. It is a Next.js 16 marketing site on
  Vercel with feedback and deep-link routes, but no user session or library UI.

## Decisions

| Area | Decision | Why | Revisit when |
|------|----------|-----|---------------|
| Service shape | One Go module with API, worker and maintenance binaries | One deployable codebase keeps transactions, contracts and debugging together | Independent teams or scaling measurements require separate services |
| Database | PostgreSQL in Supabase | Existing production data is there; relational ownership, JSONB, PostGIS, full-text search and pgvector fit the product | A measured workload cannot be handled by PostgreSQL |
| Environments | Separate Supabase projects, Redis instances and RabbitMQ virtual hosts | Infrastructure isolation prevents a dev process from reading or claiming production work | Never replace this with row flags |
| Compute region | Use the provided Mumbai production EC2 while Supabase remains in Tokyo | Reuse the existing production host and measure the accepted cross-region latency | Move compute after production rehearsal or latency SLO failure |
| Authentication | Supabase Auth for Flutter and web; local JWKS verification in Go | One identity across clients, no per-request Auth network call | Immediate token revocation becomes a measured requirement |
| Queue | RabbitMQ with durable queues, publisher confirms, manual acknowledgements and dead letters | Processing is long-running, retryable and observable | A managed workflow engine becomes cheaper than operating the queue |
| Delivery | At-least-once messages with idempotent stages and a transactional outbox | Exactly-once delivery is not available across the database and broker | Do not claim exactly-once behavior |
| Content identity | Global `content` and versioned extraction, separate from each user's save | The same public source should be downloaded and processed once | Private or credential-scoped content uses a separate access scope |
| Public resource name | `/api/v1/library-items`, not `/reels` | Saved content includes videos, posts, places and generic links | Decide before generating the web client; do not add an alias afterward |
| Search | pgvector, PostgreSQL full-text search and trigram matching fused with RRF | Semantic, exact and typo-tolerant retrieval stay beside the source data | Measured scale or relevance justifies a separate search service |
| Web repository | Extend `reelpin-web` | It already owns `reelpin.in`, Next.js, Vercel, Supabase JavaScript and deep links | Split only if deployment or ownership becomes independent |
| Web auth | `@supabase/ssr`, PKCE and cookie sessions | Next.js server components and browser components share one refreshed session | Follow Supabase changes while the package remains beta |
| Product data access | Web calls Go; browser code does not query ReelPin tables directly | One authorization and business-rule path for Flutter and web | Public read models may be exposed later with explicit RLS design |
| Authenticated caching | No ISR or shared CDN caching | A cached response containing session cookies or user data can cross users | Add only private, user-keyed caching with tests |
| Production migration | Expand, backfill, verify, switch reads, then switch writes | Existing users and IDs remain valid and rollback stays possible | Destructive cleanup waits until the observation window closes |

## Target flow

```text
Flutter                         reelpin.in/library
   |                                   |
   +-------- Supabase Auth ------------+
                   |
             access token
                   |
                   v
             ReelPin Go API
            /              \
     PostgreSQL           RabbitMQ
    user saves and       processing jobs
    global content             |
                               v
                         ReelPin Go worker
                               |
                providers, Gemini, storage
```

The web browser signs in with Supabase. Next.js stores the session in cookies,
verifies the session for protected pages and forwards the access token to Go.
Go verifies the signature and uses `sub` as the only user identifier.

## Production data rule

The current Supabase project remains production. Existing users are not copied
to a new Auth project. Their JWT `sub` values already match `public.reels.user_id`.

The production path is additive:

1. Keep every existing `public.reels.id` and `user_id`.
2. Add global content tables and nullable links from existing rows.
3. Backfill source identity and content versions in batches.
4. Verify row counts, unmapped rows and duplicate identities.
5. Serve old and newly processed saves through one `library-items` reader.
6. Keep writing the user-facing save record expected by existing production
   data while the global pipeline owns the extraction.
7. Remove no table or column during the initial web launch.

An old user proves the migration: sign in on the web with an existing account,
compare the library count with the production database, open several old items,
and confirm filters, locations and transcripts.

## Backend roadmap

The existing migration tasks remain. Each open stacked PR is reviewed and
rebased, not merged because it already exists.

### Foundation, complete

- Task 1: HTTP lifecycle, JSON errors, health and shutdown.
- Task 2: Supabase JWT authentication and core read behavior.
- Agent setup: committed `AGENTS.md`, `CLAUDE.md`, architecture, decisions and
  `make check`.

### Wave A: contract and data foundation

1. **Redesign Task 3 contract CI.** Replace Python route coverage with an
   OpenAPI document owned by Go, a generated route manifest, JSON fixtures and
   TypeScript client generation. Rename the saved-content routes to
   `/api/v1/library-items` before a web client is generated.
2. **Keep Task 4 safe ingress.** Normalize URLs, permit HTTP and HTTPS only,
   block private and link-local targets, recheck DNS after redirects, cap
   response sizes and verify media signatures.
3. **Rework Task 5 schema.** Go migrations must create all required tables on an
   empty development project and remain additive on production. Keep existing
   public user data; create global contents, content versions, locations,
   chunks, processing runs, stage results and outbox tables. Do not add
   environment columns.
4. **Keep Task 6 backfill.** Dry run first, checkpoint progress, use bounded
   batches, record failures and make reruns safe. Add a production report with
   counts before and after.
5. **Keep Task 7 Redis controls.** Rate limits and disposable caches only.
   Separate key prefixes by service purpose, not by a shared database row flag.

### Wave B: queue and processing

6. **Keep Task 8 RabbitMQ foundation.** Durable exchange and queues, publisher
   confirms, manual acknowledgements, bounded prefetch, retry queues, dead
   letters and graceful consumer shutdown.
7. **Keep Task 9 enqueue and global deduplication.** One active run for a global
   content identity and processor version. Each user receives a private job and
   save subscription. The database transaction also writes the outbox event.
8. **Keep Task 10 checkpointed pipeline.** Prepare, transcribe, extract,
   categorize, save and index. A stage result is reused only when stage version
   and input hash match.
9. **Keep Tasks 11 to 13 platform implementations.** Instagram first, then
   YouTube, TikTok, Pinterest, places, generic web, X, LinkedIn and Reddit.
   Provider-specific logic stays behind the platform interface.

### Wave C: product behavior

10. **Keep Task 14 collections.** Ownership, membership, invites, share links
    and filing an item into collections at enqueue time.
11. **Keep Task 15 map and Discover.** Normalized locations, manual pins,
    hidden pins, PostGIS queries and provider-backed place search.
12. **Keep Task 16 notifications.** Device tokens, completion notifications and
    campaigns with clear admin authorization.
13. **Keep Task 17 lifecycle.** Library-item deletion, account deletion,
    retention and Supabase identity deletion. Shared global content is deleted
    only after its final user reference disappears.

### Wave D: search and operations

14. **Keep Task 18 embeddings.** Versioned embedding documents and transcript
    chunks using `gemini-embedding-001` at 768 dimensions.
15. **Keep Task 19 hybrid search.** Dense, full-text and trigram arms, SQL-level
    user filters, RRF fusion, relevance gating and a labeled evaluation set
    measured with real Gemini embeddings.
16. **Keep Task 20 operations.** Route-shaped metrics, redacted logs, dependency
    readiness, worker heartbeats, alert rules and load tests. Thresholds are
    tested on development traffic before production.
17. **Keep Task 21 usage and cleanup evidence.** It becomes a production-data
    audit, not a reason to carry compatibility aliases. No destructive cleanup
    occurs before the web launch and observation window.
18. **Rework Task 22 deployment.** One digest is built, deployed to development,
    tested and promoted to production. Migrations run under an advisory lock.
    Rollback changes application traffic, never rolls database migrations back.
19. **Close Task 23.** Separate infrastructure replaces its environment-column
    design.

## Web roadmap

The web work begins after Wave A gives it a stable contract and development API.
It can proceed while the worker waves continue.

### W1: repository and authentication

- Branch from current `reelpin-web` `origin/main`.
- Update its `AGENTS.md` for the authenticated product routes.
- Add `@supabase/ssr`; keep `@supabase/supabase-js`.
- Add browser and server Supabase clients plus the Next.js 16 session proxy.
- Use PKCE for Google and email sign-in.
- Add `/login`, `/auth/callback` and sign-out.
- Protect product pages on the server with `getClaims()`.
- Use development Supabase in Vercel previews and production Supabase only in
  the production Vercel environment.

### W2: typed Go client

- Generate TypeScript types from the Go-owned OpenAPI document.
- Keep one server-side API client in `src/lib/reelpin-api/`.
- Forward the current Supabase access token as a bearer token.
- Give requests timeouts and map Go error codes to user-facing states.
- Never send the Supabase service-role or secret key to the browser.
- Mark authenticated fetches private and uncached.

### W3: library and submission launch slice

- Keep the marketing page at `/`.
- Add the product under `/library` with a separate work-focused layout.
- Show saved content in a responsive, virtualizable grid or list.
- Support platform, category, subcategory and saved-date filters.
- Add `/library/[id]` with summary, facts, transcript and locations.
- Let a signed-in user submit a new link for processing.
- Show processing-job placeholders and poll only while work is active.
- Add account menu, sign-out, empty, loading, retry and offline states.
- Preserve `/privacy`, `/feedback`, `/c/*` and `/go/*` behavior.

### W4: search, map and collections

- Add `/search` after Task 19 passes the real embedding evaluation gate.
- Add `/map` after Task 15 is stable and browser map costs are bounded.
- Add `/collections` and shared collection management after Task 14.
- Use URL query parameters for shareable filter and search state where safe.

### W5: remaining write workflows

- File a new save into collections.
- Delete a library item and delete an account with explicit confirmation.
- Keep mutations in Next.js server actions or route handlers with same-site
  checks; Go remains the owner of validation and state changes.

## Web design rules

- Marketing keeps its existing yellow-room visual system.
- Product pages are quieter and denser: predictable navigation, clear filters,
  compact rows or tiles, and no marketing sections inside the application.
- Use the existing Tailwind 4 tokens and shared brand fonts.
- Server components are the default. Client components exist only for
  interaction or browser-only APIs.
- Authenticated pages are dynamic and never use ISR.
- Mobile, tablet and desktop layouts are verified with browser screenshots.
- Keyboard navigation, visible focus, semantic headings and AA contrast are
  release requirements.

## Environment map

| Component | Development | Production |
|-----------|-------------|------------|
| Supabase | `reelpin-go` project | Current ReelPin project |
| Go API | `reelpin-ec2-dev` in `ap-south-1`, localhost:8080 until routed | Provided production EC2 in Mumbai |
| Python API | Existing dev service on localhost:8000 | Remains until Go write cutover |
| Redis | Dedicated Go development instance or namespace | Dedicated production instance |
| RabbitMQ | Dedicated development virtual host | Dedicated production virtual host |
| Web | Local and Vercel preview | `reelpin.in` on Vercel |
| Web API URL | `api-dev.reelpin.in` after DNS/TLS | `api.reelpin.in` |

## Tests and evidence

### Backend

- Unit tests for domain and handler behavior.
- Race tests for shared Go state.
- Tagged integration tests with PostgreSQL, Redis and RabbitMQ.
- Migration up tests from an empty PostgreSQL database.
- Migration tests against a production-shaped schema snapshot.
- Contract fixtures and route-manifest checks.
- Real Supabase JWT tests in development.
- End-to-end enqueue, worker, save and search tests on EC2 dev.
- Load tests with recorded p50, p95 and error rate.

### Web

- Unit tests for API mapping and presentation decisions.
- Component tests for loading, empty, failure and populated states.
- Playwright tests for login, refresh, sign-out, library, detail and filters.
- Two-user isolation test that proves one account cannot see another library.
- Expired-session and revoked-session behavior.
- No-cache test for authenticated server responses.
- Responsive screenshots at phone, tablet and desktop sizes.
- Accessibility scan and keyboard-only flow.

### Production-data gate

- Record production row counts before migration.
- Dry-run every backfill.
- Report mapped, skipped, duplicate and failed rows.
- Compare a sample of old items between Python and Go.
- Test with an existing production account before public web launch.
- Test both existing production accounts and new sign-ups. Enable link
  submission only after enqueue, RabbitMQ, worker and job polling pass together.

## Deployment order

1. Standardize the Go dev container and systemd unit on localhost:8080.
2. Run RabbitMQ in its own development container and persist its volume.
3. Publish `api-dev.reelpin.in` through Nginx with TLS after API tests pass.
4. Configure Vercel preview auth redirect URLs and dev environment variables.
5. Launch web auth, library and link submission against development after one
   ingestion path works end to end.
6. Finish backend worker, platform and search waves.
7. Rehearse production migrations against a disposable schema copy.
8. Rehearse on the provided Mumbai production EC2 and record cross-region
   database latency, CPU and memory evidence.
9. Deploy the tested Go image beside Python in production.
10. Shadow and compare safe reads.
11. Launch production web for existing users and new sign-ups.
12. Observe errors, latency and data mismatches.
13. Switch processing writes and workers to Go.
14. Keep Python available for rollback until the observation window ends.
15. Retire Python and old search infrastructure only after usage is zero.

## Rollback

- A web failure rolls Vercel back to the previous deployment.
- A Go read failure routes reads back to Python while Go workers continue if
  they already own RabbitMQ jobs.
- A Go write failure stops new enqueue and drains existing RabbitMQ work.
- Database migrations are forward-only and additive. Rollback never drops a
  column or table.
- Existing production user IDs and reel IDs remain valid through every phase.

## Interview demonstration

The smallest convincing vertical slice is:

1. Sign into `reelpin.in` with Supabase.
2. Open the same user's saved library from Go.
3. Filter by platform and category.
4. Open one old saved item and show its transcript and location.
5. Explain that the JWT subject scopes every query.
6. Show dev and production isolation at the infrastructure level.
7. Show one content identity shared by two private user saves.
8. Show a failed message retry and dead-letter path.
9. Show hybrid search evaluation numbers only after real embeddings are tested.
10. Show the deployment digest promoted from dev to production.

For each decision, present: context, choice, rejected alternative, trade-off and
revisit condition. The decision records in `docs/decisions/` are the source.

## Immediate next work

1. Close PR #23 and mark its environment-column design superseded.
2. Rebase Task 3 onto `dev` and replace Python compatibility coverage with the
   Go-owned contract.
3. Rename the public saved-content resource to `library-items` before client
   generation.
4. Rework the schema migration so an empty dev project and current production
   both reach the same target schema.
5. Turn the temporary EC2 PR #2 container into a versioned dev deployment.
6. Add RabbitMQ dev isolation.
7. Start web authentication and library reads after Task 3 and schema checks;
   launch only after enqueue, RabbitMQ, worker and one ingestion path also pass.

## Done

- A new development environment can be created from repository commands alone.
- Go API and worker pass all checks on EC2 dev.
- Flutter dev and ReelPin web use the Go-owned contract.
- An existing production user can sign into the web app and see every saved
  item without an account or ID migration.
- New content is processed once globally and saved privately per user.
- Search quality is measured with real embeddings.
- Production has tested rollback, alerts and data reconciliation evidence.
- Python receives no traffic and has no queued work before retirement.
