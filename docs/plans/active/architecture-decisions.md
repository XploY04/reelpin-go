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
| A | Product boundary | Launch users, web MVP features, mobile scope and administration | Open |
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
| N | Web architecture | Repository, routes, SSR, browser/server split, API client and state handling | Open |
| O | Web experience | Navigation, library views, detail, filters, search, map and accessibility | Open |
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

## Current questions

### Q1: First web release

Should the first public web release be read-only, showing the existing user's
library and item details, or should it also accept new links for processing?

Recommendation: read-only first. It can launch after the read API is stable and
does not wait for the complete worker migration.

### Q2: Client rollout order

Should production web move to Go reads before the production Flutter app, or
should both clients switch together?

Recommendation: web first, then Flutter. Web can be rolled back immediately and
does not depend on an app-store release.

### Q3: Who may sign in

At the first web launch, should access be open to every existing user, invite
only, or open to existing users plus new sign-ups?

Recommendation: every existing user plus new sign-ups, matching one product
identity across web and mobile.

### Q4: Public product URL

Should the signed-in product live at `reelpin.in/library` or on
`app.reelpin.in`?

Recommendation: `reelpin.in/library`. It keeps one Next.js deployment, one
cookie domain and the existing marketing/deep-link routes.

## Decision log

No decisions have been confirmed during this questionnaire yet.
