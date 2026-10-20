# Rehearsal runbook

**Nothing in this file has been executed against Supabase, Neon or
production.** It is Task 28: prove the migration on a copy of production before
any of it touches production. Written while the code was fresh so whoever runs
it is not reconstructing the order from memory.

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

## 1. Restore the Python tables into `legacy`

The backfill reads the Python tables and writes the Go tables through one
connection, `DATABASE_URL`, so the Python tables have to be inside the Go
database. They get there as a dump of Supabase's `public` schema, restored under
the schema name `legacy`. `pg_restore` cannot rename a schema on the way in, so
the rename happens first in a throwaway local Postgres, the stage.

Before the first run, read two things off Supabase:

- `SHOW server_version`. Use a `pg_dump` at least that new, and the stage image
  of the same major version.
- `\dx vector`. The commands below assume the extension lives in `public`. If
  it is in `extensions`, create it there in the stage instead
  (`CREATE SCHEMA extensions; CREATE EXTENSION vector SCHEMA extensions`). That
  variant has not been run.

Start the stage once:

```sh
docker run -d --name legacy-stage -e POSTGRES_HOST_AUTH_METHOD=trust \
  -p 55432:5432 pgvector/pgvector:pg17
STAGE=postgres://postgres@localhost:55432/postgres
```

Then, for the snapshot and again before every delta:

```sh
# Dump the Python tables. SUPABASE_DB_URL is the direct connection, not the pooler.
pg_dump "$SUPABASE_DB_URL" --schema=public --no-owner --no-privileges \
  --format=custom --file=public.dump

# Restore into the stage and rename public to legacy there.
psql "$STAGE" -c 'DROP SCHEMA IF EXISTS legacy CASCADE' \
  -c 'CREATE EXTENSION IF NOT EXISTS vector SCHEMA public'
pg_restore --dbname="$STAGE" --no-owner --no-privileges public.dump
psql "$STAGE" -c 'ALTER SCHEMA public RENAME TO legacy' -c 'CREATE SCHEMA public' \
  -c 'ALTER EXTENSION vector SET SCHEMA public'

# Dump legacy from the stage and replace it in the Go database.
pg_dump "$STAGE" --schema=legacy --no-owner --no-privileges \
  --format=custom --file=legacy.dump
psql "$DATABASE_URL" -c 'DROP SCHEMA IF EXISTS legacy CASCADE'
pg_restore --dbname="$DATABASE_URL" --no-owner --no-privileges --exit-on-error legacy.dump
```

The stage `pg_restore` reports `schema "public" already exists`. That one is
expected. Read any other error there: one on a function or policy that names
Supabase's `auth` schema only leaves that object out of `legacy`, which the
backfill never reads, but an error on a table is a real problem.

`ALTER EXTENSION vector SET SCHEMA public` is the step embeddings depend on.
Renaming `public` takes the `vector` type with it into `legacy`; moving the
extension back makes `legacy.dump` refer to `public.vector`, which is where the
Go database has it.

This sequence has been run twice in a row against throwaway local databases (a
`reels` table with a `vector` column, a bigserial table and a function), and the
restored column resolved to `public.vector`. It has not been run against
Supabase or Neon.

## 2. Migrate and take the snapshot

Run these as the schema owner: `reelpin_maintenance` has no INSERT.

```sh
maintenance migrate                       # apply every migration
maintenance migrate-status                # confirm the list, no gaps
maintenance backfill-legacy               # dry run, reports and writes nothing
maintenance backfill-legacy --execute
```

The dry run first is not ceremony. It reports what it would carry, and a count
that surprises you is worth understanding before rows move.

A finished `--execute` run logs `legacy backfill high-water mark` with
`next_since`. Write it down: it is the `--since` of the first delta. Dry runs and
runs stopped by `--max-rows` print no mark.

### Flags

`maintenance backfill-legacy`:

| Flag | Default | Meaning |
|---|---|---|
| `--execute` | false | Write. Without it the run reports what it would do and writes nothing. |
| `--batch-size` | 500 | Rows per keyset page. |
| `--max-rows` | 0 (all) | Stop the reel pass after this many rows, for a trial on a big table. Later passes are skipped. |
| `--since` | empty | Delta: an RFC3339 instant. Only rows changed after it are read, except the tables copied whole (below). |
| `--legacy-schema` | `legacy` | Schema the restored Python tables are read from. |

`maintenance backfill-verify`:

| Flag | Default | Meaning |
|---|---|---|
| `--sample` | 100 | Carried rows per table compared field by field (reels and jobs). Also caps every id and user list in the report. |
| `--batch-size` | 500 | Sampled rows read per query. |
| `--legacy-schema` | `legacy` | As above. |
| `--allow-drift` | false | Report mismatches but exit zero, for checks taken while both apps still write. |

### What a delta reads

A delta reads each table by the stamp Python moves when the row changes. Where
Python changes a row without moving any stamp, the table is read whole on every
run, delta or not, and the copy is an idempotent upsert.

