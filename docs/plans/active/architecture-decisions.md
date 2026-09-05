# Architecture decision register

**Status:** discovery in progress

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
| A | Product boundary | Launch users, web MVP features, mobile scope and administration | Partly confirmed |
| B | Domain language | Names for global content, user saves, jobs, collections and public routes | Open |
| C | API contract | REST shape, versioning, pagination, errors, idempotency and compatibility policy | Open |
| D | Data model | Tables, keys, relationships, JSONB boundaries, constraints and indexes | Open |
| E | Production migration | Baseline schema, backfill, reconciliation, dual operation, cutover and cleanup | Open |
| F | Authentication | Supabase flows, sessions, JWT verification, revocation and account lifecycle | Open |
| G | Authorization | Ownership, collection roles, admin access and record-not-found behavior | Open |
| H | Processing | Pipeline stages, retries, leases, timeouts, idempotency and failure classes | Open |
| I | Queue | RabbitMQ topology, routing, acknowledgements, prefetch, retry and dead letters | Open |
| J | Providers | Gemini, Apify, Google, Firebase, download tools, quotas and fallback behavior | Open |
| K | Search | Documents, embeddings, lexical search, filters, ranking, evaluation and freshness | Open |
| L | Cache and limits | Redis responsibilities, cache invalidation, rate limits and abuse controls | Open |
| M | Media and storage | Temporary media, thumbnails, Supabase Storage, retention and size limits | Open |
| N | Web architecture | Repository, routes, SSR, browser/server split, API client and state handling | Partly confirmed |
| O | Web experience | Navigation, library views, detail, filters, search, map and accessibility | Partly confirmed |
| P | Environments | Local, dev and production resources, secrets, DNS and region placement | Partly confirmed |
| Q | Deployment | Containers, image promotion, migrations, traffic switching and rollback | Open |
| R | Reliability | SLOs, timeouts, graceful shutdown, degradation and disaster recovery | Open |
| S | Observability | Logs, metrics, traces, dashboards, alerts and audit events | Open |
| T | Security and privacy | SSRF, validation, CORS, CSRF, headers, encryption, deletion and retention | Open |
| U | Testing | Unit, integration, contract, end-to-end, load, migration and production smoke tests | Open |
| V | Delivery workflow | Branches, PR size, CI gates, ownership, docs and release approvals | Open |
| W | Scale and cost | Expected load, concurrency, provider budgets, database size and scaling triggers | Open |

## Proposals awaiting confirmation

These are recommendations from the current roadmap, not confirmed decisions.

| ID | Proposal |
|----|----------|
| P1 | Use PostgreSQL in Supabase as the system of record. |
| P2 | Use one Go modular monolith with API, worker and maintenance binaries. |
| P3 | Use Supabase Auth for Flutter and web; verify access tokens locally in Go with JWKS. |
| P4 | Use `content` for a global source, `user_save` for ownership, and `library item` in the public API and UI. |
| P5 | Let web and Flutter access product data through Go rather than direct Supabase table queries. |
| P6 | Extend `reelpin-web`, keeping marketing at `/` and placing the product at `/library`. |
| P7 | Use `@supabase/ssr`, PKCE and cookie sessions in Next.js. |
| P8 | Use a transactional outbox and idempotent stages for at-least-once RabbitMQ delivery. |
| P9 | Use Redis only for disposable cache, rate-limit and ephemeral worker state. |
| P10 | Use pgvector, PostgreSQL full-text search and trigram matching with RRF fusion. |
| P11 | Preserve production IDs through expand, backfill, verify and switch migrations. |
| P12 | Run production Go compute near the Tokyo Supabase region. |

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

**Consequence:** Sign-up abuse controls, email verification, onboarding and an
empty-library experience are launch requirements.

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

## Current questions

### Q5: Public name for one saved record

What should the API and UI call one saved record?

- `A`: Library item. Internally, global source data is `content` and ownership is
  `user_save`. Recommended because it works for videos, posts, places and URLs.
- `B`: Content.
- `C`: Reel.

### Q6: ID exposed by the API

Which ID should `/api/v1/library-items/{id}` expose?

- `A`: The user's save ID. Recommended. It preserves existing `reels.id` values
  and never exposes another user's relationship.
- `B`: The global content ID shared by all users.
- `C`: Expose both as public identifiers.

### Q7: Repeated submission

What happens when the same user submits the same source again?

- `A`: Return the existing library item if complete; attach to the existing job
  if processing. Recommended. Across users, keep separate saves but reuse global
  processing.
- `B`: Create another library item every time.
- `C`: Reject it as a conflict.

### Q8: Removing a library item

What should deletion mean?

- `A`: Remove only that user's save. Keep global processed content while another
  user references it; purge unreferenced content later. Recommended.
- `B`: Delete the global content and every user's save.
- `C`: Never delete, only hide.

### Q9: Collections

Can one library item belong to multiple collections?

- `A`: Yes, use a many-to-many relation. Recommended.
- `B`: No, exactly one collection.
- `C`: Collections are not part of the product.

## Decision log

| Date | Decisions |
|------|-----------|
| 2026-09-05 | D1 through D4 confirmed by the product owner. |
