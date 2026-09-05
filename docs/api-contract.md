# API contract

The Flutter app in production talks to the Python service. Every endpoint here
has to answer exactly like that one, because the app is already shipped and
cannot be changed to suit us. That constraint is the whole design.

## Two paths for every route

Each endpoint is registered twice: at `/api/v1/<path>` and at the bare `<path>`.
The shipped app calls some of each, so dropping either breaks a release that is
already on phones. `Server.routeTable()` is the single source both registrations
come from; adding a route in one place and not the other is not possible.

## What is served today

Public, no token:

| Method | Path | Behaviour |
|--------|------|-----------|
| GET | `/api/v1/health/live` | Never touches the database. Always 200. |
| GET | `/api/v1/health/ready` | 2s database ping. 503 and `degraded` when it fails. |
| GET | `/api/v1/health` | The readiness body, but always 200, for compatibility. |

Authenticated, `Authorization: Bearer <Supabase JWT>`:

| Method | Path |
|--------|------|
| GET | `/api/v1/reels` |
| GET | `/api/v1/reels/{reel_id}` |
| GET | `/api/v1/reels/filters` |
| GET | `/api/v1/reels/category-filters` |
| GET | `/api/v1/processing-jobs` |
| GET | `/api/v1/processing-jobs/{job_id}` |
| GET | `/api/v1/account/library-stats` |
| GET | `/api/v1/account/entitlements` |

Nothing writes. See
[`decisions/0002-reads-never-write.md`](decisions/0002-reads-never-write.md).

## Rules that are easy to break

**The user comes from the token, never from the request.** A `user_id` in a
query string or a body is ignored, not honoured and not rejected. Ignoring it
keeps a stale client working; honouring it would be an authorisation bug.

**Error codes are part of the contract.** The app matches on them:
`authentication_required`, `invalid_auth_token`, `validation_error`,
`invalid_platform`, `not_found`, `method_not_allowed`, `internal_error`, plus
the per-resource failures `reel_list_failed`, `processing_job_list_failed`,
`processing_job_lookup_failed`, `library_stats_failed`. Renaming one is a
breaking change even though the status code is unchanged.

**Every response is JSON, including 404 and 405.** A plain-text body under a
JSON content type is a bug the app cannot parse.

**A driver error never reaches a response body.** It can name the database, the
host, or credentials. Log it; return the code and a sentence a person can read.

**Missing, forbidden and malformed all answer 404.** A different answer for
"another user's reel" tells a stranger the id exists.

**Entitlements degrade rather than fail.** When stats cannot be loaded it
answers 200 with the restricted response, matching Python. A 500 there would
show the app a paywall it should not show.

**Absent is not empty.** A list is `[]`, never `null`: the app decodes into a
non-nullable list.

## Pagination and filters

`limit` defaults to 50 and is clamped to 1..100. `offset` defaults to 0. Out of
range is a `validation_error`, not a silent clamp, because silently returning
something other than what was asked for hides client bugs.

`platform` accepts the named platforms plus `other`; anything else is
`invalid_platform` with the allowed list in the response, so the client can show
something useful. `other` means "not one of the named platforms" and compiles to
a `NOT IN`, not an equality.

## Changing this

A change to any of the above is an API change and says so in its pull request
description: what changed, what the app sees, and whether a shipped release
still works. When a generated route manifest and golden response fixtures land
alongside CI, `make check` will fail on an undeclared change; until then this
page and the reviewer are the check.