| Legacy | Go | A delta reads |
|---|---|---|
| `reels` | `user_saves`, `contents`, `content_versions` | `created_at`. A carried reel is never re-read into its version. |
| `reels.locations` | `content_locations` | Every carried reel, every run. The geocode backfill rewrites locations through `update_reel_fields`, which moves nothing. |
| `reels.embedding` | `content_embeddings` | Every carried reel, every run. `set_reel_embedding` runs at the index step, after the row exists, and the reel has no `updated_at`. |
| `processing_jobs` | `processing_jobs` | `coalesce(updated_at, created_at)`. Every status change sets `updated_at`. |
| `user_entitlements` | `user_entitlements` | The whole table, every run. `synced_at` is set on insert only, and Pro is granted and extended by hand in the SQL editor. |
| `profiles` | `profiles` | `coalesce(updated_at, created_at)`. The app's upsert sets `updated_at`. |
| `manual_map_pins` | `user_manual_pins` | `coalesce(updated_at, created_at)`. Python's upsert sets `updated_at`. |
| `hidden_reel_map_pins` | `user_pin_preferences` | `greatest(hidden_at, created_at)`. Hiding again sets `hidden_at`. |
| `reminders` | `reminders` | `greatest(created_at, sent_at)`. The only update is the claim, which sets `sent_at`. |
| `price_tracks` | `price_tracks` | `greatest(created_at, last_checked_at, notified_at)`. The daily check sets `last_checked_at`. |
| `feedback`, `link_clicks` | same names | `created_at`. The website only inserts. |

Places and embeddings go only onto content versions this backfill built, never
onto one the Go pipeline built. Each version takes both from one carried reel,
the one it was built from, so a place's `ordinal` is its index in that reel's
`locations` array. That is what a hidden pin's `location_index` and a
`reel:{id}:{ordinal}` pin id name. The embedding keeps the reel's
`embedding_model`, `gemini-embedding-001` when the row has none.

### Save conflicts

A legacy reel whose user already has a save of the same content under another
id never becomes a second save, and the existing save is never deleted: its
collection items, reminders and price tracks cascade from it.

- Legacy row newer, by `created_at` against `saved_at`: its `saved_at` is
  written onto the existing save, which keeps its id. The run logs
  `save conflict: merged the newer legacy save into the existing one` and counts
  it in `conflicts_merged`.
- Otherwise, or with no `created_at`: the existing save is left alone and the
  run logs `save conflict: kept the newer save`.

Either way the legacy reel is audited `skipped_conflict`, and its reminders and
price tracks point at the save that was kept. Its hidden pins do not carry: their
index names a place on the legacy reel, not on the kept save's version.

## 3. Verify, and read the numbers properly

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

Every mapped table also reports `expected` against `present`, legacy rows in
scope that were not skipped against those with a canonical row owned by the same
user, and `user_diffs` for the users whose two counts differ. Search and the map
report `embeddings` (carried reels with an embedding against those whose version
has one) and `map_points` (located places on carried reels against
`content_locations`, and manual pins on each side). Any of those short fails the
run.

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

- How many `legacy.reels` rows have a NULL `created_at`. Those get
  `saved_at = now()` from the backfill and the timestamp check skips them.
- Whether any legacy `reels.id` reaches `user_saves` from a source other than
  this backfill.
- Whether real `parse_status` values go beyond `parsed`. Anything else maps to
  `partial`, and the mapping is shared with the writer, so an unseen vocabulary
  verifies clean.

## 4. Deltas during the dual run, then the final delta

Each night while both apps run:

1. Re-dump and replace `legacy` exactly as in section 1.
2. `maintenance backfill-legacy --execute --since <next_since>`, then record the
   new `next_since` it logs.
3. `maintenance backfill-verify --allow-drift`. Rows the new app wrote, and
   legacy rows not copied yet, are expected drift here.

A delta neither reads nor moves the snapshot's cursors, and running the same one
twice changes nothing.

The final delta, once:

1. Stop the Python worker and API.
2. Re-dump and replace `legacy` as in section 1.
3. `maintenance backfill-legacy --execute --since <next_since>`.
4. `maintenance backfill-verify`, **without** `--allow-drift`. It must exit zero.
5. `psql "$DATABASE_URL" -c 'DROP SCHEMA legacy CASCADE'`, and remove the stage
   with `docker rm -f legacy-stage`.

What no delta carries: a row deleted in Python after the snapshot stays in Go,
and a reminder copied while the Go worker runs is not in the Redis due set until
the worker re-seeds it on start.

## 5. Stand up the infrastructure

Deploy the tested digest on the Mumbai production EC2, **beside** Python, taking
no traffic. Then RabbitMQ, Redis, persistent volumes, TLS, backups, monitoring
and secret rotation. None of this changes traffic; that is the point.

Load the alert rules and confirm they evaluate. `internal/metrics/alerts_test.go`
already proves every rule names a metric that exists, so a rule that does not
evaluate here is a Prometheus configuration problem, not a rules problem.

## 6. Measure, against the gates

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

## 7. Prove recovery, do not assume it

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

## 8. Drill the alerts and the rollback

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
