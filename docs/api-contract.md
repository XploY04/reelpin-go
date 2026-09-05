# API contract

The contract is Go-owned and machine-readable: **`api/openapi.yaml`** is the
source of truth, `api/spec.go` embeds it, and every merged commit publishes it
as a content-addressed artifact with its SHA-256 digest. Clients vendor a
released artifact and pin the digest; nothing generates a client from a sibling
checkout.

This page is the narrative around it: the rules that hold across every
operation, and why. For endpoint-by-endpoint detail, read the spec.

## Versioning

- This service owns `/api/v2`. `/api/v1` belongs to the Python service and is
  not described here; it keeps serving installed Flutter versions during
  coexistence and is retired separately.
- Additive v2 changes are fine. A breaking change to a released v2 operation
  requires `/api/v3`; CI runs a pinned `oasdiff` breaking-change check against
  the target branch's spec and fails on one.
- There are **no bare aliases** in v2. Every path is `/api/v2/<path>`.
- Operations that answer `503 processing_unavailable` are declared but not yet
  built. They are in the contract from the start so a client is generated once;
  each is replaced by its owning task.

## How the contract stays true

One route table, `Server.routeTable()` in `internal/httpapi/routes.go`, carries
method, path, `operationId` and an `AuthMode` per route. The contract tests walk
both directions: every registered route must appear in the spec once, every spec
operation must be registered once, ids must match, and each route's `AuthMode`
must match the operation's security scheme. `api/routes.json` is the generated
manifest (`go test ./internal/httpapi -update`), and `scripts/check_contract.py`
re-checks coverage without needing a Go toolchain.

Response fixtures in `api/fixtures/` are captured from the real handlers, not
written by hand, and compared on every run.

## Authentication modes

Every route declares one of four modes; there is no boolean:

| Mode | Credential | Used by |
|------|-----------|---------|
| `public` | none | health only |
| `bearer` | Supabase access token | the app and the web server |
| `share-token` | `X-Share-Token` | native share extensions, one endpoint |
| `public-share` | unguessable link token | read-only shared views (later task) |

The user is always the verified token's `sub`. A `user_id` in a query or body is
ignored. A share token authenticates exactly one endpoint and is never accepted
in place of a session.

## The error envelope

Every error, including 404 and 405, is:

```json
{"error": {"code": "...", "message": "...", "request_id": "...",
           "retryable": false, "details": {}}}
```

- `code` is stable and is what clients match on; `message` is for people and may
  change. Changing a code is a contract change with tests.
- `request_id` always matches the `X-Request-ID` response header.
- `details` carries only values the caller supplied or may choose from
  (`field`, `reason`, `allowed`). Driver and internal text never appears.
- Missing, forbidden and malformed ids all answer the same 404: a different
  answer tells a stranger which ids exist.

## Pagination

Opaque cursors only. The list order is saved time descending, then id
descending; the default page size is 25 and the maximum is 100. `next_cursor`
is null on the last page. A cursor is base64 of a private shape: a client that
sends anything this API did not issue gets `422 validation_error`, never a
guessed position. There is no `total_count`: it is a second query whose answer
is stale before it is read.

## Writes

`POST /api/v2/processing-jobs/reels` and `POST /api/v2/native-shares/reels`
require an `Idempotency-Key` header (a client-generated UUID per attempt).
Retrying with the same key returns the same outcome; the same key with a
different body is a `409`. `200` means already saved and carries the reel;
`202` means work is queued or in flight and carries the job to poll.

The native-share endpoint resolves and enqueues in one request, because a share
extension can be killed between two calls.

## Transcripts

A transcript appears only in `GET /api/v2/reels/{reel_id}`, never in a list.
Adding it to the list is a silent payload and cost regression.

## Changing this

Edit `api/openapi.yaml` and the route table together, regenerate
(`go test ./internal/httpapi -update`), and record what a client sees in the
pull request and in `docs/plans/active/flutter-changes.md`. `make check` fails
on any drift between the three.
