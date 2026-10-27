# Rehearsal runbook

**Sections 1 through 3 have been run for real**: on 2026-10-27, after PR #98,
the snapshot's `--execute` and `backfill-verify --sample 100` completed clean
against a production Supabase dump restored into Neon development. The deltas
and final delta have not run, and the remaining rehearsal gates are still
unproven. This is Task 28: prove the migration on a copy of production before
the production cutover.

The rehearsal exists to answer one question that is expensive to get wrong
later: **does the compute have to move before launch?** The EC2 development host
is in `ap-south-1` and both Supabase projects are in `ap-northeast-1`, and a
readiness ping between them measured about 265 ms. If the read gate below fails,
no amount of tuning fixes distance, and moving compute after traffic is much
harder than before.

## What you need first

- A Neon development database as the rehearsal destination, with the Go
  migrations applied. The 2026-10-27 run used `reelpin-dev`; production gets a
  separate Neon project at cutover.
- Read access to the production Supabase database as the dump source. The
  rehearsal reads Supabase and writes the restored `legacy` schema and Go
  tables in Neon development.
- A production dump that keeps reel and job ids and shapes. The 2026-10-27
  rehearsal used the real production dump, including the limited user export
  below. The ids have to survive: `user_saves.id` preserves `public.reels.id`.
- The image digest that development tested. Not a rebuild. A rebuilt image is a
  different artifact and proves nothing about the one that will run.

## 1. Restore the Python tables into `legacy`

The backfill reads the Python tables and writes the Go tables through one
connection, `DATABASE_URL`, so the Python tables have to be inside the Go
database. They get there as a dump of Supabase's `public` schema, restored under
the schema name `legacy`. `pg_restore` cannot rename a schema on the way in, so
the rename happens first in a throwaway local Postgres, the stage.

### Connecting to Supabase

The Supabase CLI hands out a temporary login, so no database password is
needed. From a directory linked to the project:

```sh
supabase link --project-ref <project-ref>
supabase db dump --linked --dry-run
```

The dry run dumps nothing. It prints the script it would run, and that script
carries a temporary connection for the role `cli_login_postgres`. Build
`SUPABASE_DB_URL` from it. That role is not the table owner, so every command
below assumes it: `pg_dump` takes `--role postgres`, and the `auth.users`
export runs `SET ROLE postgres` first. This is the recommended way in; a direct
connection string with the `postgres` password also works, without the role
switches.

### Before the first run

Read two things off Supabase:

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
# Read Supabase's clock immediately before the dump. This is --dumped-at.
# SUPABASE_DB_URL is the direct connection, not the pooler.
DUMPED_AT=$(psql "$SUPABASE_DB_URL" -Atc "SELECT to_json(now())#>>'{}'")

# Dump the Python tables.
pg_dump "$SUPABASE_DB_URL" --role postgres --schema=public --no-owner --no-privileges \
  --format=custom --file=public.dump

# Export the users straight after the dump: id, email and created_at only.
psql "$SUPABASE_DB_URL" -c 'SET ROLE postgres' \
  -c "\copy (SELECT id, email, created_at FROM auth.users) TO 'users.csv' WITH (FORMAT csv, HEADER)"

# Restore into the stage and rename public to legacy there.
psql "$STAGE" -c 'DROP SCHEMA IF EXISTS legacy CASCADE' \
  -c 'CREATE EXTENSION IF NOT EXISTS vector SCHEMA public' \
  -c 'CREATE EXTENSION IF NOT EXISTS pg_trgm SCHEMA public'
pg_restore --dbname="$STAGE" --no-owner --no-privileges public.dump
psql "$STAGE" -c 'ALTER SCHEMA public RENAME TO legacy' -c 'CREATE SCHEMA public' \
  -c 'ALTER EXTENSION vector SET SCHEMA public' \
  -c 'ALTER EXTENSION pg_trgm SET SCHEMA public'

# Dump legacy from the stage and replace it in the Go database.
pg_dump "$STAGE" --schema=legacy --no-owner --no-privileges \
  --format=custom --file=legacy.dump
