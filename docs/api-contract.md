# API contract

The Go backend defines the contract. Flutter dev is adjusted after an endpoint
is correct and tested. Python responses are useful evidence, not a compatibility
requirement.

## Versioning

Public routes live under `/api/v1`. There are no bare aliases. Additive changes
stay in v1; a breaking change after the client is released requires v2 or a
coordinated client release.

## What is served today

Public, no token:

| Method | Path | Behaviour |
|--------|------|-----------|
| GET | `/api/v1/health/live` | Never touches the database. Always 200. |
| GET | `/api/v1/health/ready` | 2s database ping. Returns 503 when it fails. |

Authenticated with `Authorization: Bearer <Supabase JWT>`:

| Method | Path |
|--------|------|
| GET | `/api/v1/reels` |
| GET | `/api/v1/reels/{reel_id}` |
| GET | `/api/v1/reels/filters` |
| GET | `/api/v1/reels/category-filters` |
| GET | `/api/v1/processing-jobs` |
| GET | `/api/v1/processing-jobs/{job_id}` |
| GET | `/api/v1/account/library-stats` |

There is no subscription or entitlement endpoint. Nothing writes yet. See
[`decisions/0002-reads-never-write.md`](decisions/0002-reads-never-write.md).

## Rules

**The user comes from the token, never from the request.** A `user_id` in a
query string or body is never used for authorization.

**Error codes are contracts.** Current shared codes are
`authentication_required`, `invalid_auth_token`, `validation_error`,
`invalid_platform`, `not_found`, `method_not_allowed`, and `internal_error`.
Resource failures use specific codes such as `reel_list_failed` and
`processing_job_lookup_failed`.

**Every response is JSON, including errors, 404 and 405.** A driver error is
logged and never returned in a response body.

**Missing, forbidden and malformed identifiers all answer 404.** Different
answers would reveal which identifiers exist.

**A list field is `[]`, never `null`.** This keeps decoding predictable in Go
and Flutter.

## Pagination and filters

`limit` defaults to 50 and must be between 1 and 100. `offset` defaults to 0.
Invalid values return `validation_error`.

`platform` accepts the named platforms plus `other`. An unknown value returns
`invalid_platform`. `other` means every stored platform outside the named set.

## Changing this

An API change updates this page, handler tests, contract fixtures when present,
and the Flutter dev client when it consumes the route. The pull request explains
what changed and why.
