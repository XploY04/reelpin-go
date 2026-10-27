# 2. A GET never writes

**Status:** accepted

## Context

The Python service repairs data on read. When `GET /processing-jobs/{id}` finds
a job that completed without a `result_reel_id`, it looks for a matching reel by
URL and writes the id back onto the job row.

It is well meant: it heals a job the app would otherwise poll forever. It also
means a read endpoint takes a write lock, that a burst of polling is a burst of
writes, and that the repair happens in whichever replica happened to serve the
request.

## Decision

Read endpoints in this service do not write. A completed job with no reel is
**presented** as failed, and the row is left exactly as it was. The
reel-matching lookup is not ported.

## Consequences

Every read path is provably safe to run anywhere, to retry, and to serve from a
replica. The reader interfaces have no write methods, so a write cannot appear
by accident.

The rows Python would have repaired stay unrepaired. That is the point: if those
rows need fixing, the fix belongs in the pipeline that created them or in a
maintenance command that runs deliberately, where it can be observed and counted.
A repair hidden inside a GET is a repair nobody knows is happening.

An operator looking at such a job sees the truth of what is stored, not a
version of it patched at display time.

## Exception: the dual-run migration marker (2026-10-20)

The bearer auth wrapper in `internal/httpapi/server.go` wraps GET routes too,
and the first time it verifies a user it writes one row to
`reelpin.user_migrations` through `internal/usermigration`. That is a write on a
read path, and it is deliberate. The legacy backfill has to know which users Go
has served, and a user's first request to Go is as likely to be a GET as
anything else.

It is idempotent and bounded. The statement is `INSERT ... ON CONFLICT DO
NOTHING`, so a repeat changes nothing; each API process writes a user at most
once and answers every later request from an in-memory set. It changes nothing
a read returns.

It exists for the dual-run window only. When Python is shut down, the marker
and the call in the auth wrapper are removed, and a GET goes back to writing
nothing at all.