psql "$DATABASE_URL" -c 'DROP SCHEMA IF EXISTS legacy CASCADE'
pg_restore --dbname="$DATABASE_URL" --no-owner --no-privileges --exit-on-error legacy.dump

# Load the users next to the Python tables, where the backfill reads them.
psql "$DATABASE_URL" \
  -c 'CREATE TABLE legacy.users (id UUID PRIMARY KEY, email TEXT, created_at TIMESTAMPTZ)' \
  -c "\copy legacy.users FROM 'users.csv' WITH (FORMAT csv, HEADER)"
```

Users never travel in the dump. Supabase's `auth.users` also holds password
hashes and tokens, so only three columns leave it, and only as `users.csv`.
Every user-owned Go table has a foreign key to `auth.users`, which on Neon is a
mirror the backfill fills from `legacy.users` before any other table. Export it
in the same sitting as `DUMPED_AT` and the dump, straight after `pg_dump`, so
every user the dumped rows name is in the file. Delete `users.csv` once it is
loaded. A run, dry or not, and `backfill-verify` both stop if `legacy.users` is
missing.

The stage `pg_restore` reports errors that are expected, because the stage has
no Supabase `auth` schema:

- `schema "public" already exists`;
- every RLS policy that calls `auth.uid()`;
- the view `user_display_names`;
- the foreign keys from `device_push_tokens` and `profiles` to `auth.users`.

Each of those only leaves that object out of `legacy`, which the backfill never
reads. None of them is a table. Read any other error: an error on a table is a
real problem.

`ALTER EXTENSION vector SET SCHEMA public` is the step embeddings depend on.
Renaming `public` takes the `vector` type with it into `legacy`; moving the
extension back makes `legacy.dump` refer to `public.vector`, which is where the
Go database has it. `pg_trgm` needs the same treatment, created before the
restore and moved back after the rename: Supabase's `reels` has a
`gin_trgm_ops` index, which does not restore without it.

This sequence ran against Supabase production and Neon development on
2026-10-27, with the `--role postgres` and `SET ROLE postgres` switches above.

`DUMPED_AT` is what the next delta's mark comes from, so read it on Supabase
and nowhere else. The dump, the rename and the restore of a table of 768-wide
vectors can take well over five minutes, and anything Python writes in that
time is not in this restore. A mark taken from the Go database's clock after
the restore would sit past those rows, and every later delta would skip them.

## 2. Migrate and take the snapshot

Run these as the schema owner: `reelpin_maintenance` has no INSERT.

```sh
maintenance migrate                       # apply every migration
maintenance migrate-status                # confirm the list, no gaps
maintenance backfill-legacy --dumped-at "$DUMPED_AT"   # dry run: writes auth.users only
maintenance backfill-legacy --execute --dumped-at "$DUMPED_AT"
```

The dry run first is not ceremony. It reports what it would carry, and a count
that surprises you is worth understanding before rows move. It reads every
legacy reel and job and makes the real run's decisions about each: an unusable
URL, unreadable JSON, an owner that is not a UUID (`invalid_owners`), an owner
missing from `auth.users` (`unknown_owners`), a user Go has served, and a
content already counted. The mapped tables run their real statements and roll
them back, so a constraint rejects the same rows it will on `--execute`. What
it cannot see is anything that depends on rows the reel pass writes: save
conflicts, jobs with no content to point at (`jobs_created` can be higher than
the real run's), hidden pins, reminders and price tracks that join a save, and
the places and vectors carried onto saves. On a first snapshot
`locations_copied`, `embeddings_copied` and `embeddings_skipped` read 0, so the
dry run gives no preview of vectors left behind for their width.

The dry run does write one thing: the users step commits `legacy.users` into
`auth.users` on every run, dry or not, and logs `users_written`. It is an
idempotent mirror of identities Supabase already has, and without it every
other table's dry run would count each row as a missing parent.

A finished `--execute` run logs `legacy backfill high-water mark` with
`next_since`, which is `--dumped-at` less five minutes. Write it down: it is the
`--since` of the first delta. The five minutes cover a row stamped just before
the dump that committed just after it. Dry runs and runs stopped by
`--max-rows` print no mark.

An interrupted snapshot resumes from its cursor. Resume it with the
`--dumped-at` of the dump the first attempt read, even if `legacy` has been
re-dumped since: rows added behind the cursor after that first dump were never
read, and only the first dump's time puts the mark before them.

### Flags

`maintenance backfill-legacy`:

| Flag | Default | Meaning |
|---|---|---|
| `--execute` | false | Write. Without it the run reports what it would do and writes nothing but the users step's `auth.users` rows. |
| `--batch-size` | 500 | Rows per keyset page. |
| `--max-rows` | 0 (all) | Stop the reel pass after this many rows, for a trial on a big table. Later passes are skipped. |
| `--since` | empty | Delta: an RFC3339 instant. Only rows changed after it are read, except the tables copied whole (below). Rows of users Go has served, or who deleted their account in Go, are never read, by a delta or any other run. |
| `--dumped-at` | empty | RFC3339 instant the Supabase dump was taken, from `SELECT now()` on Supabase just before `pg_dump`. Required with `--execute` or `--since`. Must be later than `--since`. |
| `--propagate-deletes` | false | Final delta only, with Python stopped. Deletes carried rows whose legacy row is gone, and the `auth.users` row (with everything it owns) of every user gone from `legacy.users`, for users Go has not served. Needs `--since`. |
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
| `users` (from `auth.users`) | `auth.users` | The whole table, every run, before anything else, including users Go has served. Only the email is updated on a user already there. A user who deleted their account in Go is never copied back. |
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
| `collections`, `collection_members`, `collection_items`, `collection_invites` | same names | The whole collection family, every run. Member roles and invite use/revocation updates have no change timestamp. |

Collection and invite IDs and capability hashes are copied unchanged. Existing
view links keep their visibility and have no expiry, matching Python; invite
expiry, use limits, use counts and revocation are preserved, including expired
invites. Members and items retain their legacy row IDs in the audit because
the canonical tables use composite keys. Each item's reel resolves to its
original user's save, including a save retained after a deduplication conflict.
An editor's item is never reassigned to the collection owner. Missing users or
save references reject and audit the whole collection; no user is invented.

A collection becomes Go-owned as a whole when its owner, a member, an item
adder or save owner, an inviter, or an invite creator has been served or deleted
by Go. This includes canonical and previously audited participants. The audit
also records a fingerprint of the copied canonical collection and its children:
if Go-only participation leaves no current member row, later canonical changes
still protect the family from overwrite or resurrection. An existing canonical
collection ID with no copied baseline is protected too. The copy and final
delete transaction first take SHARE locks on the served-user and deletion-request
tables, then lock the collection and existing children before checking or writing.
New first-auth requests and deletion requests may wait until that transaction
commits. Run the production copy before admitting Go traffic. Concurrent child
inserts wait on the parent's foreign key lock; updates and deletes wait on the
child locks. A lock or deadlock error aborts the run and is not treated as a skip.

The final delta applies tracked collection deletions before save and user
deletions. Those cascades also leave protected collection families alone. Retained save
and user dependencies appear in `canonical_total`; expected/present and per-user
comparisons still count remaining legacy rows, so unrelated drift is checked. It
refreshes fingerprints after its own deletions so the next pass does not
mistake maintenance changes for Go changes. The verifier checks all four tables,
including composite-key presence and sampled sharing, role, ownership and
invite fields. It reports canonical drift until a backfill has explicitly
protected that collection. An unexplained row or a field mismatch still fails
verification.

A fresh dry run records planned references only for collection covers and
items whose reels pass the dry reel checks. It can therefore validate collection
references before saves exist without creating fake content or users. Normal
collection, membership and invite constraints are checked in the rolled-back
transaction; planned item inserts wait for the real run's save foreign keys.

The snapshot and verifier also restrict `processing_jobs` to rows created in
the last 60 days, measured from each command's current clock. When the legacy
table has an `environment` column, both apply
`environment IS DISTINCT FROM 'development'`, which keeps NULL values and
excludes development rows. These scope filters explain why the 2026-10-27
rehearsal counted 600 of the source's 632 job rows in scope. That is the source
scope, before deliberate skips, rather than the number of jobs carried.

Places and embeddings go only onto content versions this backfill built, never
onto one the Go pipeline built. Each version takes both from one carried reel,
the one it was built from, so a place's `ordinal` is its index in that reel's
`locations` array. That is what a hidden pin's `location_index` and a
`reel:{id}:{ordinal}` pin id name. The embedding keeps the reel's
`embedding_model`, `gemini-embedding-001` when the row has none, normalized as
the next section describes.

Places and embeddings belong to content, not to a user, so the users Go has
served do not narrow this pass. It only ever writes versions this backfill
built, which nothing in Go rewrites.

### Embedding model at cutover

The carried vectors are `gemini-embedding-001` at 768 dimensions. Python tags
every one `gemini-embedding-001@768` in `embedding_model` (all 2314 embedded
production reels). Go stores the bare model name: search compares a query only
with vectors stored under `EMBEDDING_MODEL`, and the embedder sends
`EMBEDDING_MODEL` to Gemini as the model name, so neither can carry a width
suffix.

The backfill drops a trailing `@<dimensions>` when it names 768, the width of
`content_embeddings.embedding`, so the carried vectors land under
`gemini-embedding-001`. A tag naming any other width leaves its vector behind:
the reel still carries, its vector does not, and the audit records it under
source table `reel_embeddings` as `skipped_embedding_width`. The run logs those
as `embeddings_skipped`, and `backfill-verify` reports them as `embeddings`
`skipped`, outside the `legacy` count.

The Go embedder defaults to `gemini-embedding-2`, so on the default every
carried reel is invisible to semantic search. At cutover, every Go host sets
`EMBEDDING_MODEL=gemini-embedding-001`. The carried vectors are then
searchable, and new saves are embedded with the same model. `backfill-verify`
logs `embedding_models`, the carried vectors counted per stored model name;
anything there other than the model the hosts are configured with is a search
hole.

The dense search gate has to be measured again on that model.
`internal/search` gates the vector arm at `MaxDenseDistance`, 0.40, which was
calibrated on `gemini-embedding-2` vectors (see `docs/cutover.md` and
`api/eval/REPORT.md`). Distances from `gemini-embedding-001` sit on their own
scale, so 0.40 may let unrelated reels through or hide related ones. After the
snapshot backfill, with `EMBEDDING_MODEL=gemini-embedding-001` on the host:

```sh
maintenance search-eval --user <uuid> --out report.json
maintenance search-eval --user <uuid> --max-distance 0.5   # one tuning point
```

`--user` is an evaluation user with the corpus seeded under
`gemini-embedding-001`, or a real user with `--set` labeled from their own
reels. Pick the widest gate at which every unrelated query still returns
nothing, as the report did for the current value, and set it on every API host
as `SEARCH_DENSE_MAX_DISTANCE`. Unset, the gate stays 0.40.

Moving to `gemini-embedding-2` is later maintenance: a `backfill-embeddings` run
that embeds every version under the new model, then the config change. Today
`backfill-embeddings` only picks versions with no embedding at all, so it needs
a way to re-embed versions stored under another model before that run is
possible.

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

### Owners

Python's `user_id` is text, and production holds test rows owned by names
rather than users: `default-user`, `test-user`, `reddit-e2e-test` and
`reddit-s-test` own nine reels. No canonical row can belong to one. A reel,
job or row of an owner-mapped table whose owner is not a UUID is audited
`skipped_invalid_owner`, and one whose owner is a UUID that `auth.users` does
not have is audited `skipped_unknown_reference`. Two tables differ. A hidden
pin with an unknown UUID owner finds no location to hide and is audited
`skipped_no_location`. `feedback` has no owner mapping: a row whose `user_id`
is not a UUID is carried with a NULL `user_id`. Both are deliberate skips, so
`backfill-verify` counts them as explained. The run logs them as
`invalid_owners` and `unknown_owners` for reels and `jobs_invalid_owners` and
`jobs_unknown_owners` for jobs; the mapped tables report them in
`skipped_by_action`.

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
has one, `embedding_models` splitting the second count by model, and `skipped`
for vectors left behind for their width) and
`map_points` (located places on carried reels against `content_locations`, and
manual pins on each side). Any of those short fails the run.

`users` keeps no audit. Its `in_scope` and `expected` are the legacy users,
leaving out those who deleted their account in Go; `present` and `carried` are
those with an `auth.users` row; `canonical_total` is every `auth.users` row. A
legacy user in scope with no `auth.users` row is `unexplained` and fails the
run.

Each per-user table also reports `migrated`: legacy rows owned by users Go has
served. Those rows are left out of every other count and out of the field
sample, because those users change their data in Go now.

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

### Every user lives on one backend

During the dual run, users on the old app are on Python and users on the
updated app are on Go. The first authenticated `/api/v2` request Go serves for a
user adds them to `reelpin.user_migrations`, once per user per API process.

From then on every delta leaves that user's rows alone, in every per-user table:
saves, jobs, entitlements, profiles, manual and hidden pins, reminders and price
tracks. Their Python rows are frozen, but the Python worker still touches some
of them: it sets `sent_at` when a legacy reminder fires and `last_checked_at`
when it checks a legacy price track. Carrying those would overwrite what the
user changed in Go. Users not in the table get the full upsert.

What this means for users, before the dual run starts:

- **Anything a user writes on the old app between the last delta's dump and
  their first Go request is lost.** A save, a reminder, a pin or a profile
  change made in that gap is never copied to Go. The nightly deltas keep the
  gap to at most a day, and keeping the dual run short keeps the number of
  users who fall into it small.
- **Pro granted by hand has to go into the Go database once a user has
  migrated.** A grant made in the Supabase SQL editor after that never reaches
  them, because every run skips their entitlement row.

The same filter applies to every run, not only a delta: a resumed snapshot or a
rerun skips these users too. Each row's write checks the table again, so a user
who moves while a run is under way is not written after their page was read.

### Each night while both apps run

1. Re-dump and replace `legacy` exactly as in section 1, which sets
   `DUMPED_AT`.
2. `maintenance backfill-legacy --execute --since <next_since> --dumped-at "$DUMPED_AT"`,
   then record the new `next_since` it logs.
3. `maintenance backfill-verify --allow-drift`. Rows the new app wrote, and
   legacy rows not copied yet, are expected drift here.

A delta neither reads nor moves the snapshot's cursors, and running the same one
twice changes nothing.

### The final delta, once

1. Stop the Python worker and API.
2. Re-dump and replace `legacy` as in section 1.
3. `maintenance backfill-legacy --since <next_since> --dumped-at "$DUMPED_AT" --propagate-deletes`,
   a dry run, and read `deleted`: the rows it would remove, per legacy table.
4. The same command with `--execute`.
5. `maintenance backfill-verify`, **without** `--allow-drift`. It must exit zero.
6. `psql "$DATABASE_URL" -c 'DROP SCHEMA legacy CASCADE'`, and remove the stage
   with `docker rm -f legacy-stage`.

`--propagate-deletes` is what carries deletions. For every user Go has not
served, a row this backfill carried whose legacy row is gone is deleted in Go,
and a hidden pin unhidden in Python is unhidden. Deleting a save takes its
collection items, reminders and price tracks with it, as it does in the app. It
never touches a user Go has served, and it does not delete feedback or link
clicks. A carried save stays while another legacy reel of the same user still
resolves to it, one that lost a save conflict to it or merged into it, because
that reel's reminders and price tracks point at the save. A later final delta
deletes it once no legacy reel does. Run it only here: while Python still serves, a user can move between the
dump and the delete.

What no delta carries: a row deleted in Python after the snapshot stays in Go
until the final delta, and a reminder copied while the Go worker runs is not in
the Redis due set until the worker re-seeds it on start.

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
