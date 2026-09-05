# Cutover runbook: Python to Go

**Nothing in this file has been executed.** Go does not serve any traffic. This
is the order the switch happens in, written while the code was fresh, so whoever
runs it is not reconstructing it from memory.

## Before you start

- The Go stack (PRs #2 to #22) is merged to `dev` and CI is green.
- `deploy/alerts.yml` is loaded into Prometheus and firing against dev.
- The two open items are closed or accepted:
  - Account deletion cannot delete the Supabase identity. `lifecycle.New` takes
    a `nil` auth deleter, so `auth_user_deleted` is `false`.
  - Hybrid search has never been measured against real Gemini embeddings. Run
    `maintenance search-eval --user <uuid>` and compare against
    `api/eval/REPORT.md` before search takes production traffic.
- Task 21 has 30 days of route usage data. Do not remove any route before that.

## Hosts and services

Production uses managed PostgreSQL/Supabase, managed Redis and managed RabbitMQ.
Compose stays local and dev only.

API and worker are separate systemd units and separate env files. Never copy a
development cookie set or database URL into production: the Instagram cookie
pool in particular is per-environment, and sharing it gets both banned.

## Deployment mechanics

`release.yml` builds once and pushes to GHCR. The deploy job takes the
**digest**, never a tag: a tag can move between the dev deploy and the
production deploy, and then the two are not the same image.

Deployment is `workflow_dispatch` only today. Making the dev deploy automatic on
push to `dev` is step 1 below.

**Production is a promotion, not a build.** Deploy to dev first, take the digest
from that run, and dispatch production with it in the `digest` input. Leaving
`digest` empty builds a fresh image, which is fine for dev and wrong for
production: it would deploy bytes nothing tested.

Migrations run as one job before anything new starts. The runner takes a
PostgreSQL advisory lock, so two deploys racing cannot both apply. Migrations
are expand-only: **never roll one back**. Roll back the application and ship a
corrective migration forward.

## The order

1. **Deploy Go on dev, serving nothing.** Python still owns all traffic. Watch
   readiness, the metrics endpoint and the alert rules for a day.
2. **Shadow idempotent reads only.** Mirror `GET` traffic to Go and compare
   normalized status, body and latency. Never shadow a mutation: two enqueues of
   the same share are two jobs.
3. **Route reads to Go on dev**, in this order: the Task 2 reels/jobs/account
   reads, then collections, map and account reads.
4. **Run the backfills and read the reports.** `maintenance backfill-content`
   then `maintenance backfill-embeddings`, both dry-run first. Verify the counts
   before continuing.
5. **Test the native paths end to end** on real devices: Android and iOS share
   extensions, share-token enqueue, cold-start job placeholders, collection
   filing at share time, FCM routing, and one share per platform.
6. **Switch dev enqueue to Go and RabbitMQ.** Python keeps draining its Redis
   queue but takes no new writes.
7. **Wait for Python's queued and processing jobs to reach zero.** Check for
   orphans before moving on.
8. **Rehearse the migration against a disposable production snapshot.** Throw it
   away afterwards; this is not a permanent staging environment.
9. **Open the final `dev -> main` promotion PR** and re-run every check.
10. **Deploy Go beside Python in production.** Switch read routes, then write
    routes, then enqueue, with a pause between each.
11. **Keep Go workers draining RabbitMQ through any API rollback.** The Python
    worker cannot consume RabbitMQ. Stopping the Go workers strands every job
    already published.
12. **Observe for 14 days** before decommissioning anything Python.

## Rollback

- **Before Go enqueue (steps 1 to 5):** point the reverse proxy back at Python.
  Both read the same schema, so nothing is stranded.
- **After Go enqueue (step 6 onward):** either keep the Go workers running, or
  stop new enqueue and let them drain. Never route a RabbitMQ job to Python.
- **Never roll back a migration.** Expand-only means the old code still works
  against the new schema.

## Then: retiring Python

A separate `reelpin-api` change, because a GitHub stack cannot span
repositories. After the 14-day window and with zero Python API traffic, zero
Redis processing messages, zero active Python jobs and no rollback event:

- Stop and disable the Python API and worker units.
- Remove the old deploy jobs and secrets, recording their owners first.
- Delete the Pinecone vectors and index **only** after the PostgreSQL search
  backup and its verification report are stored.
- Remove the Redis queue keys only after the retention and replay windows close.
- Apply the approved cleanup migrations from Task 21.
- Keep the Python repository and its final image tag read-only, as history.
- Update the agent context, the runbooks and the final timeline entry.

## Done when

The current release Flutter app and both native share extensions pass every flow
with no base-URL change and no client release.
