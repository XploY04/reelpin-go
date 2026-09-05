# 6. Migrate production data additively

**Status:** accepted

## Context

Production already contains Supabase users, saved reels, jobs, collections,
locations and notifications. A clean backend does not justify making users lose
their library or receive new identities.

## Decision

Keep the current Supabase project as production. Preserve every Auth user ID,
`public.reels.id` and `public.reels.user_id`. Add global content tables and
nullable links, backfill in bounded batches, verify the result, then switch
reads and writes separately.

The public API and Flutter keep calling these records reels. The database uses
`contents` and `user_saves`; the legacy table name does not define the new
domain model.

## Consequences

An existing user can sign into ReelPin web and see old content before the write
pipeline moves to Go. Rollback can route reads to Python because existing rows
remain valid.

The old and new schemas coexist during migration. Cleanup is delayed until Go
owns traffic, Python queues are empty, reconciliation passes and the observation
window closes.
