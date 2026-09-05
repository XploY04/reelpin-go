# internal/httpapi

The transport layer: routing, middleware, request parsing, response shapes.
Nothing here knows what a database is.

Read [`../../docs/api-contract.md`](../../docs/api-contract.md) before changing
anything a client can see. Flutter dev is updated after the backend contract is
correct and tested.

## Rules that differ here

- **`Routes()` is the only place a route is declared.** Public routes live under
  `/api/v1`; bare aliases and legacy compatibility endpoints are not registered.
- **The user id comes from the token, always.** `requestUserID(r)` reads it from
  the context the auth middleware populated. A `user_id` in a query string or
  body is never used for authorization.
- **Every response is JSON, including errors, 404 and 405.** `writeJSON` encodes
  into a buffer before writing the status, so an encoding failure becomes a 500
  rather than a 200 with a truncated body.
- **A driver error never reaches a body.** Log it with the request id, return
  the error code and a sentence a person can read.
- **Missing, forbidden and malformed all answer the same 404.** Distinguishing
  them tells a stranger which ids exist.
- **A list field is `[]`, never `null`.** The app decodes into a non-nullable
  list and a `null` is a crash on a phone.
- **Handlers do not build presentation.** They call a builder in `reels` or
  `jobs` and write the result. A handler that formats a date is doing the
  domain's job in the wrong package.

## Testing

Handler tests use the fakes in `fakes_test.go` and need no database. A test that
reaches for PostgreSQL to test a handler is testing the wrong thing; the SQL has
its own tagged tests in `internal/postgres`.

Assert on the status, the error code, and the field the change is about. Do not
assert on the whole body: it makes every future field addition a test failure
for no reason.
