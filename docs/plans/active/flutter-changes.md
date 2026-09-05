# Flutter changes for the Go v2 API

A running record for the app developer of everything the Flutter app and both
native share extensions must change to move from the Python `/api/v1` service to
the Go `/api/v2` service. Updated in the same pull request as each backend
change, so it is never behind the contract.

The machine-readable source of truth is `api/openapi.yaml` (embedded and
published with a SHA-256 digest per release). This page is the narrative: what
changed, why, and what it means in the app. Task 30 of
`implementation-plan.md` is the migration task itself; nothing here requires an
app release before then, because Python v1 keeps serving installed versions
throughout coexistence.

## Base URL and versioning

- Dev: `https://api-dev.reelpin.in/api/v2/...` (once Task 22 deploys it).
- Production: `/api/v1` stays Python, `/api/v2` is Go. Updated clients call v2
  only; route selection is done by path, never by anything the client sets.
- **Bare aliases are gone.** The shipped app calls some endpoints without the
  `/api/v1` prefix (`/reels`, `/processing-jobs`, ...). v2 has no bare aliases;
  every call must use the full `/api/v2/<path>`.

## Error envelope (every endpoint)

v1 errors were flat and inconsistent. Every v2 error, including 404 and 405, is:

```json
{
  "error": {
    "code": "reel_not_found",
    "message": "The requested record was not found.",
    "request_id": "abc123...",
    "retryable": false,
    "details": {}
  }
}
```

- Match on `error.code`, never on `message` (message text may change).
- `success`, `error_code`, `detail` and top-level `allowed` no longer exist.
  The allowed-platform list now arrives as `error.details.allowed`.
- Show or log `request_id` in support flows; it is the correlation handle.
- `retryable` says whether repeating the same request may succeed.

## Pagination: opaque cursors replace offsets

`GET /api/v2/reels`:

- **Request**: `cursor` (opaque string from the previous page), `limit`
  (default 25, max 100 — v1 defaulted to 50). `offset` and `sort` are gone;
  there is one order, newest saved first.
- **Response**: `{"reels": [...], "next_cursor": "...", "has_more": bool,
  "limit": n}`. `total_count`, `offset` and `next_offset` are gone.
- Treat `next_cursor` as a black box. Do not parse it, store it across app
  launches, or synthesize one. Sending anything the API did not issue is a
  `422 validation_error`.
- The old client behaviour of passing an integer cursor/offset will be rejected,
  not silently accepted.

## Submission (Task 8 — endpoint shape is final now, returns 503 until built)

`POST /api/v2/processing-jobs/reels`, bearer auth:

- Body: `{"url": "...", "collection_ids": ["..."]}`.
- **New required header: `Idempotency-Key: <uuid>`.** Generate one UUID per
  submission attempt in the UI; reuse it for network retries of that attempt;
  new deliberate submission = new key. Reusing a key with a different body is a
  `409`.
- `200` = already saved, body is the reel itself (no job to poll).
- `202` = queued or already in flight, body is the processing job to poll.
- v1's single-status behaviour is gone; handle both.

## Native share extensions (Android + iOS) — biggest change

The two-call flow (resolve, then enqueue) is replaced by **one atomic call**,
because an extension can be killed between two calls:

- `POST /api/v2/native-shares/reels` with header `X-Share-Token: <token>` and
  body `{"raw_payload_text": "<exactly what the OS handed you>",
  "collection_ids": [...]}` plus `Idempotency-Key`.
- Responses are the same `200`-reel / `202`-job pair as above.
- A missing/invalid token is `401 share_token_required`.
- The extensions should persist the idempotency key with the pending payload so
  a retry after being killed cannot double-enqueue.

Share tokens are minted and revoked from the app process (bearer auth):

- `POST /api/v2/share-tokens` → `{"token": "...", "expires_at": "..."}`. The
  value is returned once; store it in the native-owned store as today.
- `DELETE /api/v2/share-tokens` revokes all of them (use on sign-out).

`POST /api/v2/share/resolve` (bearer) still exists, but only for interactive
in-app URL preview. The extensions must not call it any more.

## Endpoint-by-endpoint map

| v1 (Python) | v2 (Go) | Changes beyond the envelope |
|---|---|---|
| `GET /api/v1/health/live`, `/ready` | same paths under `/api/v2` | none |
| `GET /api/v1/health` | **removed** | poll `/ready` instead; it was a compat alias |
| `GET /api/v1/reels` (+ bare alias) | `GET /api/v2/reels` | cursor pagination, default limit 25, no `sort`, no `offset`, no `total_count` |
| `GET /api/v1/reels/{id}` | `GET /api/v2/reels/{reel_id}` | transcript still only here, never in lists |
| `GET /api/v1/reels/filters` | `GET /api/v2/reels/filters` | none |
| `GET /api/v1/reels/category-filters` | `GET /api/v2/reels/category-filters` | none |
| `GET /api/v1/processing-jobs` | `GET /api/v2/processing-jobs` | bounded list, no pagination; `active_only` default is still `false` |
| `GET /api/v1/processing-jobs/{id}` | `GET /api/v2/processing-jobs/{job_id}` | none |
| `POST /api/v1/processing-jobs/reels` | `POST /api/v2/processing-jobs/reels` | `Idempotency-Key` required; `200` vs `202` split |
| native share enqueue (v1 two-step) | `POST /api/v2/native-shares/reels` | one atomic call, `X-Share-Token` |
| `POST /api/v1/share-tokens` / `DELETE` | same paths under `/api/v2` | bearer only |
| `GET /api/v1/account/library-stats` | `GET /api/v2/account/library-stats` | none |
| `GET /api/v1/account/entitlements` | **not in v2 yet** | stays on v1 until a v2 decision; do not migrate this call |

## IDs and data

- Reel ids do not change. `user_saves.id` keeps every existing
  `public.reels.id`, so deep links, local caches and share cards keep working.
- Job `status` values are unchanged (`queued`, `processing`, `completed`,
  `failed`, `dead_lettered`); on terminal failures match `failure_code`, which
  is stable, not the message.
- `collection_ids` on a job is always an array, never null.

## Auth

Unchanged: the same Supabase project, the same access token in
`Authorization: Bearer`. Go verifies it locally, so nothing about token refresh
changes in the app. `user_id` in bodies or query strings is ignored — remove it
from requests when convenient, but sending it breaks nothing.

## Still to come (this file will grow)

- Collection endpoints (Task 15), map/Discover (Task 16), notifications
  (Task 17), account deletion (Task 18), search (Task 20): each will be recorded
  here when its contract lands.
- The dev base URL becomes real at Task 22.
- Task 30 is the app migration itself: vendor the released OpenAPI artifact,
  regenerate the Dart client, and point dev + extensions at
  `api-dev.reelpin.in`.
