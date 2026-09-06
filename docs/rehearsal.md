# Rehearsal runbook

**Nothing in this file has been executed.** It is Task 28: prove the migration
on a copy of production before any of it touches production. Written while the
code was fresh so whoever runs it is not reconstructing the order from memory.

The rehearsal exists to answer one question that is expensive to get wrong
later: **does the compute have to move before launch?** The EC2 development host
is in `ap-south-1` and both Supabase projects are in `ap-northeast-1`, and a
readiness ping between them measured about 265 ms. If the read gate below fails,
no amount of tuning fixes distance, and moving compute after traffic is much
harder than before.

## What you need first

- A **disposable** Supabase project. Not development, which now carries the
  declared auth config in `reelpin-web/supabase/config.toml`, and obviously not
  production.
- An anonymized copy of the production database. Anonymized means the reel and
  job rows keep their ids and shapes, and the user-identifying columns do not
  keep their values. The ids have to survive: the whole backfill design turns on
  `user_saves.id` preserving `public.reels.id`.
- The image digest that development tested. Not a rebuild. A rebuilt image is a
  different artifact and proves nothing about the one that will run.

## 1. Migrate and backfill

```sh
maintenance migrate                       # apply every migration
maintenance migrate-status                # confirm the list, no gaps
maintenance backfill-legacy               # dry run, reports and writes nothing
maintenance backfill-legacy --execute
```

The dry run first is not ceremony. It reports what it would carry, and a count
that surprises you is worth understanding before rows move.

## 2. Verify, and read the numbers properly

```sh
maintenance backfill-verify --sample 100
```

This exits non-zero when it fails, so it can gate a script. Three numbers decide
the gate, and only the first is the plan's:

- **`unexplained`** must be zero. A legacy row with no audit entry was neither
  carried nor deliberately skipped, so nobody knows what happened to it.
- **`carried_but_missing`** must be zero. The audit says a row was carried but
  no canonical row exists under the preserved id, which means a public reel id
  the app still holds now resolves to nothing.
- **Field mismatches** must be zero, and `user_saves.user_id` is the one that
  matters most: a wrong owner is one account's saves appearing in another's.

**Read `compared` before believing a pass.** A run that compared nothing passes
every check it made. The verifier logs a warning in that case; do not skip past
it.

**Read `text_not_comparable` too.** A content saved by several users carries one
version built from whichever legacy row the backfill reached first, and nothing
records which, so text comparison is skipped for those. If production
deduplicates far more than the test fixtures do, this can swallow most of a
100-row sample and the text check will have proven much less than it appears to.
Raise `--sample` if that number is large.

Three more things the verifier cannot know, worth reading off this first real
run rather than assuming:

- How many `public.reels` rows have a NULL `created_at`. Those get
  `saved_at = now()` from the backfill and the timestamp check skips them.
- Whether any legacy `reels.id` reaches `user_saves` from a source other than
  this backfill.
- Whether real `parse_status` values go beyond `parsed`. Anything else maps to
  `partial`, and the mapping is shared with the writer, so an unseen vocabulary
  verifies clean.

## 3. Stand up the infrastructure

Deploy the tested digest on the Mumbai production EC2, **beside** Python, taking
no traffic. Then RabbitMQ, Redis, persistent volumes, TLS, backups, monitoring
and secret rotation. None of this changes traffic; that is the point.

Load the alert rules and confirm they evaluate. `internal/metrics/alerts_test.go`
already proves every rule names a metric that exists, so a rule that does not
evaluate here is a Prometheus configuration problem, not a rules problem.

## 4. Measure, against the gates

| Measure | Gate |
|---|---|
| Read p95 | below 800 ms |
| Database network time | below 150 ms |
| Worker CPU, memory, disk | within the host's limits under the load driver |
| Provider concurrency | within the provider's limits |

**If read p95 or network time fails, move compute before launch.** That is the
decision this whole exercise exists to make.

Consumers: the host supports one media plus one light. Enable two media plus one
light **only** if the recorded load test stays inside CPU, memory and disk. Run
`cmd/load` and keep its report.

This is also the run that fills the spend ledger. Deploy with the cost gate off
(all four `COST_GATE_*` unset, which measures without restricting) and then read
`reelpin_provider_tokens_total{operation="transcribe"}` and the rest. Those
volumes are what `docs/cost-gate.md` needs before anyone can choose a warning
amount honestly.

## 5. Prove recovery, do not assume it

The inventory is not just PostgreSQL. It is PostgreSQL, Supabase Auth, Supabase
Storage objects, and the encryption keys required to read any of it back. A
backup that restores rows but not the keys is not a backup.

- Take a verified pre-migration recovery point.
- Confirm backups or PITR satisfy **RPO one hour**.
- **Run a restore and time it.** RTO is four hours and it is a measured number,
  not a plan. An untimed restore procedure is a hope.
- Compare restored row hashes and object checksums against the recovery point.
- Schedule the monthly restore drill. A drill that is never repeated stops being
  evidence within one infrastructure change.

## 6. Drill the alerts and the rollback

Every alert and rollback drill has to pass on production-shaped infrastructure,
not on a laptop. The two automatic rollback triggers are in `deploy/alerts.yml`
under `reelpin-rollback`, labelled `autorollback: "true"`:

- 5xx above 2% for five minutes, once there are at least 100 requests
- read p95 above 800 ms for fifteen minutes

The request floor matters: without it, two failures out of three requests in a
quiet window rolls back a healthy deployment on noise.

Rollback returns to the previous image digest and **never rolls back the
database**. Every migration in this stack is expand-only, so the prior digest
reads the current schema.

## Done when

- `backfill-verify` exits zero, with `compared` and `text_not_comparable` read
  and understood rather than glanced at.
- Read p95 and database network time are inside their gates, or the decision to
  move compute has been taken.
- A restore has been run and timed inside four hours, with hashes compared.
- The alert and rollback drills have passed on this infrastructure.
- The spend ledger has enough real jobs in it to price a month.

Then, and not before, `docs/cutover.md`.
