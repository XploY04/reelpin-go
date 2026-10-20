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
different body is a `409`. That is the attempt. The content is checked too:
before anything is created, the user's existing save or job still in flight
for the same normalized URL is looked up, so a second attempt at the same link
never makes a second job. `200` is always the reel; `202` is always a job to
poll, either new work or that job in flight. Every success body has
`already_saved`, true when the user had the link before this submission.

The native-share endpoint resolves and enqueues in one request, because a share
extension can be killed between two calls.

Submission bodies are strict: an unknown field is a `422`, so a client cannot
smuggle a `user_id` past the token. Provider-costing endpoints fail closed —
without a working rate limiter they answer `503 processing_unavailable` rather
than spending unmetered — while reads never consult the limiter at all.

## Transcripts and views

A transcript appears in `GET /api/v2/reels/{reel_id}` and in the items of a
collection page (`GET /api/v2/collections/{collection_id}` and
`GET /api/v2/shared-collections/{token}`), never in the reel list or search.
Adding it to those is a silent payload and cost regression.

Collection items are the exception because a collaborator or a link viewer
cannot call `GET /reels/{reel_id}` for a save that is not theirs: that read is
scoped to the save's owner. So each item carries the whole reel, the display
fields, `transcript`, `locations` and the long-form extraction, plus
`added_at` and `added_by`.

`view=concise` on the reel list, search and both collection pages drops the
long-form fields (`key_facts`, `people_mentioned`, `actionable_items`,
`events`, and on a collection page `transcript`, which a concise page never
reads from the database). `detailed` is the default, so a client that never
sends it is unaffected. The projection lives in `reels.Display`, not in a
handler, so the MCP server applies the same one.

## One reel, one shape

`GET /api/v2/reels/{reel_id}` carries every record field and the same display
fields a list item does (`category_label`, `sub_category_label`,
`relative_date`, `display_date`, `saved_date_key`, `content_type`,
`has_map_locations`, `mappable_locations`, `map_location_count`,
`primary_location_label`), built by the same `reels.BuildDisplayReel`.

A collection item is that same object. Both collection pages read each save
from the table `GET /reels/{reel_id}` reads, with the same columns and scan,
and build it with `reels.BuildReel`, so a detailed item equals the save's
`getReel` body apart from `reel_id`, `saved_at`, `added_at` and `added_by`.
`internal/postgres` tests this across all three endpoints.

The one difference is `user_id`, the save owner's account id. The signed-in
collection page sends it; `GET /api/v2/shared-collections/{token}` leaves it
out, because its reader is anonymous and the link grants a view of the saves,
not of whose they are.

Filing works only on your own saves. `POST /api/v2/collections` and
`POST /api/v2/collections/{collection_id}/items` check that every reel id
belongs to the caller; an unknown or foreign id answers `404 reel_not_found`,
the same answer `getReel` gives, and the request files nothing. Being an
editor of a collection grants no right to another member's saves.

The subcategory is `sub_category` (and `sub_category_label`) in every
reel-bearing response. The single-reel shape and map pins first sent it as
`subcategory`; they keep that name beside `sub_category`, marked deprecated,
for one release, because removing it is a breaking change to a released v2
operation. Query and body filters still take `subcategory`.

## Public URLs

Every reel and collection carries `web_url`, built on `PUBLIC_WEB_BASE_URL`:
`/r/{reel_id}` and `/collections/{collection_id}`. `/c/` is not used: on
`reelpin.in`, `link.reelpin.in` and the app's universal links it is the
share-token route.

## Rate limits per client

A Supabase OAuth-server token carries a `client_id`. The submission, search
and map-pin limits count such a token against its own bucket under the user
and against the user's total, and both must allow it, so a second client
cannot add to a user's quota. An app session, which has no client, counts
against the user's total alone.

## Changing this

Edit `api/openapi.yaml` and the route table together, regenerate
(`go test ./internal/httpapi -update`), and record what a client sees in the
pull request and in `docs/plans/active/flutter-changes.md`. `make check` fails
on any drift between the three.
